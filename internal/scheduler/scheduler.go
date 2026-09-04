// Package scheduler drives the job state machine. Every transition it applies
// is an idempotent SQL statement, so running it after a crash — or running two
// schedulers concurrently — cannot corrupt state.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/cron"
	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

type Scheduler struct {
	store         *store.Store
	blobs         blob.Store
	logs          *logstore.Service
	tick          time.Duration
	staleAfter    time.Duration
	gcEvery       time.Duration // retention GC cadence
	archiveEvery  time.Duration // log-archive safety-net sweep cadence
	scheduleEvery time.Duration // cron-schedule fire-check cadence
	retention     time.Duration // RETENTION_DAYS as a duration; 0 = keep forever

	// Commit-status write-back: post pipeline status back to the origin VCS.
	poster        *vcs.Poster
	statusEnabled bool                // COMMIT_STATUS != "off"
	statusSem     chan struct{}       // bounds concurrent in-flight posts
	inflightMu    sync.Mutex          // guards inflight
	inflight      map[string]struct{} // (pipeline:status) currently being posted this process
}

func New(s *store.Store, blobs blob.Store, logs *logstore.Service) *Scheduler {
	return &Scheduler{
		store:         s,
		blobs:         blobs,
		logs:          logs,
		tick:          time.Second,
		staleAfter:    90 * time.Second,
		gcEvery:       gcInterval(),
		archiveEvery:  30 * time.Second,
		scheduleEvery: 30 * time.Second,
		retention:     retentionWindow(),
		poster:        vcs.NewPoster(),
		statusEnabled: os.Getenv("COMMIT_STATUS") != "off",
		statusSem:     make(chan struct{}, 4),
		inflight:      map[string]struct{}{},
	}
}

// gcInterval reads GC_INTERVAL (a Go duration, default 1h) — the cadence of the
// retention/expiry sweep. A shorter interval makes per-artifact expire_in take
// effect promptly; the default matches the historical hourly sweep.
func gcInterval() time.Duration {
	if v := os.Getenv("GC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// retentionWindow reads RETENTION_DAYS (default 30). 0 disables pipeline/
// artifact GC (expired sessions are always collected).
func retentionWindow() time.Duration {
	days := 30
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

func (sc *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	gc := time.NewTicker(sc.gcEvery)
	defer gc.Stop()
	archive := time.NewTicker(sc.archiveEvery)
	defer archive.Stop()
	schedules := time.NewTicker(sc.scheduleEvery)
	defer schedules.Stop()
	// Run one GC pass shortly after startup so operators see it work without
	// waiting a full interval — but never later than the configured cadence
	// (a sub-30s GC_INTERVAL fires its first sweep on that cadence instead).
	firstGCDelay := 30 * time.Second
	if sc.gcEvery < firstGCDelay {
		firstGCDelay = sc.gcEvery
	}
	firstGC := time.After(firstGCDelay)
	// Fire due schedules shortly after startup too, so a schedule already past
	// its next_run_at (e.g. while the server was down) fires promptly rather than
	// waiting a full scheduleEvery interval.
	firstSchedules := time.After(5 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.step(ctx)
		case <-archive.C:
			sc.archivePending(ctx)
		case <-firstSchedules:
			sc.fireSchedules(ctx)
		case <-schedules.C:
			sc.fireSchedules(ctx)
		case <-firstGC:
			sc.gc(ctx)
		case <-gc.C:
			sc.gc(ctx)
		}
	}
}

// archivePending is the log-archive safety net: it flushes to the blob store the
// logs of jobs that reached a terminal state WITHOUT a runner complete call
// (stale/overdue/canceled-while-pending), whose Redis live buffers would
// otherwise expire unflushed. No-op for the postgres log backend.
func (sc *Scheduler) archivePending(ctx context.Context) {
	n, err := sc.logs.ArchivePending(ctx)
	if err != nil {
		slog.Error("archive pending logs", "err", err)
		return
	}
	if n > 0 {
		slog.Info("archived terminal-but-unflushed job logs", "jobs", n)
	}
}

// fireSchedules finds enabled schedules whose next_run_at has passed and, for
// each, atomically CLAIMs it (compare-and-set advancing next_run_at to the next
// future slot and stamping last_run_at) before building a pipeline. The
// claim-before-build order is what makes firing replica-safe AND catch-up-safe:
//
//   - single-fire across replicas: only the tick whose compare-and-set matches
//     the value it read wins; every other replica/tick sees the advanced value
//     and skips. This mirrors ClaimStatusPost's dedup role for commit statuses.
//   - no storm on a missed window: next_run_at advances to the next slot AFTER
//     now(), so a schedule that missed hours of windows fires once and resumes
//     its cadence — it never backfills.
//
// A schedule whose repo has no registered config, or whose ref can't be
// resolved to a sha, is skipped with a logged warning: the claim already
// advanced next_run_at, so a broken schedule is retried on its next slot, not
// re-hammered every tick.
func (sc *Scheduler) fireSchedules(ctx context.Context) {
	now := time.Now().UTC()
	due, err := sc.store.DueSchedules(ctx, now)
	if err != nil {
		slog.Error("schedules: list due", "err", err)
		return
	}
	for _, s := range due {
		next, err := cron.Next(s.Cron, now)
		if err != nil {
			// A bad expression should never reach the DB (the API validates on
			// create/update), but if one does, skip it rather than crash-loop.
			slog.Error("schedules: bad cron expression, skipping",
				"schedule", s.ID, "repo", s.Repo, "cron", s.Cron, "err", err)
			continue
		}
		claimed, err := sc.store.ClaimScheduleFire(ctx, s.ID, s.NextRunAt, next, now)
		if err != nil {
			slog.Error("schedules: claim", "err", err, "schedule", s.ID)
			continue
		}
		if !claimed {
			continue // another tick/replica already fired this schedule
		}
		p, err := sc.store.BuildScheduledPipeline(ctx, s.Repo, s.Ref)
		if err != nil {
			slog.Warn("schedules: could not fire (skipped) — schedule advanced to next slot",
				"schedule", s.ID, "repo", s.Repo, "ref", s.Ref, "next_run_at", next, "err", err)
			continue
		}
		slog.Info("schedule fired pipeline",
			"schedule", s.ID, "repo", s.Repo, "ref", s.Ref, "pipeline", p.ID,
			"sha", shortSHA(p.SHA), "next_run_at", next)
	}
}

// gc runs retention cleanup: expired sessions always, and pipelines (with
// their artifact blobs) older than the retention window when one is set.
func (sc *Scheduler) gc(ctx context.Context) {
	if n, err := sc.store.DeleteExpiredSessions(ctx); err != nil {
		slog.Error("gc: delete expired sessions", "err", err)
	} else if n > 0 {
		slog.Info("gc: deleted expired sessions", "rows", n)
	}

	// Per-artifact expiry (artifacts.expire_in), INDEPENDENT of RETENTION_DAYS:
	// delete blobs then rows for artifacts whose expires_at has passed. A short
	// expire_in expires promptly even when retention is large or disabled;
	// artifacts with no expire_in (NULL expires_at) are untouched here and fall
	// back to the retention sweep below. Runs before the retention guard so it
	// works even with RETENTION_DAYS=0 (retention off).
	if keys, err := sc.store.ExpiredArtifactBlobKeysByExpiry(ctx); err != nil {
		slog.Error("gc: list expire_in artifact blobs", "err", err)
	} else {
		expiredBlobs := 0
		for _, k := range keys {
			if err := sc.blobs.Delete(ctx, k); err != nil {
				slog.Error("gc: delete expire_in artifact blob", "err", err, "key", k)
				continue
			}
			expiredBlobs++
		}
		if n, err := sc.store.DeleteExpiredArtifactsByExpiry(ctx); err != nil {
			slog.Error("gc: delete expire_in artifact rows", "err", err)
		} else if n > 0 || expiredBlobs > 0 {
			slog.Info("gc: expired artifacts (expire_in)", "rows", n, "blobs", expiredBlobs)
		}
	}

	if sc.retention <= 0 {
		return // retention disabled — keep pipelines/artifacts forever
	}

	if n, err := sc.store.DeleteExpiredWebhookDeliveries(ctx, sc.retention); err != nil {
		slog.Error("gc: delete expired webhook deliveries", "err", err)
	} else if n > 0 {
		slog.Info("gc: deleted expired webhook deliveries", "rows", n)
	}

	// Prune the append-only audit trail. This is the ONLY path that deletes
	// audit rows (retention only); normal operation is insert/select.
	if n, err := sc.store.DeleteExpiredAudit(ctx, sc.retention); err != nil {
		slog.Error("gc: delete expired audit rows", "err", err)
	} else if n > 0 {
		slog.Info("gc: deleted expired audit rows", "rows", n)
	}

	// Delete artifact blobs and archived log objects first (we still have the DB
	// rows to find their keys), then cascade-delete the pipeline rows.
	keys, err := sc.store.ExpiredArtifactBlobKeys(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: list expired artifact blobs", "err", err)
		return
	}
	blobsDeleted := 0
	for _, k := range keys {
		if err := sc.blobs.Delete(ctx, k); err != nil {
			slog.Error("gc: delete artifact blob", "err", err, "key", k)
			continue
		}
		blobsDeleted++
	}
	logKeys, err := sc.store.ExpiredLogObjectKeys(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: list expired log objects", "err", err)
		return
	}
	logsDeleted := 0
	for _, k := range logKeys {
		if err := sc.blobs.Delete(ctx, k); err != nil {
			slog.Error("gc: delete log object", "err", err, "key", k)
			continue
		}
		logsDeleted++
	}
	n, err := sc.store.DeleteExpiredPipelines(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: delete expired pipelines", "err", err)
		return
	}

	// Caches are shared across pipelines, so they are GC'd on their own age
	// (updated_at) rather than cascading with a pipeline. Delete the blobs, then
	// the rows.
	cacheKeys, err := sc.store.ExpiredCacheBlobKeys(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: list expired cache blobs", "err", err)
		return
	}
	cachesDeleted := 0
	for _, k := range cacheKeys {
		if err := sc.blobs.Delete(ctx, k); err != nil {
			slog.Error("gc: delete cache blob", "err", err, "key", k)
			continue
		}
		cachesDeleted++
	}
	if _, err := sc.store.DeleteExpiredCache(ctx, sc.retention); err != nil {
		slog.Error("gc: delete expired cache rows", "err", err)
	}

	if n > 0 || blobsDeleted > 0 || logsDeleted > 0 || cachesDeleted > 0 {
		slog.Info("gc: retention sweep", "pipelines", n, "artifact_blobs", blobsDeleted,
			"log_objects", logsDeleted, "cache_blobs", cachesDeleted, "retention", sc.retention)
	}
}

func (sc *Scheduler) step(ctx context.Context) {
	// Post commit statuses BEFORE the transition ops so a freshly-created
	// pipeline is observed in its "created" (queued) phase this tick and its
	// pending status is posted, before PromoteReadyJobs moves it to running.
	sc.postStatuses(ctx)

	for _, op := range []struct {
		name string
		fn   func(context.Context) (int64, error)
	}{
		{"cancel_dead", sc.store.CancelDeadJobs},
		{"fail_fast_cancel", sc.store.FailFastCancel},
		{"promote_ready", sc.store.PromoteReadyJobs},
		{"expire_blocked", sc.store.ExpireBlockedJobs},
		{"fail_overdue", sc.store.FailOverdueJobs},
		{"fail_stuck_pending", sc.store.FailStuckPending},
		{"fail_stale", func(ctx context.Context) (int64, error) {
			return sc.store.FailStaleJobs(ctx, sc.staleAfter)
		}},
	} {
		n, err := op.fn(ctx)
		if err != nil {
			slog.Error("scheduler op failed", "op", op.name, "err", err)
			continue
		}
		if n > 0 {
			slog.Info("scheduler transition", "op", op.name, "jobs", n)
		}
	}
}

// postStatuses finds pipelines whose current commit-status phase has not yet
// been posted, claims each one (so concurrent ticks/instances never double-post),
// and delivers it asynchronously. Delivery is off the tick's critical path: a
// slow or broken VCS API can never block the scheduler.
func (sc *Scheduler) postStatuses(ctx context.Context) {
	if !sc.statusEnabled {
		return
	}
	candidates, err := sc.store.PipelinesPendingStatusPost(ctx)
	if err != nil {
		slog.Error("commit-status: list pending", "err", err)
		return
	}
	for _, c := range candidates {
		key := fmt.Sprintf("%d:%s", c.PipelineID, c.Status)

		sc.inflightMu.Lock()
		if _, busy := sc.inflight[key]; busy {
			sc.inflightMu.Unlock()
			continue // a previous tick's post for this exact state is still running
		}
		claimed, err := sc.store.ClaimStatusPost(ctx, c.PipelineID, c.Status)
		if err != nil {
			sc.inflightMu.Unlock()
			slog.Error("commit-status: claim", "err", err, "pipeline", c.PipelineID)
			continue
		}
		if !claimed {
			sc.inflightMu.Unlock()
			continue // another instance owns this post
		}
		sc.inflight[key] = struct{}{}
		sc.inflightMu.Unlock()

		sc.statusSem <- struct{}{}
		go func(c store.StatusCandidate, key string) {
			defer func() {
				<-sc.statusSem
				sc.inflightMu.Lock()
				delete(sc.inflight, key)
				sc.inflightMu.Unlock()
			}()
			sc.deliverStatus(ctx, c)
		}(c, key)
	}
}

// deliverStatus resolves the repo's decrypted token and posts one status. On a
// permanent failure (bad token/scope, wrong repo/sha) it keeps the dedup claim
// and logs an actionable message so a misconfigured repo is not hammered; on a
// transient failure it releases the claim so a later tick retries. The token is
// never logged.
func (sc *Scheduler) deliverStatus(ctx context.Context, c store.StatusCandidate) {
	// CodeCommit has no commit-status API; its result goes to the pull request
	// as a comment instead, over an IAM-signed call with no token involved.
	if c.Provider == "codecommit" {
		sc.deliverCodeCommitComment(ctx, c)
		return
	}
	provider, token, ok, err := sc.store.RepoStatusTarget(ctx, c.Repo)
	if err != nil {
		slog.Error("commit-status: resolve repo token", "err", err, "repo", c.Repo)
		sc.releaseStatus(ctx, c) // treat as transient; retry later
		return
	}
	// Belt-and-braces: the candidate query already excludes unpostable repos,
	// but the registry could have changed between the query and here.
	if !ok || token == "" || (provider != "github" && provider != "bitbucket") {
		slog.Debug("commit-status: repo not postable, skipping", "repo", c.Repo, "provider", provider)
		return
	}

	err = sc.poster.Post(ctx, vcs.PostRequest{
		Provider:  provider,
		Repo:      c.Repo,
		SHA:       c.SHA,
		Token:     token,
		Status:    c.Status,
		TargetURL: pipelineURL(c.PipelineID),
	})
	if err == nil {
		slog.Info("commit-status posted", "repo", c.Repo, "sha", shortSHA(c.SHA),
			"status", c.Status, "provider", provider)
		return
	}
	var pe *vcs.PostError
	if errors.As(err, &pe) && pe.Permanent {
		slog.Error("commit-status: giving up — check the repo's token scope "+
			"(GitHub repo:status / Bitbucket repositories:write), repo name and sha",
			"repo", c.Repo, "sha", shortSHA(c.SHA), "status", c.Status,
			"provider", provider, "http_status", pe.StatusCode, "attempts", pe.Attempts)
		return // keep the claim: don't re-hammer a misconfigured repo
	}
	slog.Warn("commit-status: transient delivery failure, will retry on a later tick",
		"repo", c.Repo, "sha", shortSHA(c.SHA), "status", c.Status, "err", err)
	sc.releaseStatus(ctx, c)
}

// deliverCodeCommitComment reports a finished CodeCommit pipeline by commenting
// on the pull request that triggered it. The candidate scan only ever offers
// terminal phases of runs that HAVE a pull request, so this posts exactly one
// comment per run and nothing at all for a push run.
//
// Claim handling matches the status path: a permanent failure (missing PR, bad
// commit, denied policy) keeps the dedup claim so a misconfigured repo is not
// hammered; anything transient releases it for a later tick.
func (sc *Scheduler) deliverCodeCommitComment(ctx context.Context, c store.StatusCandidate) {
	repo, roleARN, ok, err := sc.store.CodeCommitStatusTarget(ctx, c.Repo)
	if err != nil {
		slog.Error("pr-comment: resolve codecommit target", "err", err, "repo", c.Repo)
		sc.releaseStatus(ctx, c) // treat as transient; retry later
		return
	}
	if !ok || c.MRIID == "" {
		// The registry changed under us, or the run has no pull request after
		// all. Nothing to report, and nothing to retry.
		slog.Debug("pr-comment: nothing to comment on, skipping",
			"repo", c.Repo, "pipeline", c.PipelineID)
		return
	}

	body := vcs.MapCodeCommitComment(c.Status, pipelineURL(c.PipelineID))
	permanent, err := sc.poster.PostCodeCommitComment(
		ctx, repo, roleARN, c.MRIID, c.MRBaseSHA, c.SHA, body)
	if err == nil {
		slog.Info("pr-comment posted", "repo", c.Repo, "pull_request", c.MRIID,
			"sha", shortSHA(c.SHA), "status", c.Status, "provider", "codecommit")
		return
	}
	if permanent {
		slog.Error("pr-comment: giving up — check the pull request id, the commits "+
			"and that Forge's AWS identity has codecommit:PostCommentForPullRequest",
			"repo", c.Repo, "pull_request", c.MRIID, "status", c.Status, "err", err)
		return // keep the claim: don't re-hammer a misconfigured repo
	}
	slog.Warn("pr-comment: transient delivery failure, will retry on a later tick",
		"repo", c.Repo, "pull_request", c.MRIID, "status", c.Status, "err", err)
	sc.releaseStatus(ctx, c)
}

func (sc *Scheduler) releaseStatus(ctx context.Context, c store.StatusCandidate) {
	if err := sc.store.ReleaseStatusPost(ctx, c.PipelineID, c.Status); err != nil {
		slog.Error("commit-status: release claim", "err", err, "pipeline", c.PipelineID)
	}
}

// pipelineURL is the target_url for a commit status: the Forge pipeline page.
// The dashboard route is /pipelines/:id (see web/src/App.tsx). FRONTEND_URL is
// preferred (that's where the page lives); EXTERNAL_URL is the fallback.
func pipelineURL(id int64) string {
	base := os.Getenv("FRONTEND_URL")
	if base == "" {
		base = os.Getenv("EXTERNAL_URL")
	}
	if base == "" {
		base = "http://localhost:5173"
	}
	return strings.TrimSuffix(base, "/") + "/pipelines/" + strconv.FormatInt(id, 10)
}

// shortSHA trims a commit sha to its first 7 characters for log lines.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
