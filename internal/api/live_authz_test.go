package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// seedApprovedDeploy makes an environment-targeting job succeed, which writes a
// deployment row for (repo, env) — the record Forge Live inherits approval from.
func seedApprovedDeploy(t *testing.T, st *store.Store, db *pgx.Conn, repo, ref, sha, env, by string) {
	t.Helper()
	ctx := context.Background()
	jobs := []compiler.CompiledJob{{
		Name: "deploy", Stage: "deploy", StageIdx: 0,
		Script: "echo deploy", Environment: env, Env: map[string]string{},
	}}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: ref, SHA: sha, Config: "x", TriggeredBy: by,
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	var jobID int64
	if err := db.QueryRow(ctx, `SELECT id FROM jobs WHERE pipeline_id=$1 AND name='deploy'`, p.ID).Scan(&jobID); err != nil {
		t.Fatalf("find deploy job: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE jobs SET status='running', started_at=now(), heartbeat_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if final, _, err := st.CompleteJob(ctx, jobID, "success", 0); err != nil || final != "success" {
		t.Fatalf("CompleteJob: final=%q err=%v", final, err)
	}
}

// TestAutoSyncInheritsApprovedDeploy proves the pipeline-approval → Live-sync
// linkage: an approved deploy to a protected environment stands as the approval
// for syncing the revision it shipped, so auto-sync releases the op at once
// instead of waiting for a second, identical approval — but only for the apply
// that deploy authorizes, not later changes.
func TestAutoSyncInheritsApprovedDeploy(t *testing.T) {
	ctx := context.Background()
	h := newLiveHarness(t)
	const repo, env = "lab/platform-gitops", "fleet" // the harness target's repo/env

	// An app exists on the target, out of sync with a new revision.
	app := live.App{Key: "prod/api", Name: "api", Namespace: "prod"}
	h.batch(live.Batch{Epoch: "a", Seq: 1, Synced: true, Deltas: []live.Delta{{App: app,
		Status:  &live.Status{Health: live.Health{Status: live.Healthy}},
		Upserts: []live.Resource{res("d1", "Deployment")}, Keep: []string{"d1"},
		Desired: &live.Desired{Revision: live.Revision{SHA: "rev1"},
			Manifests: map[string]json.RawMessage{"apps/Deployment//d1": json.RawMessage(`{}`)}}}}})
	id := h.apps("").Apps[0].ID

	// No deploy yet → nothing authorizes a sync.
	if _, ok, err := h.st.DeployAuthorizationSince(ctx, repo, env, time.Time{}); err != nil || ok {
		t.Fatalf("before any deploy: ok=%v err=%v", ok, err)
	}

	// An approved deploy to (repo, env) succeeds.
	seedApprovedDeploy(t, h.st, h.db, repo, "main", "rev1", env, "alice")

	auth, ok, err := h.st.DeployAuthorizationSince(ctx, repo, env, time.Time{})
	if err != nil || !ok {
		t.Fatalf("after the deploy: ok=%v err=%v", ok, err)
	}
	if auth.By != "alice" || auth.JobID == 0 || auth.DeploymentID == 0 {
		t.Fatalf("authorization = %+v", auth)
	}

	// Auto-sync creates the op already released (pending), not waiting for approval.
	o, err := h.st.CreateApprovedLiveOp(ctx, id, live.Op{Kind: live.OpSync, Revision: "rev1", Force: true}, "Forge auto-sync", auth)
	if err != nil {
		t.Fatalf("CreateApprovedLiveOp: %v", err)
	}
	if o.State != "pending" {
		t.Fatalf("op state = %q, want pending (released, not awaiting approval)", o.State)
	}
	full, err := h.st.LiveOp(ctx, o.ID)
	if err != nil {
		t.Fatalf("LiveOp: %v", err)
	}
	if len(full.Approvals) != 1 || full.Approvals[0].Verdict != "approved" || full.Approvals[0].Approver != "alice" {
		t.Fatalf("inherited approval = %+v", full.Approvals)
	}

	// One approval authorizes one apply: a later hand-pushed revision (no fresh
	// deploy) finds nothing newer than this deploy and must wait for its own.
	if _, ok, err := h.st.DeployAuthorizationSince(ctx, repo, env, auth.At); err != nil || ok {
		t.Fatalf("after consuming the deploy: ok=%v err=%v", ok, err)
	}
}
