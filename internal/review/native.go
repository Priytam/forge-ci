package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Forge-native merges (phase F). A plain git server has no merge API, so a
// runner merges in the repo mirror it already keeps and pushes the result
// with a lease. The outbox item stays the single record of the merge: it
// queues the runner task once, then follows it.

// taskWait bounds how long a merge waits for a runner to pick it up.
var taskWait = 10 * time.Minute

// taskPoll is how often a waiting merge looks at its task again.
var taskPoll = 2 * time.Second

func (s *Service) nativeMerge(ctx context.Context, o store.OutboxItem) {
	key := fmt.Sprintf("merge:outbox:%d", o.ID)
	t, err := s.store.RunnerTaskStatus(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		// Not sent yet: check everything now, then hand it to a runner.
		c, p, ok := s.mergeReady(ctx, o)
		if !ok {
			return
		}
		d, err := s.store.GetChangeDetail(ctx, c.ID)
		if err != nil {
			s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
			return
		}
		task := proto.MergeTask{Target: c.TargetBranch, ExpectedTarget: p.ExpectedBase, Source: d.SourceBranch,
			ExpectedSource: p.ExpectedHead, Heads: []string{p.ExpectedHead}, Method: p.Method,
			Message: mergeMessage(p.Method, d), PushRef: "refs/heads/" + c.TargetBranch}
		task.AuthorName, task.AuthorEmail = person(o.RequestedBy)
		t, err = s.store.EnqueueRunnerTask(ctx, proto.TaskKindMerge, c.Repo, key, task)
	}
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
		return
	}
	switch t.State {
	case "pending", "running":
		if !t.Started && time.Since(t.QueuedAt) > taskWait {
			s.finishWrite(ctx, o, "failed", nil, errors.New("no runner took the merge (runners need git 2.38+ and runner tasks on)"), time.Time{}, true)
			return
		}
		s.finishWrite(ctx, o, "pending", nil, nil, time.Now().Add(taskPoll), false)
	case "failed":
		s.finishWrite(ctx, o, "failed", nil, errors.New("the runner could not merge: "+t.Error), time.Time{}, true)
	case "done":
		var res proto.MergeResult
		if err := json.Unmarshal(t.Result, &res); err != nil || (res.SHA == "" && !refused(res)) {
			s.finishWrite(ctx, o, "failed", nil, errors.New("the runner reported no merge"), time.Time{}, true)
			return
		}
		if why := refusal(res); why != nil {
			s.finishWrite(ctx, o, "failed", nil, why, time.Time{}, true)
			s.refreshNative(ctx, o)
			return
		}
		if err := s.store.MarkNativeMerged(ctx, *o.ChangeID, res.SHA); err != nil {
			s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), false)
			return
		}
		s.finishWrite(ctx, o, "done", map[string]string{"merge_sha": res.SHA}, nil, time.Time{}, true)
	}
}

func refused(r proto.MergeResult) bool { return len(r.Conflicts) > 0 || r.Moved != "" || r.NotFF }

// refusal explains a merge the runner did not make (nil when it did).
func refusal(r proto.MergeResult) error {
	switch {
	case len(r.Conflicts) > 0:
		return fmt.Errorf("%w: conflicts in %s", vcs.ErrNotMergeable, strings.Join(r.Conflicts, ", "))
	case r.NotFF:
		return fmt.Errorf("%w: the target has moved on, so it cannot be fast-forwarded", vcs.ErrNotMergeable)
	case r.Moved != "":
		return fmt.Errorf("%w: %s", vcs.ErrHeadMoved, r.Moved)
	}
	return nil
}

func (s *Service) refreshNative(ctx context.Context, o store.OutboxItem) {
	if o.ChangeID == nil {
		return
	}
	if c, err := s.store.ChangeForAction(ctx, *o.ChangeID); err == nil {
		_ = s.store.RequestChangeRefresh(ctx, c.Repo, c.Provider, c.Number, store.PriorityWebhook, 0, false)
	}
}

// mergeMessage is the commit message of a native merge.
func mergeMessage(method string, d *store.ChangeDetail) string {
	if method == "squash" {
		msg := fmt.Sprintf("%s (#%s)", d.Title, d.Number)
		if strings.TrimSpace(d.Body) != "" {
			msg += "\n\n" + strings.TrimSpace(d.Body)
		}
		return msg
	}
	return fmt.Sprintf("Merge change #%s from %s\n\n%s", d.Number, d.SourceBranch, d.Title)
}

// person turns a Forge identity (an email) into a commit author.
func person(email string) (string, string) {
	if email == "" || !strings.Contains(email, "@") {
		return "Forge", "forge@localhost"
	}
	name, _, _ := strings.Cut(email, "@")
	return name, email
}
