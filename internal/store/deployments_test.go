package store

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// newDeployTestStore spins up a throwaway database (dropped on cleanup) for the
// deployments/board/rollback/freeze tests. Skips when Postgres is unreachable.
func newDeployTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, appBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed deployment test", err)
	}
	dbName := fmt.Sprintf("forge_deploy_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}
	u, _ := url.Parse(appBaseDSN())
	u.Path = "/" + dbName
	st, err := New(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("store.New on throwaway db: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return st
}

// makeEnvPipeline creates a pipeline with a single environment-targeting job and
// returns the pipeline and the job id.
func makeEnvPipeline(t *testing.T, st *Store, repo, ref, sha, env, triggeredBy string) (*proto.Pipeline, int64) {
	t.Helper()
	ctx := context.Background()
	jobs := []compiler.CompiledJob{{
		Name: "deploy", Stage: "deploy", StageIdx: 0,
		Script: "echo deploy", Environment: env, Env: map[string]string{},
	}}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: ref, SHA: sha, Config: "x", TriggeredBy: triggeredBy,
	}, jobs, nil, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	var jobID int64
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM jobs WHERE pipeline_id=$1 AND name='deploy'`, p.ID).Scan(&jobID); err != nil {
		t.Fatalf("find job: %v", err)
	}
	return p, jobID
}

// setRunning forces a job into the 'running' state so CompleteJob can terminate it.
func setRunning(t *testing.T, st *Store, jobID int64) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`UPDATE jobs SET status='running', started_at=now(), heartbeat_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("set running: %v", err)
	}
}

// TestDeploymentRecordedOnceOnEnvJobSuccess proves a deployment row is written
// when an environment job reaches success, and that a duplicate/late CompleteJob
// does not create a second row (idempotent, deduped by job_id).
func TestDeploymentRecordedOnceOnEnvJobSuccess(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)

	_, jobID := makeEnvPipeline(t, st, "acme/app", "main", "sha-aaa", "production", "alice")
	setRunning(t, st, jobID)

	final, _, err := st.CompleteJob(ctx, jobID, "success", 0)
	if err != nil || final != "success" {
		t.Fatalf("CompleteJob success: final=%q err=%v", final, err)
	}

	deps, total, err := st.ListDeployments(ctx, "acme/app", "production", 50, 0)
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if total != 1 || len(deps) != 1 {
		t.Fatalf("want 1 deployment, got total=%d len=%d", total, len(deps))
	}
	d := deps[0]
	if d.SHA != "sha-aaa" || d.Ref != "main" || d.DeployedBy != "alice" || d.Status != "success" {
		t.Fatalf("unexpected deployment: %+v", d)
	}

	// A late/duplicate runner report: the job already left 'running' → ErrNotFound,
	// and no second deployment row. Also force-run again and re-complete to prove
	// the ON CONFLICT(job_id) dedupe holds even if CompleteJob's success branch runs twice.
	if _, _, err := st.CompleteJob(ctx, jobID, "success", 0); err != ErrNotFound {
		t.Fatalf("duplicate complete: want ErrNotFound, got %v", err)
	}
	setRunning(t, st, jobID)
	if _, _, err := st.CompleteJob(ctx, jobID, "success", 0); err != nil {
		t.Fatalf("re-complete: %v", err)
	}
	_, total, err = st.ListDeployments(ctx, "acme/app", "production", 50, 0)
	if err != nil {
		t.Fatalf("ListDeployments2: %v", err)
	}
	if total != 1 {
		t.Fatalf("dedupe failed: want 1 deployment after re-complete, got %d", total)
	}
}

// TestDeployedByPrefersApprover proves that when the env job was approval-gated,
// deployed_by is the approver rather than the pipeline's triggered_by.
func TestDeployedByPrefersApprover(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	_, jobID := makeEnvPipeline(t, st, "acme/app", "main", "sha-appr", "production", "alice")
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO job_approvals (job_id, approver, verdict) VALUES ($1,'carol','approved')`, jobID); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
	setRunning(t, st, jobID)
	if _, _, err := st.CompleteJob(ctx, jobID, "success", 0); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	deps, _, err := st.ListDeployments(ctx, "acme/app", "production", 50, 0)
	if err != nil || len(deps) != 1 {
		t.Fatalf("ListDeployments: len=%d err=%v", len(deps), err)
	}
	if deps[0].DeployedBy != "carol" {
		t.Fatalf("deployed_by should prefer approver 'carol', got %q", deps[0].DeployedBy)
	}
}

// TestNonEnvJobDoesNotRecordDeployment proves a job WITHOUT an environment never
// produces a deployment row.
func TestNonEnvJobDoesNotRecordDeployment(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	_, jobID := makeEnvPipeline(t, st, "acme/app", "main", "sha-x", "", "bob")
	setRunning(t, st, jobID)
	if _, _, err := st.CompleteJob(ctx, jobID, "success", 0); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	rows, err := st.EnvironmentsForRepo(ctx, "acme/app")
	if err != nil {
		t.Fatalf("EnvironmentsForRepo: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want no environments, got %d", len(rows))
	}
}

// TestBoardCurrentIsNewestAndCount deploys twice to production at different SHAs
// and asserts the board's current is the newest and the count is 2.
func TestBoardCurrentIsNewestAndCount(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)

	_, j1 := makeEnvPipeline(t, st, "acme/app", "main", "sha-old", "production", "alice")
	setRunning(t, st, j1)
	if _, _, err := st.CompleteJob(ctx, j1, "success", 0); err != nil {
		t.Fatalf("complete j1: %v", err)
	}
	_, j2 := makeEnvPipeline(t, st, "acme/app", "main", "sha-new", "production", "alice")
	setRunning(t, st, j2)
	if _, _, err := st.CompleteJob(ctx, j2, "success", 0); err != nil {
		t.Fatalf("complete j2: %v", err)
	}

	rows, err := st.EnvironmentsForRepo(ctx, "acme/app")
	if err != nil {
		t.Fatalf("EnvironmentsForRepo: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 environment, got %d", len(rows))
	}
	if rows[0].Environment != "production" || rows[0].Count != 2 {
		t.Fatalf("want production count=2, got env=%q count=%d", rows[0].Environment, rows[0].Count)
	}
	if rows[0].Current.SHA != "sha-new" {
		t.Fatalf("current should be newest sha-new, got %q", rows[0].Current.SHA)
	}

	// RollbackTarget by pipeline_id and by sha resolve the OLD deployment.
	p1SHA, p1Ref, err := st.RollbackTarget(ctx, "acme/app", "production", rows[0].Current.PipelineID-1, "")
	if err != nil {
		t.Fatalf("RollbackTarget by pipeline: %v", err)
	}
	if p1SHA != "sha-old" || p1Ref != "main" {
		t.Fatalf("rollback-by-pipeline want sha-old/main, got %q/%q", p1SHA, p1Ref)
	}
	s2, _, err := st.RollbackTarget(ctx, "acme/app", "production", 0, "sha-old")
	if err != nil {
		t.Fatalf("RollbackTarget by sha: %v", err)
	}
	if s2 != "sha-old" {
		t.Fatalf("rollback-by-sha want sha-old, got %q", s2)
	}
	if _, _, err := st.RollbackTarget(ctx, "acme/app", "production", 0, "sha-never"); err != ErrNotFound {
		t.Fatalf("rollback unknown sha: want ErrNotFound, got %v", err)
	}
}

// TestFreezeHoldsThenReleases proves PromoteReadyJobs holds an env job in
// 'created' during an active freeze and promotes it once the window has passed.
func TestFreezeHoldsThenReleases(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)

	// Active freeze on staging for this repo.
	if _, err := st.CreateFreeze(ctx, proto.DeployFreeze{
		Repo: "acme/app", Environment: "staging",
		StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour),
		Reason: "release train",
	}); err != nil {
		t.Fatalf("CreateFreeze: %v", err)
	}
	frozen, err := st.IsFrozen(ctx, "acme/app", "staging")
	if err != nil || !frozen {
		t.Fatalf("IsFrozen: want true, got %v err=%v", frozen, err)
	}

	// staging is NOT protected, so absent a freeze it would promote to pending.
	_, jobID := makeEnvPipeline(t, st, "acme/app", "main", "sha-1", "staging", "alice")
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatalf("PromoteReadyJobs (frozen): %v", err)
	}
	var status string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "created" {
		t.Fatalf("frozen env job should stay 'created', got %q", status)
	}

	// End the freeze window; next promote releases the held job to pending.
	if _, err := st.pool.Exec(ctx, `UPDATE deploy_freezes SET ends_at=now() WHERE repo='acme/app'`); err != nil {
		t.Fatalf("expire freeze: %v", err)
	}
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatalf("PromoteReadyJobs (thawed): %v", err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("read status2: %v", err)
	}
	if status != "pending" {
		t.Fatalf("thawed env job should be 'pending', got %q", status)
	}
}
