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
	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

type Scheduler struct {
	store        *store.Store
	blobs        blob.Store
	logs         *logstore.Service
	tick         time.Duration
	staleAfter   time.Duration
	gcEvery      time.Duration // retention GC cadence
	archiveEvery time.Duration // log-archive safety-net sweep cadence
	retention    time.Duration // RETENTION_DAYS as a duration; 0 = keep forever

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
		gcEvery:       time.Hour,
		archiveEvery:  30 * time.Second,
		retention:     retentionWindow(),
		poster:        vcs.NewPoster(),
		statusEnabled: os.Getenv("COMMIT_STATUS") != "off",
		statusSem:     make(chan struct{}, 4),
		inflight:      map[string]struct{}{},
	}
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
	// Run one GC pass shortly after startup so operators see it work without
	// waiting a full hour.
	firstGC := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.step(ctx)
		case <-archive.C:
			sc.archivePending(ctx)
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

// gc runs retention cleanup: expired sessions always, and pipelines (with
// their artifact blobs) older than the retention window when one is set.
func (sc *Scheduler) gc(ctx context.Context) {
	if n, err := sc.store.DeleteExpiredSessions(ctx); err != nil {
		slog.Error("gc: delete expired sessions", "err", err)
	} else if n > 0 {
		slog.Info("gc: deleted expired sessions", "rows", n)
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
	if n > 0 || blobsDeleted > 0 || logsDeleted > 0 {
		slog.Info("gc: retention sweep", "pipelines", n, "artifact_blobs", blobsDeleted,
			"log_objects", logsDeleted, "retention", sc.retention)
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
