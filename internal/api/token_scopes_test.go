package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestAPITokenScopesForAutomation: an admin's scripts run on API tokens, not
// a browser session. An admin-scoped token does admin automation (Live
// targets and policy, runners, retries); an operate token retries but cannot
// do admin-only things; a read token only reads; a revoked token stops; and
// a stale browser cookie sent along with a token does not get in the way.
func TestAPITokenScopesForAutomation(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")

	mint := func(scope string) (string, int64) {
		rec := h.do(admin, "POST", "/api/v1/api-tokens", map[string]any{"description": scope, "scope": scope})
		h.expect(rec, http.StatusCreated, "mint "+scope)
		tok := decodeInto[store.APIToken](t, rec)
		if tok.Scope != scope {
			t.Fatalf("scope = %q, want %q", tok.Scope, scope)
		}
		return tok.Token, tok.ID
	}
	adminTok, _ := mint("admin")
	operateTok, _ := mint("operate")
	readTok, readID := mint("read")
	h.expect(h.do(admin, "POST", "/api/v1/api-tokens", map[string]any{"scope": "root"}), http.StatusBadRequest, "unknown scope")

	// A failed deploy job to retry.
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	failed := func() int64 {
		var id int64
		_ = h.db.QueryRow(ctx, `SELECT id FROM jobs WHERE pipeline_id=$1 ORDER BY id DESC LIMIT 1`, p.ID).Scan(&id)
		_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='failed', finished_at=now() WHERE id=$1`, id)
		_, _ = h.db.Exec(ctx, `UPDATE pipelines SET status='failed' WHERE id=$1`, p.ID)
		return id
	}

	at := testCaller{bearer: adminTok}
	rec := h.do(at, "POST", "/api/v1/live/targets", map[string]any{"name": "gke-travafa", "environment": "staging"})
	h.expect(rec, http.StatusCreated, "admin token creates a Live target")
	created := decodeInto[struct {
		Target store.LiveTarget `json:"target"`
	}](t, rec)
	target := "/api/v1/live/targets/" + pid(created.Target.ID)
	h.expect(h.do(at, "PATCH", target, map[string]any{"namespace_environments": map[string]string{"travafa-prod": "production"}}), http.StatusOK, "admin token edits a target")
	h.expect(h.do(at, "POST", "/api/v1/runners/no-such-runner/drain", nil), http.StatusNotFound, "admin token reaches runner admin")
	h.expect(h.do(at, "POST", "/api/v1/jobs/"+itoa64(failed())+"/retry", nil), http.StatusOK, "admin token retries")

	ot := testCaller{bearer: operateTok}
	h.expect(h.do(ot, "POST", "/api/v1/jobs/"+itoa64(failed())+"/retry", nil), http.StatusOK, "operate token retries")
	h.expect(h.do(ot, "PATCH", target, map[string]any{"environment": "x"}), http.StatusForbidden, "operate token is not admin")
	h.expect(h.do(ot, "POST", "/api/v1/runners/no-such-runner/drain", nil), http.StatusForbidden, "operate token cannot drain")

	rt := testCaller{bearer: readTok}
	h.expect(h.do(rt, "GET", "/api/v1/live/targets", nil), http.StatusOK, "read token reads")
	h.expect(h.do(rt, "POST", "/api/v1/jobs/"+itoa64(failed())+"/retry", nil), http.StatusForbidden, "read token cannot retry")

	// A script with an expired browser cookie and a token: the token wins,
	// even without an Origin header (CSRF does not apply to bearer tokens).
	req := httptest.NewRequest("POST", "/api/v1/jobs/"+itoa64(failed())+"/retry", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired-session"})
	req.Header.Set("Authorization", "Bearer "+operateTok)
	rr := httptest.NewRecorder()
	h.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stale cookie + token: %d %s", rr.Code, rr.Body.String())
	}

	// Revocation stops a token at once.
	h.expect(h.do(admin, "POST", "/api/v1/api-tokens/"+itoa64(readID)+"/revoke", nil), http.StatusNoContent, "revoke")
	if rec := h.do(rt, "GET", "/api/v1/live/targets", nil); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authentication required") {
		t.Fatalf("revoked token: %d %s", rec.Code, rec.Body.String())
	}
}
