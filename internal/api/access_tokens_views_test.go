package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// ---- shared helpers ----

// testCaller is how a request authenticates: an SSO cookie session, a
// personal API token, or nothing.
type testCaller struct {
	cookie string
	bearer string
}

type apiHarness struct {
	t   *testing.T
	st  *store.Store
	db  *pgx.Conn
	srv *Server
}

// newEnforcedHarness returns a server with SSO enforced and admin@acme.io as
// the only platform admin.
func newEnforcedHarness(t *testing.T) *apiHarness {
	t.Helper()
	st, db := newCFRStore(t, "")
	t.Setenv("ADMIN_EMAILS", "admin@acme.io")
	ctx := context.Background()
	if err := st.UpsertSSOProvider(ctx, store.SSOProvider{
		Provider: "google", Enabled: true, ClientID: "test-client",
	}); err != nil {
		t.Fatalf("enable SSO: %v", err)
	}
	return &apiHarness{t: t, st: st, db: db, srv: New(st, nil, nil)}
}

func (h *apiHarness) login(email string) testCaller {
	h.t.Helper()
	sess, err := h.st.CreateSession(context.Background(), email, email, "google", time.Hour)
	if err != nil {
		h.t.Fatalf("CreateSession: %v", err)
	}
	return testCaller{cookie: sess.Token}
}

func (h *apiHarness) do(c testCaller, method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", externalURL()) // same-origin for the CSRF check
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookie})
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec
}

func (h *apiHarness) expect(rec *httptest.ResponseRecorder, want int, what string) {
	h.t.Helper()
	if rec.Code != want {
		h.t.Fatalf("%s: status = %d, want %d; body = %s", what, rec.Code, want, rec.Body.String())
	}
}

const manualYAML = `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [echo build]
  ship:
    stage: deploy
    script: [echo ship]
    rules:
      - when: manual
`

// seedPipeline creates a pipeline for repo/ref straight through the store.
func (h *apiHarness) seedPipeline(repo, ref, yml string) *proto.Pipeline {
	h.t.Helper()
	ctx := context.Background()
	jobs, err := compiler.Compile(yml, ref, compiler.SourceAPI, h.st.TemplateResolver(ctx, repo))
	if err != nil {
		h.t.Fatalf("compile: %v", err)
	}
	p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: ref, SHA: "abc123", Config: yml,
	}, jobs, nil, false, false)
	if err != nil {
		h.t.Fatalf("CreatePipeline: %v", err)
	}
	return p
}

func (h *apiHarness) jobID(pipelineID int64, name string) int64 {
	h.t.Helper()
	var id int64
	if err := h.db.QueryRow(context.Background(),
		`SELECT id FROM jobs WHERE pipeline_id=$1 AND name=$2`, pipelineID, name).Scan(&id); err != nil {
		h.t.Fatalf("job id: %v", err)
	}
	return id
}

func pid(id int64) string { return strconv.FormatInt(id, 10) }

// ---- 1. retry / play permissions ----

// TestDevelopersCanRetryAndPlay: a repo member with the developer role may
// retry a pipeline and play a manual job; a signed-in non-member may not;
// cancel and settings stay admin-only.
func TestDevelopersCanRetryAndPlay(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	const repo = "acme/guarded"
	if _, err := h.st.AddMember(ctx, repo, "Dev@Acme.io", "developer"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	dev := h.login("dev@acme.io") // case differs from the member row on purpose
	outsider := h.login("outsider@acme.io")
	admin := h.login("admin@acme.io")

	p := h.seedPipeline(repo, "main", manualYAML)
	ship := h.jobID(p.ID, "ship")
	// Put the job where a manual job waits for play (the scheduler normally
	// parks it in 'blocked' once its stage is reached).
	if _, err := h.db.Exec(ctx, `UPDATE jobs SET status='blocked', manual=TRUE WHERE id=$1`, ship); err != nil {
		t.Fatal(err)
	}

	h.expect(h.do(outsider, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil), 403, "outsider retry")
	h.expect(h.do(outsider, "POST", "/api/v1/jobs/"+pid(ship)+"/play", nil), 403, "outsider play")

	// Play first: a retry on the same ref may auto-cancel the original run.
	h.expect(h.do(dev, "POST", "/api/v1/jobs/"+pid(ship)+"/play", nil), 200, "developer play")
	h.expect(h.do(dev, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil), 201, "developer retry")

	// Still admin-only.
	h.expect(h.do(dev, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/cancel", nil), 403, "developer cancel")
	h.expect(h.do(dev, "POST", "/api/v1/variables",
		map[string]any{"repo": repo, "key": "K", "value": "v"}), 403, "developer variable")
	h.expect(h.do(dev, "POST", "/api/v1/members",
		map[string]any{"repo": repo, "username": "x@acme.io", "role": "admin"}), 403, "developer add member")
	h.expect(h.do(dev, "POST", "/api/v1/runner-tokens", nil), 403, "developer runner token")
	h.expect(h.do(admin, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil), 201, "admin retry")

	// Unknown ids are 404, not 403, regardless of role.
	h.expect(h.do(dev, "POST", "/api/v1/pipelines/999999/retry", nil), 404, "missing pipeline")
	h.expect(h.do(dev, "POST", "/api/v1/jobs/999999/play", nil), 404, "missing job")

	// The permissions hint the UI uses agrees with the server.
	var perms map[string]bool
	rec := h.do(dev, "GET", "/api/v1/permissions?repo="+repo, nil)
	h.expect(rec, 200, "permissions")
	_ = json.Unmarshal(rec.Body.Bytes(), &perms)
	if !perms["can_run"] || perms["can_cancel"] || perms["is_admin"] {
		t.Errorf("developer permissions = %v, want can_run only", perms)
	}
	rec = h.do(outsider, "GET", "/api/v1/permissions?repo="+repo, nil)
	perms = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &perms)
	if perms["can_run"] {
		t.Errorf("outsider permissions = %v, want can_run false", perms)
	}
}

// TestRetryOnRepoWithoutMembers: a repo with no members is in bootstrap mode,
// so any signed-in user may retry there (same rule as approvals).
func TestRetryOnRepoWithoutMembers(t *testing.T) {
	h := newEnforcedHarness(t)
	p := h.seedPipeline("acme/open", "main", manualYAML)
	h.expect(h.do(h.login("someone@acme.io"), "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil),
		201, "retry on member-less repo")
	h.expect(h.do(testCaller{}, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil),
		401, "anonymous retry")
}

// ---- 2. personal API tokens ----

type tokenResp struct {
	ID         int64      `json:"id"`
	Token      string     `json:"token"`
	Owner      string     `json:"owner"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Status     string     `json:"status"`
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

func TestAPITokensActOnBehalfOfUser(t *testing.T) {
	h := newEnforcedHarness(t)
	dev := h.login("dev@acme.io")
	admin := h.login("admin@acme.io")

	// A non-expiring token, allowed by the default policy.
	rec := h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"description": "ci", "no_expiry": true})
	h.expect(rec, 201, "create no-expiry token")
	forever := decodeInto[tokenResp](t, rec)
	if !strings.HasPrefix(forever.Token, store.APITokenPrefix) || forever.ExpiresAt != nil {
		t.Fatalf("token = %+v, want fci_ prefix and no expiry", forever)
	}

	// A 30-day token.
	rec = h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"expires_in_days": 30})
	h.expect(rec, 201, "create 30-day token")
	month := decodeInto[tokenResp](t, rec)
	if month.ExpiresAt == nil || month.ExpiresAt.Sub(time.Now()) < 29*24*time.Hour {
		t.Fatalf("30-day token expires_at = %v", month.ExpiresAt)
	}

	// The token authenticates as its owner.
	tok := testCaller{bearer: forever.Token}
	rec = h.do(tok, "GET", "/api/v1/auth/me", nil)
	h.expect(rec, 200, "me via token")
	if me := decodeInto[map[string]any](t, rec); me["email"] != "dev@acme.io" || me["provider"] != apiTokenProvider {
		t.Fatalf("me via token = %v", me)
	}
	h.expect(h.do(testCaller{bearer: "fci_nottoken"}, "GET", "/api/v1/pipelines", nil), 401, "bogus token")

	// ...with the owner's rights, not more.
	h.expect(h.do(tok, "POST", "/api/v1/runner-tokens", nil), 403, "dev token creating runner token")
	// A token cannot mint more tokens.
	h.expect(h.do(tok, "POST", "/api/v1/api-tokens", map[string]any{"no_expiry": true}), 403, "token minting token")

	// The list shows last-used for the token we just used, and never the secret.
	rec = h.do(dev, "GET", "/api/v1/api-tokens", nil)
	h.expect(rec, 200, "list tokens")
	list := decodeInto[[]tokenResp](t, rec)
	if len(list) != 2 {
		t.Fatalf("listed %d tokens, want 2", len(list))
	}
	for _, l := range list {
		if l.Token != "" {
			t.Fatal("list leaked a raw token")
		}
		if l.ID == forever.ID && (l.LastUsedAt == nil || l.ExpiresAt != nil || l.Status != "active") {
			t.Errorf("no-expiry token in list = %+v, want last_used set, no expiry, active", l)
		}
	}
	// Other users don't see dev's tokens; admins can with ?all=1.
	other := h.login("other@acme.io")
	if got := decodeInto[[]tokenResp](t, h.do(other, "GET", "/api/v1/api-tokens", nil)); len(got) != 0 {
		t.Errorf("other user sees %d tokens", len(got))
	}
	h.expect(h.do(dev, "GET", "/api/v1/api-tokens?all=1", nil), 403, "non-admin all")
	if got := decodeInto[[]tokenResp](t, h.do(admin, "GET", "/api/v1/api-tokens?all=1", nil)); len(got) != 2 {
		t.Errorf("admin all = %d tokens, want 2", len(got))
	}

	// Revoke: another user can't, the owner can, and the token stops working.
	h.expect(h.do(other, "POST", "/api/v1/api-tokens/"+pid(month.ID)+"/revoke", nil), 404, "other revoke")
	h.expect(h.do(dev, "POST", "/api/v1/api-tokens/"+pid(month.ID)+"/revoke", nil), 204, "owner revoke")
	h.expect(h.do(testCaller{bearer: month.Token}, "GET", "/api/v1/pipelines", nil), 401, "revoked token")
}

func TestAPITokenPolicy(t *testing.T) {
	h := newEnforcedHarness(t)
	dev := h.login("dev@acme.io")
	admin := h.login("admin@acme.io")

	rec := h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"no_expiry": true})
	h.expect(rec, 201, "create no-expiry token")
	forever := decodeInto[tokenResp](t, rec)
	h.expect(h.do(testCaller{bearer: forever.Token}, "GET", "/api/v1/pipelines", nil), 200, "token before policy")

	// Only admins set the policy.
	h.expect(h.do(dev, "PUT", "/api/v1/api-token-policy",
		map[string]any{"allow_no_expiry": false}), 403, "dev sets policy")

	// Turning off non-expiring tokens blocks new ones AND disables existing ones.
	h.expect(h.do(admin, "PUT", "/api/v1/api-token-policy",
		map[string]any{"allow_no_expiry": false, "max_lifetime_days": 0}), 200, "disable no-expiry")
	h.expect(h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"no_expiry": true}), 400, "no-expiry when disabled")
	h.expect(h.do(testCaller{bearer: forever.Token}, "GET", "/api/v1/pipelines", nil), 401, "existing no-expiry token")
	if list := decodeInto[[]tokenResp](t, h.do(dev, "GET", "/api/v1/api-tokens", nil)); list[0].Status != "disabled" {
		t.Errorf("status = %q, want disabled", list[0].Status)
	}

	// A lifetime cap rejects longer tokens and clamps the default.
	h.expect(h.do(admin, "PUT", "/api/v1/api-token-policy",
		map[string]any{"allow_no_expiry": false, "max_lifetime_days": 30}), 200, "cap 30 days")
	h.expect(h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"expires_in_days": 90}), 400, "90 days over cap")
	rec = h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{})
	h.expect(rec, 201, "default lifetime under cap")
	if tr := decodeInto[tokenResp](t, rec); tr.ExpiresAt == nil || tr.ExpiresAt.Sub(time.Now()) > 31*24*time.Hour {
		t.Errorf("default token expires_at = %v, want clamped to 30 days", tr.ExpiresAt)
	}

	// Re-allowing brings the old token back (it was never revoked).
	h.expect(h.do(admin, "PUT", "/api/v1/api-token-policy",
		map[string]any{"allow_no_expiry": true, "max_lifetime_days": 0}), 200, "re-allow")
	h.expect(h.do(testCaller{bearer: forever.Token}, "GET", "/api/v1/pipelines", nil), 200, "token after re-allow")

	// An expired token is rejected.
	rec = h.do(dev, "POST", "/api/v1/api-tokens", map[string]any{"expires_in_days": 1})
	short := decodeInto[tokenResp](t, rec)
	if _, err := h.db.Exec(context.Background(),
		`UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id=$1`, short.ID); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(testCaller{bearer: short.Token}, "GET", "/api/v1/pipelines", nil), 401, "expired token")
}

func TestAPITokenNeedsSSO(t *testing.T) {
	st, _ := newCFRStore(t, "")
	h := &apiHarness{t: t, st: st, srv: New(st, nil, nil)}
	h.expect(h.do(testCaller{}, "POST", "/api/v1/api-tokens", map[string]any{"no_expiry": true}),
		400, "token in open mode")
}

// ---- 3. filters and saved views ----

const plainYAML = `
stages: [test]
jobs:
  unit:
    stage: test
    script: [echo ok]
`

func TestPipelineListFilters(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	u := h.login("dev@acme.io")

	a1 := h.seedPipeline("acme/a", "main", plainYAML)
	a2 := h.seedPipeline("acme/a", "feature", plainYAML)
	b1 := h.seedPipeline("acme/b", "main", plainYAML)
	c1 := h.seedPipeline("acme/c", "main", plainYAML)

	exec := func(q string, args ...any) {
		if _, err := h.db.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE jobs SET status='failed', runner_id='runner-1', environment='staging' WHERE pipeline_id=$1`, a1.ID)
	exec(`UPDATE jobs SET status='success', runner_id='runner-2' WHERE pipeline_id=$1`, a2.ID)
	exec(`UPDATE jobs SET status='failed', allow_failure=TRUE, runner_id='runner-1', environment='production' WHERE pipeline_id=$1`, b1.ID)
	exec(`UPDATE jobs SET status='running', runner_id='runner-2' WHERE pipeline_id=$1`, c1.ID)

	ids := func(query string) []int64 {
		t.Helper()
		rec := h.do(u, "GET", "/api/v1/pipelines?"+query, nil)
		h.expect(rec, 200, "list "+query)
		var out []int64
		for _, p := range decodeInto[[]proto.Pipeline](t, rec) {
			out = append(out, p.ID)
		}
		return out
	}
	same := func(query string, want ...int64) {
		t.Helper()
		got := ids(query)
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", query, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", query, got, want)
				return
			}
		}
	}

	same("repo=acme/a", a2.ID, a1.ID)
	same("repo=acme/a&repo=acme/b", b1.ID, a2.ID, a1.ID)
	same("repo=acme/a,acme/c", c1.ID, a2.ID, a1.ID)
	same("ref=feature", a2.ID)
	same("status=failed", a1.ID) // b1's failure is allowed, so b1 is success
	same("status=success", b1.ID, a2.ID)
	same("status=running,failed", c1.ID, a1.ID)
	same("runner=runner-1", b1.ID, a1.ID)
	same("environment=production", b1.ID)
	same("repo=acme/a&runner=runner-1&status=failed", a1.ID)
	same("repo=acme/zzz")
	h.expect(h.do(u, "GET", "/api/v1/pipelines?status=bogus", nil), 400, "unknown status")

	// Paging still reports totals for the filtered set.
	rec := h.do(u, "GET", "/api/v1/pipelines?runner=runner-2&limit=1", nil)
	if rec.Header().Get("X-Total-Count") != "2" || rec.Header().Get("X-Has-More") != "true" {
		t.Errorf("headers total=%s more=%s, want 2/true",
			rec.Header().Get("X-Total-Count"), rec.Header().Get("X-Has-More"))
	}

	// Facets offer the values seen.
	rec = h.do(u, "GET", "/api/v1/pipeline-facets", nil)
	h.expect(rec, 200, "facets")
	f := decodeInto[store.PipelineFacets](t, rec)
	if strings.Join(f.Environments, ",") != "production,staging" ||
		strings.Join(f.Runners, ",") != "runner-1,runner-2" || len(f.Repos) != 3 {
		t.Errorf("facets = %+v", f)
	}
	rec = h.do(u, "GET", "/api/v1/pipeline-facets?repo=acme/b", nil)
	if f := decodeInto[store.PipelineFacets](t, rec); strings.Join(f.Environments, ",") != "production" {
		t.Errorf("facets for acme/b environments = %v", f.Environments)
	}
}

func TestPipelineSavedViews(t *testing.T) {
	h := newEnforcedHarness(t)
	alice := h.login("alice@acme.io")
	bob := h.login("bob@acme.io")
	admin := h.login("admin@acme.io")

	filters := map[string]any{"repos": []string{"acme/a", "acme/b"}, "statuses": []string{"failed"}, "runner": "runner-1"}
	rec := h.do(alice, "POST", "/api/v1/pipeline-views", map[string]any{"name": "My services", "filters": filters})
	h.expect(rec, 201, "create private view")
	private := decodeInto[store.PipelineView](t, rec)
	if private.Owner != "alice@acme.io" || len(private.Filters.Repos) != 2 || private.Filters.Runner != "runner-1" {
		t.Fatalf("view = %+v", private)
	}
	h.expect(h.do(alice, "POST", "/api/v1/pipeline-views", map[string]any{"name": "My services", "filters": filters}),
		409, "duplicate name")
	h.expect(h.do(alice, "POST", "/api/v1/pipeline-views",
		map[string]any{"name": "bad", "filters": map[string]any{"statuses": []string{"nope"}}}), 400, "bad status")
	h.expect(h.do(alice, "POST", "/api/v1/pipeline-views", map[string]any{"name": " "}), 400, "empty name")

	rec = h.do(alice, "POST", "/api/v1/pipeline-views",
		map[string]any{"name": "Team board", "filters": filters, "shared": true})
	h.expect(rec, 201, "create shared view")
	shared := decodeInto[store.PipelineView](t, rec)

	names := func(c testCaller) string {
		var out []string
		for _, v := range decodeInto[[]store.PipelineView](t, h.do(c, "GET", "/api/v1/pipeline-views", nil)) {
			out = append(out, v.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(alice); got != "My services,Team board" {
		t.Errorf("alice sees %q", got)
	}
	if got := names(bob); got != "Team board" {
		t.Errorf("bob sees %q, want only the shared view", got)
	}

	// Only the owner (or an admin) may change a view.
	h.expect(h.do(bob, "PUT", "/api/v1/pipeline-views/"+pid(shared.ID),
		map[string]any{"name": "hijack", "filters": filters}), 403, "bob edits shared view")
	h.expect(h.do(bob, "DELETE", "/api/v1/pipeline-views/"+pid(private.ID), nil), 404, "bob deletes private view")
	rec = h.do(alice, "PUT", "/api/v1/pipeline-views/"+pid(private.ID),
		map[string]any{"name": "Renamed", "filters": map[string]any{"environment": "production"}, "shared": false})
	h.expect(rec, 200, "alice edits own view")
	if v := decodeInto[store.PipelineView](t, rec); v.Name != "Renamed" || v.Filters.Environment != "production" || len(v.Filters.Repos) != 0 {
		t.Errorf("updated view = %+v", v)
	}
	h.expect(h.do(admin, "DELETE", "/api/v1/pipeline-views/"+pid(shared.ID), nil), 204, "admin deletes shared view")
	if got := names(bob); got != "" {
		t.Errorf("bob sees %q after delete", got)
	}
}
