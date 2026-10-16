package api

import (
	"context"
	"testing"
)

// TestServiceAccounts: an admin creates a service account and a token for it;
// the token acts as svc:<name> with the rights its repo membership gives (not
// a person's), it can't manage service accounts itself, and disabling the
// account stops the token at once.
func TestServiceAccounts(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	dev := h.login("dev@acme.io")

	h.expect(h.do(dev, "POST", "/api/v1/service-accounts", map[string]any{"name": "deployer"}), 403, "dev creates SA")
	h.expect(h.do(admin, "POST", "/api/v1/service-accounts", map[string]any{"name": "Bad Name!"}), 400, "invalid name")
	rec := h.do(admin, "POST", "/api/v1/service-accounts", map[string]any{"name": "deployer", "description": "GitHub Actions deploys"})
	h.expect(rec, 201, "admin creates SA")
	sa := decodeInto[map[string]any](t, rec)
	if sa["identity"] != "svc:deployer" {
		t.Fatalf("service account = %v", sa)
	}
	id := pid(int64(sa["id"].(float64)))
	h.expect(h.do(admin, "POST", "/api/v1/service-accounts", map[string]any{"name": "deployer"}), 409, "duplicate")

	rec = h.do(admin, "POST", "/api/v1/service-accounts/"+id+"/tokens", map[string]any{"description": "ci", "no_expiry": true})
	h.expect(rec, 201, "create SA token")
	tok := testCaller{bearer: decodeInto[tokenResp](t, rec).Token}

	// It is svc:deployer, not a person, and not an admin.
	rec = h.do(tok, "GET", "/api/v1/auth/me", nil)
	h.expect(rec, 200, "me as SA")
	me := decodeInto[map[string]any](t, rec)
	if me["email"] != "svc:deployer" || me["provider"] != serviceAccountProvider || me["is_admin"] != false {
		t.Fatalf("me = %v", me)
	}
	h.expect(h.do(tok, "POST", "/api/v1/runner-tokens", nil), 403, "SA (non-admin) creates runner token")
	h.expect(h.do(tok, "POST", "/api/v1/api-tokens", map[string]any{"no_expiry": true}), 403, "SA mints personal token")

	// Repo access comes from membership, like a person.
	const repo = "acme/cos"
	if _, err := h.st.AddMember(ctx, repo, "dev@acme.io", "developer"); err != nil {
		t.Fatal(err)
	}
	p := h.seedPipeline(repo, "main", plainYAML)
	h.expect(h.do(tok, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil), 403, "SA without membership")
	if _, err := h.st.AddMember(ctx, repo, "svc:deployer", "developer"); err != nil {
		t.Fatal(err)
	}
	rec = h.do(tok, "POST", "/api/v1/pipelines/"+pid(p.ID)+"/retry", nil)
	h.expect(rec, 201, "SA with membership")

	// An admin service account gets admin rights but still can't manage SAs.
	h.expect(h.do(admin, "PATCH", "/api/v1/service-accounts/"+id, map[string]any{"is_admin": true}), 200, "make SA admin")
	h.expect(h.do(tok, "POST", "/api/v1/runner-tokens", nil), 201, "admin SA creates runner token")
	h.expect(h.do(tok, "POST", "/api/v1/service-accounts", map[string]any{"name": "other"}), 403, "SA creates SA")

	// Disabling stops the token immediately; listing shows the account.
	h.expect(h.do(admin, "PATCH", "/api/v1/service-accounts/"+id, map[string]any{"disabled": true}), 200, "disable SA")
	h.expect(h.do(tok, "GET", "/api/v1/pipelines", nil), 401, "disabled SA token")
	list := decodeInto[[]map[string]any](t, h.do(admin, "GET", "/api/v1/service-accounts", nil))
	if len(list) != 1 || list[0]["disabled"] != true || list[0]["active_tokens"].(float64) != 1 {
		t.Errorf("service accounts = %v", list)
	}
	h.expect(h.do(admin, "DELETE", "/api/v1/service-accounts/"+id, nil), 204, "delete SA")
	h.expect(h.do(admin, "GET", "/api/v1/service-accounts/"+id+"/tokens", nil), 404, "tokens of deleted SA")
}
