package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// The merge queue (phase F). Per repo and target branch, the first change
// in line is merged onto the target's current tip by a runner, the result
// is pushed to forge-queue/<target> and tested by a Forge pipeline, and the
// change is merged only if that passes. The gate's "Merge queue" rule
// passes only then, so the host's own Merge button waits for the queue too.
// One change is tested at a time: simple, and two changes that each pass
// can never break the target together.

// QueueBranchPrefix names the branches the queue pushes its test merges to.
// Pushes to them never trigger pipelines from webhooks: the queue starts
// its own.
const QueueBranchPrefix = "forge-queue/"

// QueueBranch is target's queue branch.
func QueueBranch(target string) string { return QueueBranchPrefix + target }

// PipelineStarter starts a Forge pipeline for repo at sha on ref and returns
// its id (the API server provides it; the queue runs pipelines like a push).
type PipelineStarter func(ctx context.Context, repo, ref, sha, actor string) (int64, error)

// SetPipelineStarter lets the queue start test pipelines.
func (s *Service) SetPipelineStarter(fn PipelineStarter) { s.startPipeline = fn }

// AdvanceQueues moves the first change of every queue one step. Returns how
// many queues moved.
func (s *Service) AdvanceQueues(ctx context.Context) int {
	heads, err := s.store.QueueHeads(ctx)
	if err != nil {
		slog.Error("merge queue: list", "err", err)
		return 0
	}
	moved := 0
	for _, h := range heads {
		changed := false
		ran, err := s.store.WithQueueLock(ctx, h.Repo, h.TargetBranch, func() error {
			e, err := s.store.ActiveQueueEntry(ctx, h.ChangeID)
			if err != nil || e == nil || e.Position != 1 {
				return err // finished or overtaken meanwhile
			}
			changed, err = s.advance(ctx, e)
			return err
		})
		if err != nil {
			slog.Error("merge queue: advance", "repo", h.Repo, "target", h.TargetBranch, "err", err)
		}
		if ran && changed {
			moved++
		}
	}
	return moved
}

// advance moves e one step and reports whether it changed.
func (s *Service) advance(ctx context.Context, e *store.QueueEntry) (bool, error) {
	save := func(state, msg string) (bool, error) {
		e.State, e.Error = state, msg
		if state == "failed" {
			slog.Info("merge queue: change left the queue", "change", e.ChangeID, "reason", msg)
		}
		return true, s.store.UpdateQueueEntry(ctx, *e)
	}
	c, err := s.store.ChangeForAction(ctx, e.ChangeID)
	if err != nil {
		return false, err
	}
	switch {
	case c.State == "merged" && e.State != "merging":
		return save("merged", "merged outside the queue")
	case c.State != "open" && e.State != "merging":
		return save("failed", "the change was "+c.State)
	case c.HeadSHA != e.HeadSHA && e.State != "merging":
		return save("failed", "the change has new commits — add it to the queue again")
	}
	policies, err := s.store.ListMergePolicies(ctx)
	if err != nil {
		return false, err
	}
	if p := store.MatchMergePolicy(policies, c.Repo, c.TargetBranch); p == nil || !p.MergeQueue {
		return save("failed", "the merge queue is off for "+c.TargetBranch)
	}
	switch e.State {
	case "queued":
		return s.startTest(ctx, e, c, save)
	case "testing":
		if e.TestSHA == "" {
			return s.testBuilt(ctx, e, save)
		}
		return s.testRan(ctx, e, save)
	case "merging":
		return s.queueMerge(ctx, e, c, save)
	}
	return false, nil
}

type saveFn func(state, msg string) (bool, error)

// startTest checks the change may merge apart from the queue, then asks a
// runner to build the test merge.
func (s *Service) startTest(ctx context.Context, e *store.QueueEntry, c *store.ReviewTargetChange, save saveFn) (bool, error) {
	g, err := s.store.GetGate(ctx, c.ID)
	if err != nil || g == nil || g.HeadSHA != c.HeadSHA {
		return false, err // not evaluated for this head yet
	}
	for _, r := range g.Rules {
		switch {
		case r.ID == "merge_queue" || r.State == "pass":
		case r.ID == "freeze":
			return false, nil // wait for the window to open
		default:
			return save("failed", "merge rule not met: "+r.Detail)
		}
	}
	d, err := s.store.GetChangeDetail(ctx, c.ID)
	if err != nil {
		return false, err
	}
	refs, _ := vcs.MirrorRefsFor(c.Provider, c.Number, d.SourceBranch, d.TargetBranch)
	task := proto.MergeTask{Target: c.TargetBranch, Heads: []string{e.HeadSHA}, Method: "merge",
		Message: fmt.Sprintf("Merge queue: test change #%s onto %s\n\n%s", c.Number, c.TargetBranch, d.Title),
		PushRef: "refs/heads/" + QueueBranch(c.TargetBranch), Force: true, FetchRefs: refs}
	task.AuthorName, task.AuthorEmail = person(e.RequestedBy)
	if _, err := s.store.EnqueueRunnerTask(ctx, proto.TaskKindMerge, c.Repo, buildKey(e), task); err != nil {
		return false, err
	}
	return save("testing", "")
}

func buildKey(e *store.QueueEntry) string { return fmt.Sprintf("queue:%d:build", e.ID) }

// testBuilt starts the test pipeline once the runner pushed the test merge.
func (s *Service) testBuilt(ctx context.Context, e *store.QueueEntry, save saveFn) (bool, error) {
	t, err := s.store.RunnerTaskStatus(ctx, buildKey(e))
	if err != nil {
		return false, err
	}
	switch t.State {
	case "pending", "running":
		if !t.Started && time.Since(t.QueuedAt) > taskWait {
			return save("failed", "no runner took the test merge (runners need git 2.38+ and runner tasks on)")
		}
		return false, nil
	case "failed":
		return save("failed", "building the test merge failed: "+t.Error)
	}
	var res proto.MergeResult
	if err := json.Unmarshal(t.Result, &res); err != nil {
		return save("failed", "the runner reported no test merge")
	}
	if why := refusal(res); why != nil {
		return save("failed", "cannot merge onto "+e.TargetBranch+": "+strings.TrimPrefix(why.Error(), vcs.ErrNotMergeable.Error()+": "))
	}
	if s.startPipeline == nil {
		return save("failed", "this server cannot start pipelines")
	}
	e.BaseSHA, e.TestSHA = res.Base, res.SHA
	id, err := s.startPipeline(ctx, e.Repo, QueueBranch(e.TargetBranch), res.SHA, e.RequestedBy)
	if err != nil {
		return save("failed", "could not start the test pipeline: "+err.Error())
	}
	e.PipelineID = &id
	return save("testing", "")
}

// testRan moves on when the test pipeline finished.
func (s *Service) testRan(ctx context.Context, e *store.QueueEntry, save saveFn) (bool, error) {
	if e.PipelineID == nil {
		return save("failed", "the test pipeline is missing")
	}
	st, err := s.store.PipelineStatus(ctx, *e.PipelineID)
	if err != nil {
		return false, err
	}
	switch st {
	case "success":
		return save("merging", "")
	case "failed", "canceled":
		return save("failed", fmt.Sprintf("the test pipeline #%d %s", *e.PipelineID, st))
	case "":
		return save("failed", "the test pipeline was deleted")
	}
	return false, nil
}

// queueMerge sends the tested change to the outbox once its gate (now with
// the queue rule passing) passes, and follows it.
func (s *Service) queueMerge(ctx context.Context, e *store.QueueEntry, c *store.ReviewTargetChange, save saveFn) (bool, error) {
	if e.OutboxID == nil {
		if c.State == "merged" {
			return save("merged", "")
		}
		g, err := s.store.GetGate(ctx, c.ID)
		if err != nil || g == nil || g.HeadSHA != c.HeadSHA || g.State == "pending" {
			return false, err // the gate has not caught up yet
		}
		if g.State == "fail" {
			return save("failed", "merge rule not met: "+g.Summary)
		}
		payload, _ := json.Marshal(MergePayload{Method: e.Method, ExpectedHead: e.HeadSHA, ExpectedBase: e.BaseSHA})
		id := c.ID
		o, err := s.store.EnqueueOutbox(ctx, store.OutboxItem{IdemKey: fmt.Sprintf("queue-merge:%d", e.ID), Kind: KindMerge,
			Repo: c.Repo, ChangeID: &id, Class: vcs.ClassUser, Payload: payload, RequestedBy: e.RequestedBy}, "")
		if err != nil {
			return false, err
		}
		e.OutboxID = &o.ID
		return save("merging", "")
	}
	o, err := s.store.OutboxItemByID(ctx, *e.OutboxID)
	if err != nil {
		return false, err
	}
	switch o.State {
	case "done":
		return save("merged", "")
	case "failed":
		return save("failed", "the merge failed: "+o.LastError)
	}
	return false, nil
}

// queueRule is the gate's "Merge queue" rule: pending until the queue has
// tested the change on top of the target.
func queueRule(q *store.QueueEntry, target string) (string, string) {
	switch {
	case q == nil:
		return "pending", "Add the change to the merge queue to merge it into " + target
	case q.State == "queued" && q.Position > 1:
		return "pending", fmt.Sprintf("In the merge queue, position %d", q.Position)
	case q.State == "queued":
		return "pending", "Next in the merge queue"
	case q.State == "testing" && q.PipelineID != nil:
		return "pending", fmt.Sprintf("Testing on top of %s (pipeline #%d)", target, *q.PipelineID)
	case q.State == "testing":
		return "pending", "Merging onto " + target + " to test"
	}
	return "pass", "Tested on top of " + target
}

// ErrQueueOff: the change's branch does not merge through a queue.
var ErrQueueOff = errors.New("the merge queue is off for this branch")
