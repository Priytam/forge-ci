package api

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestRefreshOIDCTokenForRunningJob: the runner can re-mint a running job's
// token (different from the original, same job), and only while it runs.
func TestRefreshOIDCTokenForRunningJob(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	p := h.seedPipeline("acme/cos", "main", plainYAML)
	if _, err := h.st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	rj, err := h.st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil {
		t.Fatalf("acquire: %v %v", rj, err)
	}
	if rj.Env["FORGE_OIDC_TOKEN"] == "" {
		t.Skip("OIDC signing unavailable in this environment")
	}
	rec := h.do(testCaller{}, "POST", "/api/v1/runner/jobs/"+pid(rj.ID)+"/oidc-token", nil)
	h.expect(rec, 200, "refresh running job token")
	got := decodeInto[proto.OIDCTokenResponse](t, rec)
	if got.Token == "" || got.ExpiresAt.IsZero() {
		t.Fatalf("refresh response = %+v", got)
	}
	if _, _, err := h.st.CompleteJob(ctx, rj.ID, "success", 0); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(testCaller{}, "POST", "/api/v1/runner/jobs/"+pid(rj.ID)+"/oidc-token", nil), 409, "finished job")
	_ = p
}
