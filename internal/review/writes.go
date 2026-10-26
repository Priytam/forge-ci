package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Gate evaluation and the provider-write outbox (phase B). The gate is
// re-evaluated only when one of its inputs moved; a changed verdict is
// queued for publishing as forge/merge-gate; the outbox worker sends queued
// writes under the credential's budget, user actions first.

const (
	gateBatch       = 50
	outboxBatch     = 8
	outboxLease     = time.Minute
	maxWriteTries   = 6
	gatePublishWait = 10 * time.Minute // a merge waits this long for its gate to reach the host
)

// Outbox kinds.
const (
	KindPublishGate = "publish_gate"
	KindMerge       = "merge"
)

// gatePayload is a queued forge/merge-gate publish.
type gatePayload struct {
	SHA         string `json:"sha"`
	GateState   string `json:"gate_state"` // pass | pending | fail
	State       string `json:"state"`      // success | pending | failure (the host's word)
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// MergePayload is a queued merge.
type MergePayload struct {
	Method       string `json:"method"`
	ExpectedHead string `json:"expected_head"`
}

// ChangeURL is the change page in Forge (the gate's link on the host).
func ChangeURL(id int64) string {
	base := os.Getenv("FRONTEND_URL")
	if base == "" {
		base = os.Getenv("EXTERNAL_URL")
	}
	if base == "" {
		base = "http://localhost:5173"
	}
	return strings.TrimSuffix(base, "/") + "/changes/" + strconv.FormatInt(id, 10)
}

func hostState(gate string) string {
	switch gate {
	case "pass":
		return "success"
	case "fail":
		return "failure"
	}
	return "pending"
}

// EvaluateGates re-evaluates the gates whose inputs moved. Returns how many
// verdicts changed.
func (s *Service) EvaluateGates(ctx context.Context) int {
	ids, claimedAt, err := s.store.ClaimDirtyGates(ctx, gateBatch)
	if err != nil || len(ids) == 0 {
		if err != nil {
			slog.Error("code review: claim gates", "err", err)
		}
		return 0
	}
	policies, err := s.store.ListMergePolicies(ctx)
	if err != nil {
		slog.Error("code review: load merge rules", "err", err)
		return 0
	}
	changed := 0
	for _, id := range ids {
		ok, err := s.evaluateGate(ctx, id, policies, claimedAt)
		if err != nil {
			slog.Error("code review: evaluate gate", "change", id, "err", err)
			continue
		}
		if ok {
			changed++
		}
	}
	return changed
}

func (s *Service) evaluateGate(ctx context.Context, id int64, policies []store.MergePolicy, claimedAt time.Time) (bool, error) {
	in, err := s.store.LoadGateInputs(ctx, id)
	if err != nil {
		return false, err
	}
	c := in.Change
	p := store.MatchMergePolicy(policies, c.Repo, c.TargetBranch)
	var v Verdict
	var policyID *int64
	if p == nil {
		had, err := s.store.HasGate(ctx, id)
		if err != nil {
			return false, err
		}
		if !had {
			_, err := s.store.ClearGate(ctx, id, claimedAt)
			return false, err
		}
		// A rule used to apply: say so on the host rather than leave the
		// change blocked by a gate nobody can satisfy any more.
		v = Verdict{State: "pass", Summary: "No Forge merge rules apply to " + c.TargetBranch, Rules: []store.GateRule{}}
	} else {
		policyID = &p.ID
		emails := []string{}
		for _, r := range in.Reviews {
			if r.Source == "forge" {
				emails = append(emails, r.Reviewer)
			} else if e := in.Identities[strings.ToLower(r.Reviewer)]; e != "" {
				emails = append(emails, e)
			}
		}
		elig, group, bootstrap, err := s.store.ApproverEligibility(ctx, c.Repo, emails, p.ApproverRoles, p.RequiredGroup)
		if err != nil {
			return false, err
		}
		v = Evaluate(in, p, Eligibility{Eligible: elig, Group: group, Bootstrap: bootstrap}, time.Now())
	}
	changed, err := s.store.SaveGateResult(ctx, store.GateResult{
		ChangeID: id, HeadSHA: c.HeadSHA, PolicyID: policyID, State: v.State, Summary: v.Summary, Rules: v.Rules,
	}, claimedAt, v.RecheckAt)
	if err != nil || !changed {
		return changed, err
	}
	if c.HeadSHA != "" && vcs.CapabilitiesOf(c.Provider).StatusChecks {
		if err := s.queueGatePublish(ctx, c, v); err != nil {
			return true, err
		}
	}
	return true, nil
}

// queueGatePublish queues forge/merge-gate for the change's head. A newer
// verdict supersedes an unsent older one, so a burst publishes once.
func (s *Service) queueGatePublish(ctx context.Context, c store.ReviewTargetChange, v Verdict) error {
	p := gatePayload{SHA: c.HeadSHA, GateState: v.State, State: hostState(v.State),
		Description: v.Summary, TargetURL: ChangeURL(c.ID)}
	body, _ := json.Marshal(p)
	sum := sha256.Sum256([]byte(p.State + "|" + p.Description))
	id := c.ID
	_, err := s.store.EnqueueOutbox(ctx, store.OutboxItem{
		IdemKey: fmt.Sprintf("gate:%d:%s:%s", c.ID, c.HeadSHA, hex.EncodeToString(sum[:8])),
		Kind:    KindPublishGate, Repo: c.Repo, ChangeID: &id, Class: vcs.ClassWebhook, Payload: body,
	}, KindPublishGate)
	return err
}

// SendOutbox sends due provider writes concurrently. Returns how many were
// claimed.
func (s *Service) SendOutbox(ctx context.Context) int {
	items, err := s.store.ClaimOutbox(ctx, outboxBatch, outboxLease)
	if err != nil {
		slog.Error("code review: claim outbox", "err", err)
		return 0
	}
	var wg sync.WaitGroup
	for _, o := range items {
		wg.Add(1)
		go func(o store.OutboxItem) {
			defer wg.Done()
			s.send(ctx, o)
		}(o)
	}
	wg.Wait()
	return len(items)
}

func (s *Service) finishWrite(ctx context.Context, o store.OutboxItem, state string, result any, failure error, retryAt time.Time, count bool) {
	if failure != nil && state == "failed" {
		slog.Warn("code review: provider write failed", "kind", o.Kind, "repo", o.Repo, "err", failure)
	}
	if err := s.store.FinishOutbox(ctx, o, state, result, failure, retryAt, count); err != nil {
		slog.Error("code review: finish outbox", "id", o.ID, "err", err)
	}
}

// retryOrFail retries a transient failure with backoff and gives up after
// maxWriteTries; permanent failures stop at once. It reports whether the
// write gave up for good.
func (s *Service) retryOrFail(ctx context.Context, o store.OutboxItem, err error, cred, provider string) bool {
	var rl *vcs.RateLimitedError
	switch {
	case errors.As(err, &rl):
		until := time.Now().Add(rl.RetryAfter)
		_ = s.store.ExhaustProviderBudget(ctx, cred, provider, until)
		s.finishWrite(ctx, o, "pending", nil, err, until, false)
	case vcs.Permanent(err) || o.Attempts+1 >= maxWriteTries:
		s.finishWrite(ctx, o, "failed", nil, err, time.Time{}, true)
		return true
	default:
		d := time.Duration(math.Min(float64(10*time.Minute), float64(5*time.Second)*math.Pow(2, float64(o.Attempts))))
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(d), true)
	}
	return false
}

func (s *Service) send(ctx context.Context, o store.OutboxItem) {
	switch o.Kind {
	case KindPostComment, KindResolveThread, KindPostReview:
		s.sendThreadWrite(ctx, o)
		return
	}
	ref, cred, found, err := s.store.ReviewTarget(ctx, o.Repo)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(30*time.Second), true)
		return
	}
	w := vcs.ChangeWriterFor(ref.Provider)
	if !found || w == nil {
		s.finishWrite(ctx, o, "failed", nil, errors.New("repo is not connected to a host Forge can write to"), time.Time{}, true)
		return
	}
	ok, wait, err := s.store.AcquireProviderBudget(ctx, cred, ref.Provider, o.Class)
	if err != nil || !ok {
		s.finishWrite(ctx, o, "pending", nil, errBudget, time.Now().Add(max(wait, time.Second)), false)
		return
	}
	cctx := s.callCtx(ctx, o.Class, cred)
	switch o.Kind {
	case KindPublishGate:
		var p gatePayload
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			s.finishWrite(ctx, o, "failed", nil, err, time.Time{}, true)
			return
		}
		number := ""
		if o.ChangeID != nil {
			if c, err := s.store.ChangeForAction(ctx, *o.ChangeID); err == nil {
				number = c.Number
			}
		}
		err := w.PublishCheck(cctx, s.client, ref, p.SHA,
			vcs.CheckResult{State: p.State, Description: p.Description, TargetURL: p.TargetURL, ChangeID: number})
		if err != nil {
			s.retryOrFail(ctx, o, err, cred, ref.Provider)
			return
		}
		s.finishWrite(ctx, o, "done", nil, nil, time.Time{}, true)
		if o.ChangeID != nil {
			if err := s.store.MarkGatePublished(ctx, *o.ChangeID, p.SHA, p.GateState); err != nil {
				slog.Error("code review: mark gate published", "err", err)
			}
		}
	case KindMerge:
		s.merge(ctx, cctx, o, ref, cred, w)
	default:
		s.finishWrite(ctx, o, "failed", nil, fmt.Errorf("unknown write %q", o.Kind), time.Time{}, true)
	}
}

// merge sends a queued merge, re-checking everything at send time: the change
// is still open on the approved head, its gate still passes and has reached
// the host. A retry first re-reads the change, so a merge that succeeded
// before a timeout is never sent twice.
func (s *Service) merge(ctx, cctx context.Context, o store.OutboxItem, ref vcs.RepoRef, cred string, w vcs.ChangeWriter) {
	var p MergePayload
	if err := json.Unmarshal(o.Payload, &p); err != nil || o.ChangeID == nil {
		s.finishWrite(ctx, o, "failed", nil, errors.New("malformed merge request"), time.Time{}, true)
		return
	}
	c, err := s.store.ChangeForAction(ctx, *o.ChangeID)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
		return
	}
	if c.State == "merged" {
		s.finishWrite(ctx, o, "done", map[string]string{"note": "already merged"}, nil, time.Time{}, true)
		return
	}
	if c.State != "open" || c.HeadSHA != p.ExpectedHead {
		s.finishWrite(ctx, o, "failed", nil, vcs.ErrHeadMoved, time.Time{}, true)
		return
	}
	// Re-evaluate now rather than trust a verdict whose inputs may have just
	// moved (a review a second ago). The zero claim time leaves any dirty
	// mark for the regular tick.
	if policies, err := s.store.ListMergePolicies(ctx); err == nil {
		if _, err := s.evaluateGate(ctx, c.ID, policies, time.Time{}); err != nil {
			slog.Warn("code review: evaluate gate before merge", "change", c.ID, "err", err)
		}
	}
	gate, err := s.store.GetGate(ctx, c.ID)
	if err != nil {
		s.finishWrite(ctx, o, "pending", nil, err, time.Now().Add(10*time.Second), true)
		return
	}
	if gate != nil {
		if gate.State != "pass" {
			s.finishWrite(ctx, o, "failed", nil, errors.New("the merge gate no longer passes: "+gate.Summary), time.Time{}, true)
			return
		}
		if vcs.CapabilitiesOf(c.Provider).StatusChecks && gate.PublishedState != "pass" {
			if time.Since(o.CreatedAt) > gatePublishWait {
				s.finishWrite(ctx, o, "failed", nil, errors.New("the passing gate could not be published to the host"), time.Time{}, true)
				return
			}
			// The host would refuse until it sees the gate; wait for it.
			s.finishWrite(ctx, o, "pending", nil, nil, time.Now().Add(2*time.Second), false)
			return
		}
	}
	if o.Attempts > 0 {
		if src := vcs.ChangeSourceFor(ref.Provider); src != nil {
			prev, _ := s.store.StoredChange(ctx, c.Repo, c.Number)
			if ch, err := src.GetChange(cctx, s.client, ref, c.Number, prev); err == nil && ch.State == "merged" {
				_, _, _ = s.store.ApplyChange(ctx, c.Repo, ref.Provider, ch)
				s.finishWrite(ctx, o, "done", map[string]string{"note": "merged by an earlier attempt"}, nil, time.Time{}, true)
				return
			}
		}
	}
	sha, err := w.Merge(cctx, s.client, ref, c.Number, vcs.MergeOpts{Method: p.Method, ExpectedHead: p.ExpectedHead})
	switch {
	case errors.Is(err, vcs.ErrHeadMoved), errors.Is(err, vcs.ErrNotMergeable):
		s.finishWrite(ctx, o, "failed", nil, err, time.Time{}, true)
		_ = s.store.RequestChangeRefresh(ctx, c.Repo, ref.Provider, c.Number, store.PriorityWebhook, 0, false)
		return
	case err != nil:
		s.retryOrFail(ctx, o, err, cred, ref.Provider)
		return
	}
	s.finishWrite(ctx, o, "done", map[string]string{"merge_sha": sha}, nil, time.Time{}, true)
	// The host's webhook will say merged too; refresh now so the page does.
	_ = s.store.RequestChangeRefresh(ctx, c.Repo, ref.Provider, c.Number, store.PriorityWebhook, 0, false)
}
