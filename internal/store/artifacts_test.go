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

// newArtifactsTestStore spins up a throwaway DB (dropped on cleanup). Skips when
// Postgres is unreachable.
func newArtifactsTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, appBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed artifacts test", err)
	}
	dbName := fmt.Sprintf("forge_artifacts_test_%d", time.Now().UnixNano())
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

// TestArtifactExpirySelectsRightRows verifies that a short expire_in artifact is
// selected (and deleted) by the expiry sweep while a sibling with no expire_in
// survives — independent of RETENTION_DAYS.
func TestArtifactExpirySelectsRightRows(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()

	jobs := []compiler.CompiledJob{
		{Name: "expiring", Stage: "build", StageIdx: 0, Script: "echo hi",
			Env: map[string]string{}, ArtifactExpireSeconds: 3600},
		{Name: "keep", Stage: "build", StageIdx: 0, Script: "echo hi",
			Env: map[string]string{}, ArtifactExpireSeconds: 0},
	}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "r", Ref: "main", SHA: "abc", Config: "x",
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	_, pjobs, err := st.GetPipeline(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	var expiringID, keepID int64
	for _, j := range pjobs {
		switch j.Name {
		case "expiring":
			expiringID = j.ID
		case "keep":
			keepID = j.ID
		}
	}

	// The expiring artifact gets a non-NULL expires_at (job.artifact_expire_seconds>0).
	aExp, err := st.SaveArtifact(ctx, expiringID, "a.tar.gz", "job-x/a.tar.gz", 100)
	if err != nil {
		t.Fatalf("SaveArtifact expiring: %v", err)
	}
	if aExp.ExpiresAt == nil {
		t.Fatal("expiring artifact should have a non-nil expires_at")
	}
	// The sibling has no expire_in -> NULL expires_at.
	aKeep, err := st.SaveArtifact(ctx, keepID, "b.tar.gz", "job-y/b.tar.gz", 100)
	if err != nil {
		t.Fatalf("SaveArtifact keep: %v", err)
	}
	if aKeep.ExpiresAt != nil {
		t.Fatalf("sibling artifact should have nil expires_at, got %v", aKeep.ExpiresAt)
	}

	// Nothing is expired yet (expires_at is 1h in the future).
	if keys, err := st.ExpiredArtifactBlobKeysByExpiry(ctx); err != nil {
		t.Fatalf("ExpiredArtifactBlobKeysByExpiry: %v", err)
	} else if len(keys) != 0 {
		t.Fatalf("expected 0 expired keys yet, got %v", keys)
	}

	// Force the expiring artifact's expires_at into the past.
	if _, err := st.pool.Exec(ctx,
		`UPDATE artifacts SET expires_at = now() - interval '1 hour' WHERE id=$1`, aExp.ID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	keys, err := st.ExpiredArtifactBlobKeysByExpiry(ctx)
	if err != nil {
		t.Fatalf("ExpiredArtifactBlobKeysByExpiry: %v", err)
	}
	if len(keys) != 1 || keys[0] != "job-x/a.tar.gz" {
		t.Fatalf("expected only the expiring blob key, got %v", keys)
	}

	n, err := st.DeleteExpiredArtifactsByExpiry(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredArtifactsByExpiry: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row deleted, got %d", n)
	}

	// The sibling with no expire_in survives.
	arts, err := st.ListArtifacts(ctx, "r", 0)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 1 || arts[0].ID != aKeep.ID {
		t.Fatalf("expected only the sibling to survive, got %+v", arts)
	}
}

func TestJUnitReportUpsert(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()

	jobs := []compiler.CompiledJob{
		{Name: "test", Stage: "test", StageIdx: 0, Script: "echo hi", Env: map[string]string{}},
	}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "r", Ref: "main", SHA: "abc", Config: "x",
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	_, pjobs, _ := st.GetPipeline(ctx, p.ID)
	jobID := pjobs[0].ID

	// No report yet.
	if _, err := st.GetJUnitReport(ctx, jobID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	r1 := proto.JUnitReport{JobID: jobID, Total: 3, Passed: 2, Failed: 1, Skipped: 0,
		DurationSeconds: 1.2, Failures: []proto.JUnitFailure{{Name: "fails_b", Type: "failure"}}}
	if err := st.SaveJUnitReport(ctx, r1); err != nil {
		t.Fatalf("SaveJUnitReport: %v", err)
	}
	got, err := st.GetJUnitReport(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJUnitReport: %v", err)
	}
	if got.Total != 3 || got.Failed != 1 || len(got.Failures) != 1 || got.Failures[0].Name != "fails_b" {
		t.Fatalf("report = %+v", got)
	}

	// Re-upload replaces (upsert on job_id).
	r2 := proto.JUnitReport{JobID: jobID, Total: 5, Passed: 5, Failed: 0, Failures: nil}
	if err := st.SaveJUnitReport(ctx, r2); err != nil {
		t.Fatalf("SaveJUnitReport replace: %v", err)
	}
	got, _ = st.GetJUnitReport(ctx, jobID)
	if got.Total != 5 || got.Failed != 0 || len(got.Failures) != 0 {
		t.Fatalf("replaced report = %+v", got)
	}
}
