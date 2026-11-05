package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestPeopleListAndRoleSwitch(t *testing.T) {
	h := newEnforcedHarness(t) // admin@acme.io is the ADMIN_EMAILS admin
	ctx := context.Background()
	root := h.login("admin@acme.io")
	bob := h.login("bob@acme.io")

	// The directory fills from sign-ins.
	for _, p := range []struct{ email, name string }{
		{"carol@acme.io", "Carol Jones"}, {"Alice@Acme.io", "Alice Smith"}, {"bob@acme.io", "Bob"}, {"dave@acme.io", ""},
	} {
		if err := h.st.RecordSignIn(ctx, p.email, p.name, "google"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // distinct sign-in times for ordering
	}
	// An empty name on a later sign-in keeps the one on file.
	if err := h.st.RecordSignIn(ctx, "alice@acme.io", "", "google"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddMember(ctx, "acme/web", "Bob@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}
	// Dave is an admin through an IdP group.
	if err := h.st.RecordUserGroups(ctx, "dave@acme.io", "oidc", []string{"Forge-Admins"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateGroupMapping(ctx, store.GroupMapping{Group: "forge-admins", PlatformAdmin: true}); err != nil {
		t.Fatal(err)
	}

	list := func(c testCaller, query string) ([]store.Person, string) {
		t.Helper()
		rec := h.do(c, "GET", "/api/v1/people"+query, nil)
		h.expect(rec, 200, "people "+query)
		return decodeInto[[]store.Person](t, rec), rec.Header().Get("X-Total-Count")
	}
	emails := func(ps []store.Person) string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Email+":"+p.AdminSource)
		}
		return strings.Join(out, ",")
	}

	h.expect(h.do(bob, "GET", "/api/v1/people", nil), 403, "non-admin lists people")

	// Most recent sign-in first; the env admin who never signed in comes last.
	all, total := list(root, "")
	if got := emails(all); got != "alice@acme.io:,dave@acme.io:group,bob@acme.io:,carol@acme.io:,admin@acme.io:env" || total != "5" {
		t.Fatalf("people = %s (total %s)", got, total)
	}
	byEmail := map[string]store.Person{}
	for _, p := range all {
		byEmail[p.Email] = p
	}
	if byEmail["alice@acme.io"].Name != "Alice Smith" {
		t.Errorf("alice name = %q", byEmail["alice@acme.io"].Name)
	}
	if rr := byEmail["bob@acme.io"].RepoRoles; len(rr) != 1 || rr[0] != (store.RepoRole{Repo: "acme/web", Role: "owner"}) {
		t.Errorf("bob repo roles = %+v", rr)
	}
	if byEmail["admin@acme.io"].LastSignInAt != nil {
		t.Errorf("env admin last sign-in = %v, want never", byEmail["admin@acme.io"].LastSignInAt)
	}
	if g := byEmail["dave@acme.io"].AdminGroups; len(g) != 1 || g[0] != "forge-admins" {
		t.Errorf("dave admin groups = %v", g)
	}

	// Search by name or email, filter to admins, page.
	if got, _ := list(root, "?q=SMITH"); emails(got) != "alice@acme.io:" {
		t.Errorf("search smith = %s", emails(got))
	}
	if got, _ := list(root, "?q=carol%40"); emails(got) != "carol@acme.io:" {
		t.Errorf("search carol@ = %s", emails(got))
	}
	if got, _ := list(root, "?q=%25"); len(got) != 0 {
		t.Errorf("search %% matched %s", emails(got))
	}
	if got, total := list(root, "?role=admin"); emails(got) != "dave@acme.io:group,admin@acme.io:env" || total != "2" {
		t.Errorf("admins = %s (total %s)", emails(got), total)
	}
	if got, total := list(root, "?limit=2&offset=2"); emails(got) != "bob@acme.io:,carol@acme.io:" || total != "5" {
		t.Errorf("page 2 = %s (total %s)", emails(got), total)
	}

	// Switch Bob to admin and back from the dropdown.
	h.expect(h.do(root, "PUT", "/api/v1/people/Bob@acme.io/platform-role", map[string]string{"role": "admin"}), 200, "bob → admin")
	if got, _ := list(bob, "?q=bob"); emails(got) != "bob@acme.io:ui" {
		t.Errorf("bob after grant = %s", emails(got))
	}
	h.expect(h.do(root, "PUT", "/api/v1/people/bob@acme.io/platform-role", map[string]string{"role": "admin"}), 409, "bob → admin twice")
	h.expect(h.do(root, "PUT", "/api/v1/people/bob@acme.io/platform-role", map[string]string{"role": "member"}), 200, "bob → member")
	h.expect(h.do(bob, "GET", "/api/v1/people", nil), 403, "bob no longer admin")

	// Locked rows and bad input.
	h.expect(h.do(root, "PUT", "/api/v1/people/admin@acme.io/platform-role", map[string]string{"role": "member"}), 409, "env admin → member")
	rec := h.do(root, "PUT", "/api/v1/people/dave@acme.io/platform-role", map[string]string{"role": "member"})
	h.expect(rec, 404, "group admin → member")
	if !strings.Contains(decodeInto[map[string]string](t, rec)["error"], "Sign-in & identity") {
		t.Errorf("group admin message = %s", rec.Body.String())
	}
	h.expect(h.do(root, "PUT", "/api/v1/people/bob@acme.io/platform-role", map[string]string{"role": "owner"}), 400, "bad role")
	h.expect(h.do(root, "PUT", "/api/v1/people/not-an-email/platform-role", map[string]string{"role": "admin"}), 400, "bad email")

	// Never with a token.
	rec = h.do(root, "POST", "/api/v1/api-tokens", map[string]any{"description": "ci", "no_expiry": true})
	h.expect(rec, 201, "root token")
	tok := testCaller{bearer: decodeInto[map[string]any](t, rec)["token"].(string)}
	h.expect(h.do(tok, "PUT", "/api/v1/people/bob@acme.io/platform-role", map[string]string{"role": "admin"}), 403, "role via token")
}
