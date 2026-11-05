package api

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

type adminsResp struct {
	Admins []struct {
		Email   string `json:"email"`
		Source  string `json:"source"`
		AddedBy string `json:"added_by"`
	} `json:"admins"`
	AdminGroups []struct {
		Group string `json:"group"`
	} `json:"admin_groups"`
	Me string `json:"me"`
}

func TestPlatformAdminsFromUI(t *testing.T) {
	h := newEnforcedHarness(t) // admin@acme.io is the ADMIN_EMAILS admin
	root := h.login("admin@acme.io")
	bob := h.login("bob@acme.io")
	isAdmin := func(c testCaller) bool {
		t.Helper()
		rec := h.do(c, "GET", "/api/v1/auth/me", nil)
		h.expect(rec, 200, "me")
		return decodeInto[map[string]any](t, rec)["is_admin"] == true
	}

	// Bob starts as a plain user.
	if isAdmin(bob) {
		t.Fatal("bob is admin before any grant")
	}
	h.expect(h.do(bob, "GET", "/api/v1/platform-admins", nil), 403, "non-admin lists admins")
	h.expect(h.do(bob, "POST", "/api/v1/platform-admins", map[string]string{"email": "bob@acme.io"}), 403, "non-admin grants")

	// Bad input.
	for _, bad := range []string{"", "not an email", "Bob <bob@acme.io>", "a@b.io,c@d.io"} {
		h.expect(h.do(root, "POST", "/api/v1/platform-admins", map[string]string{"email": bad}), 400, "bad email "+bad)
	}
	h.expect(h.do(root, "POST", "/api/v1/platform-admins", map[string]string{"email": "Admin@Acme.io"}), 409, "env admin again")

	// Grant (case-insensitive) and it takes effect at once.
	rec := h.do(root, "POST", "/api/v1/platform-admins", map[string]string{"email": " Bob@Acme.io "})
	h.expect(rec, 201, "grant bob")
	if !isAdmin(bob) {
		t.Fatal("bob is not admin after the grant")
	}
	h.expect(h.do(root, "POST", "/api/v1/platform-admins", map[string]string{"email": "bob@acme.io"}), 409, "grant twice")

	rec = h.do(bob, "GET", "/api/v1/platform-admins", nil)
	h.expect(rec, 200, "bob lists admins")
	got := decodeInto[adminsResp](t, rec)
	if len(got.Admins) != 2 || got.Admins[0].Email != "admin@acme.io" || got.Admins[0].Source != "env" ||
		got.Admins[1].Email != "bob@acme.io" || got.Admins[1].Source != "ui" || got.Admins[1].AddedBy != "admin@acme.io" {
		t.Fatalf("admins = %+v", got.Admins)
	}
	if got.Me != "bob@acme.io" {
		t.Errorf("me = %q", got.Me)
	}

	// Removal guards.
	h.expect(h.do(bob, "DELETE", "/api/v1/platform-admins/bob@acme.io", nil), 409, "remove yourself")
	h.expect(h.do(bob, "DELETE", "/api/v1/platform-admins/admin@acme.io", nil), 409, "remove env admin")
	h.expect(h.do(root, "DELETE", "/api/v1/platform-admins/nobody@acme.io", nil), 404, "remove unknown")

	// An API token can't grant or revoke admin, even an admin's.
	rec = h.do(root, "POST", "/api/v1/api-tokens", map[string]any{"description": "ci", "no_expiry": true})
	h.expect(rec, 201, "root token")
	tok := testCaller{bearer: decodeInto[map[string]any](t, rec)["token"].(string)}
	h.expect(h.do(tok, "POST", "/api/v1/platform-admins", map[string]string{"email": "eve@acme.io"}), 403, "grant via token")
	h.expect(h.do(tok, "DELETE", "/api/v1/platform-admins/bob@acme.io", nil), 403, "revoke via token")

	// Revoke: bob loses admin on his next request.
	h.expect(h.do(root, "DELETE", "/api/v1/platform-admins/BOB@acme.io", nil), 204, "revoke bob")
	if isAdmin(bob) {
		t.Fatal("bob is still admin after revoke")
	}
	h.expect(h.do(bob, "GET", "/api/v1/platform-admins", nil), 403, "revoked bob lists admins")

	// Both changes are audited.
	var n int
	if err := h.db.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action IN ('platform-admin.add','platform-admin.remove') AND target='bob@acme.io'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("audit entries = %d, want 2", n)
	}
}

// Without ADMIN_EMAILS or an admin group, the last UI admin can't be removed:
// enabling SSO afterwards would lock everyone out of the admin pages.
func TestPlatformAdminsKeepsLastAdmin(t *testing.T) {
	st, db := newCFRStore(t, "")
	t.Setenv("ADMIN_EMAILS", "")
	h := &apiHarness{t: t, st: st, db: db, srv: New(st, nil, nil)} // open mode
	open := testCaller{}
	h.expect(h.do(open, "POST", "/api/v1/platform-admins", map[string]string{"email": "carol@acme.io"}), 201, "grant carol")
	h.expect(h.do(open, "DELETE", "/api/v1/platform-admins/carol@acme.io", nil), 409, "remove last admin")
	h.expect(h.do(open, "POST", "/api/v1/platform-admins", map[string]string{"email": "dave@acme.io"}), 201, "grant dave")
	h.expect(h.do(open, "DELETE", "/api/v1/platform-admins/carol@acme.io", nil), 204, "remove carol with dave left")

	// An IdP group mapped to platform admin also counts as an admin left.
	if _, err := st.CreateGroupMapping(context.Background(), store.GroupMapping{Group: "forge-admins", PlatformAdmin: true}); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(open, "DELETE", "/api/v1/platform-admins/dave@acme.io", nil), 204, "remove dave with an admin group")
}
