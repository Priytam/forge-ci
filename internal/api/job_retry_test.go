package api

import (
	"context"
	"testing"
)

func TestRetryJobEndpoint(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	const repo = "acme/cos"
	if _, err := h.st.AddMember(ctx, repo, "dev@acme.io", "developer"); err != nil {
		t.Fatal(err)
	}
	dev := h.login("dev@acme.io")
	outsider := h.login("outsider@acme.io")
	p := h.seedPipeline(repo, "main", plainYAML)
	unit := h.jobID(p.ID, "unit")

	h.expect(h.do(dev, "POST", "/api/v1/jobs/"+pid(unit)+"/retry", nil), 409, "retry a job that has not failed")
	if _, err := h.db.Exec(ctx, `UPDATE jobs SET status='failed', finished_at=now() WHERE id=$1`, unit); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(outsider, "POST", "/api/v1/jobs/"+pid(unit)+"/retry", nil), 403, "outsider retry")
	rec := h.do(dev, "POST", "/api/v1/jobs/"+pid(unit)+"/retry", nil)
	h.expect(rec, 200, "developer retry")
	got := decodeInto[map[string]any](t, rec)
	if got["pipeline_id"].(float64) != float64(p.ID) || got["job_id"].(float64) == float64(unit) || got["retried"].(float64) != 1 {
		t.Errorf("retry response = %v", got)
	}
	h.expect(h.do(dev, "POST", "/api/v1/jobs/999999/retry", nil), 404, "missing job")
}
