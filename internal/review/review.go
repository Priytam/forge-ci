// Package review keeps Forge's copy of every pull/merge request current with
// as few provider calls as possible (batch 7):
//
//   - Webhooks are the source of truth. Ingest applies what a delivery
//     carries and asks for one refresh of that change, makes no provider call
//     itself, and coalesces bursts (debounce per change).
//   - A refresh is one GetChange, claimed with a lease so replicas never
//     duplicate it, under the credential's token bucket.
//   - The only scheduled provider traffic is the reconcile: one conditional
//     list per repo (a 304 when nothing changed), every 15 minutes while the
//     repo has open changes and every few hours otherwise.
//
// Pages never come here: they read Postgres only (see internal/api).
package review

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// debounce is how long a change's refresh waits for more events, so a burst
// (a push that also dismisses reviews, a review with comments) costs one call.
var debounce = 2 * time.Second

const (
	refreshLease   = time.Minute
	reconcileLease = 2 * time.Minute
	reconcileOpen  = 15 * time.Minute // repos with open changes
	reconcileIdle  = 6 * time.Hour    // repos with none
	reconcileRetry = 5 * time.Minute  // after a transient error
	maxAttempts    = 8                // transient refresh failures before giving up (~40 min)
	statsKeep      = 400 * 24 * time.Hour
)

// Enabled reports whether code review is on (CODE_REVIEW=off disables the
// ingest, refresh and reconcile; pages then show what is stored).
func Enabled() bool { return os.Getenv("CODE_REVIEW") != "off" }

// Ingest normalises one webhook delivery and records it: the payload's
// snapshot is applied at once (the page updates before the refresh) and one
// coalesced refresh is requested per change. Deliveries for repos Forge does
// not know, or from a provider other than the one registered, are ignored.
func Ingest(ctx context.Context, st *store.Store, provider string, hdr http.Header, body []byte) (int, error) {
	if !Enabled() {
		return 0, nil
	}
	src := vcs.ChangeSourceFor(provider)
	if src == nil {
		return 0, nil
	}
	events, err := src.NormalizeEvent(hdr, body)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ev := range events {
		registered, found, err := st.RegisteredProvider(ctx, ev.Repo)
		if err != nil {
			return n, err
		}
		if !found || vcs.Lookup(registered).Name() != vcs.Lookup(provider).Name() {
			continue
		}
		if ev.Snapshot != nil {
			if _, _, err := st.ApplyChange(ctx, ev.Repo, registered, ev.Snapshot); err != nil {
				return n, err
			}
		}
		if err := st.RequestChangeRefresh(ctx, ev.Repo, registered, ev.ChangeID,
			store.PriorityWebhook, debounce, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Service runs the refresh worker and the reconcile.
type Service struct {
	store  *store.Store
	blobs  blob.Store // diff storage (retention sweep)
	client *vcs.ChangeClient
	tick   time.Duration
	batch  int
}

// New builds a service whose every provider call is counted in Postgres and
// feeds the credential's budget.
func New(st *store.Store) *Service {
	s := &Service{store: st, client: vcs.NewChangeClient(), tick: time.Second, batch: 8}
	s.client.OnCall = s.onCall
	return s
}

// Client exposes the provider client (tests count its calls).
func (s *Service) Client() *vcs.ChangeClient { return s.client }

// Loop runs the service until ctx ends (no-op when code review is off).
func Loop(ctx context.Context, st *store.Store, blobs blob.Store) {
	if !Enabled() {
		slog.Info("code review: disabled (CODE_REVIEW=off)")
		return
	}
	s := New(st)
	s.blobs = blobs
	s.Run(ctx)
}

// Run drives refreshes, gate evaluations and queued writes every tick, and
// reconciles every minute (each repo only when its own schedule says so).
// The ticks read Postgres, not hosts.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	rec := time.NewTicker(time.Minute)
	defer rec.Stop()
	first := time.After(10 * time.Second)
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RefreshDue(ctx)
			s.EvaluateGates(ctx)
			s.SendOutbox(ctx)
			s.ReanchorThreads(ctx)
		case <-first:
			s.ReconcileAll(ctx)
		case <-rec.C:
			s.ReconcileAll(ctx)
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				if err := s.store.PruneProviderCallStats(ctx, statsKeep); err != nil {
					slog.Warn("code review: prune call stats", "err", err)
				}
				ExpireDiffs(ctx, s.store, s.blobs)
			}
		}
	}
}

// credKey carries the budget credential to onCall.
type credKey struct{}

func (s *Service) callCtx(ctx context.Context, class, credential string) context.Context {
	return context.WithValue(vcs.WithClass(ctx, class), credKey{}, credential)
}

// onCall counts every provider call and feeds the host's rate-limit headers
// back into the credential's bucket.
func (s *Service) onCall(ctx context.Context, info vcs.CallInfo) {
	bg := context.WithoutCancel(ctx)
	if info.ReadPath {
		slog.Error("code review: provider call attempted while serving a page (refused)",
			"provider", info.Provider, "class", info.Class)
	}
	if err := s.store.RecordProviderCall(bg, info); err != nil {
		slog.Warn("code review: record provider call", "err", err)
	}
	if cred, _ := ctx.Value(credKey{}).(string); cred != "" && info.Rate != nil {
		if err := s.store.ObserveProviderRate(bg, cred, info.Provider, *info.Rate); err != nil {
			slog.Warn("code review: observe rate", "err", err)
		}
	}
}

// RefreshDue claims due refreshes and runs them concurrently (bounded by the
// batch size). Returns how many were claimed.
func (s *Service) RefreshDue(ctx context.Context) int {
	tasks, err := s.store.ClaimChangeRefreshes(ctx, s.batch, refreshLease)
	if err != nil {
		slog.Error("code review: claim refreshes", "err", err)
		return 0
	}
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(t store.RefreshTask) {
			defer wg.Done()
			s.refresh(ctx, t)
		}(t)
	}
	wg.Wait()
	return len(tasks)
}

// errBudget is recorded on a change whose refresh waits for budget.
var errBudget = errors.New("waiting for provider API budget")

func (s *Service) refresh(ctx context.Context, t store.RefreshTask) {
	ref, cred, found, err := s.store.ReviewTarget(ctx, t.Repo)
	if err != nil {
		s.finish(ctx, t, err, backoff(t))
		return
	}
	src := vcs.ChangeSourceFor(ref.Provider)
	if !found || src == nil {
		s.drop(ctx, t, "repo is no longer connected for code review")
		return
	}
	class := store.PriorityClass(t.Priority)
	ok, wait, err := s.store.AcquireProviderBudget(ctx, cred, ref.Provider, class)
	if err != nil {
		s.finish(ctx, t, err, backoff(t))
		return
	}
	if !ok {
		s.finish(ctx, t, errBudget, time.Now().Add(wait))
		return
	}
	prev, err := s.store.StoredChange(ctx, t.Repo, t.Number)
	if err != nil {
		s.finish(ctx, t, err, backoff(t))
		return
	}
	ch, err := src.GetChange(s.callCtx(ctx, class, cred), s.client, ref, t.Number, prev)
	var rl *vcs.RateLimitedError
	switch {
	case errors.Is(err, vcs.ErrChangeNotFound):
		s.drop(ctx, t, "the host does not know this change")
		return
	case errors.As(err, &rl):
		until := time.Now().Add(rl.RetryAfter)
		if xerr := s.store.ExhaustProviderBudget(ctx, cred, ref.Provider, until); xerr != nil {
			slog.Warn("code review: exhaust budget", "err", xerr)
		}
		s.finish(ctx, t, err, until)
		return
	case vcs.Permanent(err):
		s.finish(ctx, t, err, time.Time{}) // a new webhook or reconcile asks again
		return
	case err != nil:
		s.finish(ctx, t, err, backoff(t))
		return
	}
	if _, _, err := s.store.ApplyChange(ctx, t.Repo, ref.Provider, ch); err != nil {
		s.finish(ctx, t, err, backoff(t))
		return
	}
	s.finish(ctx, t, nil, time.Time{})
}

func (s *Service) finish(ctx context.Context, t store.RefreshTask, failure error, retryAt time.Time) {
	if failure != nil && !errors.Is(failure, errBudget) {
		slog.Warn("code review: refresh failed", "repo", t.Repo, "change", t.Number, "err", failure, "retry_at", retryAt)
	}
	if err := s.store.FinishChangeRefresh(ctx, t, failure, retryAt); err != nil {
		slog.Error("code review: finish refresh", "err", err)
	}
}

func (s *Service) drop(ctx context.Context, t store.RefreshTask, reason string) {
	slog.Info("code review: refresh dropped", "repo", t.Repo, "change", t.Number, "reason", reason)
	if err := s.store.DropChangeRefresh(ctx, t, reason); err != nil {
		slog.Error("code review: drop refresh", "err", err)
	}
}

// backoff doubles from 10s per failed attempt, capped at 30 minutes; after
// maxAttempts it gives up (zero time) until something requests a refresh.
func backoff(t store.RefreshTask) time.Time {
	if t.Attempts+1 >= maxAttempts {
		return time.Time{}
	}
	d := time.Duration(math.Min(float64(30*time.Minute), float64(10*time.Second)*math.Pow(2, float64(t.Attempts))))
	return time.Now().Add(d)
}

// ReconcileAll runs the reconcile for every connected repo that is due.
func (s *Service) ReconcileAll(ctx context.Context) {
	repos, err := s.store.ListRegisteredRepos(ctx)
	if err != nil {
		slog.Error("code review: list repos", "err", err)
		return
	}
	for _, r := range repos {
		if vcs.ChangeSourceFor(r.Provider) == nil {
			continue
		}
		claim, ok, err := s.store.ClaimReconcile(ctx, r.Repo, reconcileLease)
		if err != nil {
			slog.Error("code review: claim reconcile", "repo", r.Repo, "err", err)
			continue
		}
		if ok {
			s.reconcile(ctx, claim)
		}
	}
}

// reconcile compares the host's open list with Forge's copy and requests a
// refresh only for what differs. The list is conditional, so a quiet repo
// costs one 304.
func (s *Service) reconcile(ctx context.Context, claim store.ReconcileClaim) {
	next := func(open int) time.Time {
		if open > 0 {
			return time.Now().Add(reconcileOpen)
		}
		return time.Now().Add(reconcileIdle)
	}
	done := func(v vcs.Validators, at time.Time, result string) {
		if err := s.store.FinishReconcile(ctx, claim.Repo, v, at, result); err != nil {
			slog.Error("code review: finish reconcile", "repo", claim.Repo, "err", err)
		}
	}
	stored, err := s.store.OpenChangeRefs(ctx, claim.Repo)
	if err != nil {
		done(claim.Validators, time.Now().Add(reconcileRetry), err.Error())
		return
	}
	ref, cred, found, err := s.store.ReviewTarget(ctx, claim.Repo)
	if err != nil || !found {
		done(claim.Validators, time.Now().Add(reconcileRetry), "repo not reachable")
		return
	}
	src := vcs.ChangeSourceFor(ref.Provider)
	if src == nil {
		done(claim.Validators, next(0), "no change source")
		return
	}
	class, priority := vcs.ClassReconcile, store.PriorityReconcile
	if claim.First {
		class, priority = vcs.ClassBackfill, store.PriorityBackfill
	}
	if ok, wait, err := s.store.AcquireProviderBudget(ctx, cred, ref.Provider, class); err != nil || !ok {
		done(claim.Validators, time.Now().Add(max(wait, time.Minute)), "waiting for provider API budget")
		return
	}
	request := func(number string) {
		if err := s.store.RequestChangeRefresh(ctx, claim.Repo, ref.Provider, number, priority, 0, false); err != nil {
			slog.Error("code review: request refresh", "repo", claim.Repo, "change", number, "err", err)
		}
	}
	refs, v, complete, err := src.ListOpenChanges(s.callCtx(ctx, class, cred), s.client, ref, claim.Validators)
	var rl *vcs.RateLimitedError
	switch {
	case errors.Is(err, vcs.ErrNoChangeAPI):
		// Native changes: the list is Forge's own; re-read each branch tip.
		for number := range stored {
			request(number)
		}
		done(claim.Validators, next(len(stored)), "native")
		return
	case errors.Is(err, vcs.ErrNotModified):
		done(claim.Validators, next(len(stored)), "not modified")
		return
	case errors.As(err, &rl):
		until := time.Now().Add(rl.RetryAfter)
		_ = s.store.ExhaustProviderBudget(ctx, cred, ref.Provider, until)
		done(claim.Validators, until, err.Error())
		return
	case vcs.Permanent(err):
		done(claim.Validators, next(0), err.Error()) // broken credential: back off to the idle cadence
		return
	case err != nil:
		done(claim.Validators, time.Now().Add(reconcileRetry), err.Error())
		return
	}
	stale := 0
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.ID] = true
		have, ok := stored[r.ID]
		if !ok || have.HeadSHA != r.HeadSHA || have.UpdatedAt.Unix() != r.UpdatedAt.Unix() {
			request(r.ID)
			stale++
		}
	}
	if complete {
		for number := range stored {
			if !seen[number] {
				request(number) // closed or merged while a webhook was missed
				stale++
			}
		}
	}
	slog.Info("code review: reconciled", "repo", claim.Repo, "open", len(refs), "stale", stale, "class", class)
	done(v, next(len(refs)), "ok")
}
