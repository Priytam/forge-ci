package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestRetryFailedJobs: after an outage, retry every failed deploy whose log
// shows the same transient error since a time — previewed with dry_run,
// limited to repos the caller may operate, and audited.
func TestRetryFailedJobs(t *testing.T) {
	t.Setenv("LOG_BACKEND", "postgres")
	h := newEnforcedHarness(t)
	ctx := context.Background()
	h.srv = New(h.st, nil, logstore.Resolve(ctx, h.st, nil))
	dev := h.login("dev@acme.io")
	if _, err := h.st.AddMember(ctx, "acme/api", "dev@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddMember(ctx, "acme/secret", "owner@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}

	yml := "stages: [d]\njobs:\n  deploy-prod:\n    stage: d\n    script: [./deploy]\n"
	fail := func(repo, log string, ago time.Duration) int64 {
		p := h.seedPipeline(repo, "main", yml)
		var id int64
		_ = h.db.QueryRow(ctx, `SELECT id FROM jobs WHERE pipeline_id=$1`, p.ID).Scan(&id)
		_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='failed', finished_at=now() - $2::interval WHERE id=$1`, id, ago.String())
		_, _ = h.db.Exec(ctx, `UPDATE pipelines SET status='failed' WHERE id=$1`, p.ID)
		if err := h.st.AppendLog(ctx, id, log); err != nil {
			t.Fatal(err)
		}
		return id
	}
	transient := "deploying…\nError: dial tcp 10.0.0.5:5432: i/o timeout\n"
	a1 := fail("acme/api", transient, time.Hour)
	a2 := fail("acme/api", transient, 2*time.Hour)
	_ = fail("acme/api", "Error: image not found\n", time.Hour) // another error
	_ = fail("acme/api", transient, 30*time.Hour)               // before the window
	hidden := fail("acme/secret", transient, time.Hour)         // no role there

	type out struct {
		Matched []store.FailedJob `json:"matched"`
		Retried []bulkRetryDone   `json:"retried"`
		Skipped []bulkRetrySkip   `json:"skipped"`
	}
	body := map[string]any{"name": "deploy", "error": "I/O TIMEOUT", "since": "6h", "dry_run": true}
	rec := h.do(dev, "POST", "/api/v1/jobs/retry-failed", body)
	h.expect(rec, http.StatusOK, "dry run")
	dry := decodeInto[out](t, rec)
	if len(dry.Matched) != 3 || len(dry.Retried) != 0 || len(dry.Skipped) != 1 || dry.Skipped[0].JobID != hidden {
		t.Fatalf("dry run = %+v", dry)
	}
	var st string
	_ = h.db.QueryRow(ctx, `SELECT status FROM jobs WHERE id=$1`, a1).Scan(&st)
	if st != "failed" {
		t.Fatalf("dry run changed job %d to %s", a1, st)
	}

	body["dry_run"] = false
	rec = h.do(dev, "POST", "/api/v1/jobs/retry-failed", body)
	h.expect(rec, http.StatusOK, "retry")
	done := decodeInto[out](t, rec)
	got := map[int64]bool{}
	for _, d := range done.Retried {
		got[d.JobID] = d.NewJobID != 0
	}
	if len(done.Retried) != 2 || !got[a1] || !got[a2] {
		t.Fatalf("retried = %+v", done)
	}
	var audits int
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='job.retry.bulk'`).Scan(&audits)
	if audits != 1 {
		t.Errorf("bulk audit entries = %d", audits)
	}
	// Retried jobs are superseded: a second run finds nothing to do.
	rec = h.do(dev, "POST", "/api/v1/jobs/retry-failed", body)
	if again := decodeInto[out](t, rec); len(again.Retried) != 0 {
		t.Errorf("second run retried %+v", again.Retried)
	}

	h.expect(h.do(dev, "POST", "/api/v1/jobs/retry-failed", map[string]any{"name": "deploy"}), http.StatusBadRequest, "since is required")
	h.expect(h.do(dev, "POST", "/api/v1/jobs/retry-failed", map[string]any{"since": "1h"}), http.StatusBadRequest, "a filter is required")
	h.expect(h.do(dev, "POST", "/api/v1/jobs/retry-failed", map[string]any{"name": "x", "since": "yesterday"}), http.StatusBadRequest, "bad since")
}
