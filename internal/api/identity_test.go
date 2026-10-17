package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// fakeIdP is a minimal OpenID Connect provider: discovery, a token endpoint
// that checks PKCE, and userinfo with groups.
type fakeIdP struct {
	srv       *httptest.Server
	challenge string
	nonce     string
	aud       string
	groups    []string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{aud: "forge-client", groups: []string{"platform-admins", "shop-devs"}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "userinfo_endpoint": f.srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("code") != "the-code" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error_description":"bad pkce or code"}`))
			return
		}
		claims, _ := json.Marshal(map[string]any{
			"iss": f.srv.URL, "aud": f.aud, "sub": "u-123", "exp": time.Now().Add(time.Hour).Unix(),
			"nonce": f.nonce, "email": "dev@acme.io", "email_verified": true, "name": "Dev One",
		})
		idt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "id_token": idt})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "u-123", "groups": f.groups})
	})
	return f
}

// signIn walks login → (IdP) → callback and returns the session cookie.
func (f *fakeIdP) signIn(t *testing.T, srv *Server) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/auth/login/oidc", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), f.srv.URL+"/authorize") || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "forge-client" {
		t.Fatalf("authorize redirect = %s", loc)
	}
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	cb := httptest.NewRequest("GET", "/api/v1/auth/callback/oidc?code=the-code&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	srv.ServeHTTP(out, cb)
	for _, c := range out.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return out, c.Value
		}
	}
	return out, ""
}

func TestOIDCSignInWithGroupRoles(t *testing.T) {
	h := newEnforcedHarness(t)
	idp := newFakeIdP(t)
	ctx := context.Background()
	if err := h.st.UpsertSSOProvider(ctx, store.SSOProvider{Provider: "oidc", Enabled: true, ClientID: "forge-client",
		ClientSecret: "s3cret", Settings: map[string]string{"issuer": idp.srv.URL, "display_name": "Okta"}}); err != nil {
		t.Fatal(err)
	}
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "POST", "/api/v1/identity/group-mappings", map[string]any{"group": "shop-devs", "repo_pattern": "acme/*", "role": "developer"}), 201, "")

	rec, cookie := idp.signIn(t, h.srv)
	if cookie == "" || rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	dev := testCaller{cookie: cookie}
	var me map[string]any
	_ = json.Unmarshal(h.do(dev, "GET", "/api/v1/auth/me", nil).Body.Bytes(), &me)
	if me["email"] != "dev@acme.io" || me["provider"] != "oidc" || me["is_admin"] != false {
		t.Errorf("me = %v", me)
	}
	// shop-devs → developer on acme/*: may run things on acme/shop, not on globex/x
	// (which has a member, so it isn't in open bootstrap mode).
	_, _ = h.st.AddMember(ctx, "globex/x", "someone@globex.io", "owner")
	_, _ = h.st.AddMember(ctx, "acme/shop", "owner@acme.io", "owner")
	var perms map[string]any
	_ = json.Unmarshal(h.do(dev, "GET", "/api/v1/permissions?repo=acme/shop", nil).Body.Bytes(), &perms)
	if perms["can_run"] != true {
		t.Errorf("group role not applied: %v", perms)
	}
	_ = json.Unmarshal(h.do(dev, "GET", "/api/v1/permissions?repo=globex/x", nil).Body.Bytes(), &perms)
	if perms["can_run"] != false {
		t.Errorf("group role leaked to another namespace: %v", perms)
	}

	// platform-admins → admin, picked up at the next sign-in.
	h.expect(h.do(admin, "POST", "/api/v1/identity/group-mappings", map[string]any{"group": "platform-admins", "platform_admin": true}), 201, "")
	_, cookie = idp.signIn(t, h.srv)
	_ = json.Unmarshal(h.do(testCaller{cookie: cookie}, "GET", "/api/v1/auth/me", nil).Body.Bytes(), &me)
	if me["is_admin"] != true {
		t.Errorf("platform-admins mapping not applied: %v", me)
	}
	// Leaving the group in the IdP removes the role at the next sign-in.
	idp.groups = []string{"shop-devs"}
	_, cookie = idp.signIn(t, h.srv)
	_ = json.Unmarshal(h.do(testCaller{cookie: cookie}, "GET", "/api/v1/auth/me", nil).Body.Bytes(), &me)
	if me["is_admin"] != false {
		t.Errorf("stale admin group kept: %v", me)
	}

	// A token for another client is refused.
	idp.aud = "someone-else"
	if rec, c := idp.signIn(t, h.srv); c != "" || rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong audience accepted: %d %s", rec.Code, rec.Body.String())
	}
	// Non-admins can't manage mappings.
	h.expect(h.do(dev, "POST", "/api/v1/identity/group-mappings", map[string]any{"group": "x", "platform_admin": true}), 403, "")
	// Login page sees the provider with its label.
	var prov struct {
		Info []map[string]string `json:"provider_info"`
	}
	_ = json.Unmarshal(h.do(testCaller{}, "GET", "/api/v1/auth/providers", nil).Body.Bytes(), &prov)
	found := false
	for _, p := range prov.Info {
		found = found || (p["name"] == "oidc" && p["label"] == "Okta" && p["kind"] == "redirect")
	}
	if !found {
		t.Errorf("provider info = %v", prov.Info)
	}
}

func TestSSOConfigValidation(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	for body, want := range map[string]string{
		`{"provider":"oidc","enabled":true,"client_id":"c","settings":{"issuer":"http://idp.example.com"}}`: "https",
		`{"provider":"ldap","enabled":true,"settings":{"url":"ldap://dir:389","base_dn":"dc=x"}}`:           "unencrypted",
		`{"provider":"ldap","enabled":true,"settings":{"url":"ldaps://dir:636"}}`:                           "base_dn",
		`{"provider":"saml","enabled":true}`:                                                                "provider must be",
	} {
		var b map[string]any
		_ = json.Unmarshal([]byte(body), &b)
		rec := h.do(admin, "PUT", "/api/v1/sso", b)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s → %d %s, want 400 %q", body, rec.Code, rec.Body.String(), want)
		}
	}
}

func TestSessionLifetimeAndRuleDelete(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "PUT", "/api/v1/identity/session-settings", map[string]any{"lifetime_hours": 0}), 200, "")
	if rec := h.do(admin, "PUT", "/api/v1/identity/session-settings", map[string]any{"lifetime_hours": 9999}); rec.Code != 400 {
		t.Errorf("out-of-range lifetime = %d", rec.Code)
	}
	h.expect(h.do(admin, "PUT", "/api/v1/identity/session-settings", map[string]any{"lifetime_hours": 72}), 200, "")
	if got := h.srv.sessionLifetime(context.Background()); got != 72*time.Hour {
		t.Errorf("lifetime = %s", got)
	}

	// Deleting the default production rule sticks across restarts.
	rules, _ := h.st.ListProtectedEnvs(context.Background(), "")
	var prodID int64
	for _, r := range rules {
		if r.Repo == "" && r.Name == "production" {
			prodID = r.ID
		}
	}
	if prodID == 0 {
		t.Fatal("no default production rule")
	}
	h.expect(h.do(admin, "DELETE", "/api/v1/protected-environments/"+strconv.FormatInt(prodID, 10), nil), 204, "")
	if err := h.st.Migrate(context.Background()); err != nil { // what a restart does
		t.Fatal(err)
	}
	rules, _ = h.st.ListProtectedEnvs(context.Background(), "")
	for _, r := range rules {
		if r.Repo == "" && r.Name == "production" {
			t.Error("default production rule came back after a restart")
		}
	}
}

// ---- a tiny LDAP server, enough for bind + search ----

type ldapEntry struct {
	dn, uid, password string
	attrs             map[string][]string
}

func serveLDAP(t *testing.T, entries []ldapEntry, serviceDN, servicePW string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleLDAP(conn, entries, serviceDN, servicePW)
		}
	}()
	return "ldap://" + ln.Addr().String()
}

func ldapResult(id int64, tag ber.Tag, code int64) *ber.Packet {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "msg")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "id"))
	r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "result")
	r.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, "code"))
	r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "dn"))
	r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "msg"))
	p.AppendChild(r)
	return p
}

// equalityValues collects every (attr, value) equality assertion in a filter.
func equalityValues(f *ber.Packet, out map[string]string) {
	if f.ClassType == ber.ClassContext && f.Tag == 3 && len(f.Children) == 2 {
		out[strings.ToLower(f.Children[0].Data.String())] = f.Children[1].Data.String()
		return
	}
	for _, c := range f.Children {
		equalityValues(c, out)
	}
}

func handleLDAP(conn net.Conn, entries []ldapEntry, serviceDN, servicePW string) {
	defer conn.Close()
	for {
		pkt, err := ber.ReadPacket(conn)
		if err != nil || len(pkt.Children) < 2 {
			return
		}
		id := pkt.Children[0].Value.(int64)
		op := pkt.Children[1]
		switch op.Tag {
		case 0: // bind
			dn, pw := op.Children[1].Data.String(), op.Children[2].Data.String()
			code := int64(49) // invalidCredentials
			if dn == serviceDN && pw == servicePW {
				code = 0
			}
			for _, e := range entries {
				if e.dn == dn && e.password == pw && pw != "" {
					code = 0
				}
			}
			_, _ = conn.Write(ldapResult(id, 1, code).Bytes())
		case 3: // search
			eq := map[string]string{}
			equalityValues(op.Children[6], eq)
			for _, e := range entries {
				if eq["uid"] != e.uid && eq["mail"] != e.attrs["mail"][0] {
					continue
				}
				p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "msg")
				p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "id"))
				r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 4, nil, "entry")
				r.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, e.dn, "dn"))
				attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attrs")
				for k, vs := range e.attrs {
					a := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attr")
					a.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, k, "type"))
					set := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "vals")
					for _, v := range vs {
						set.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, "v"))
					}
					a.AppendChild(set)
					attrs.AppendChild(a)
				}
				r.AppendChild(attrs)
				p.AppendChild(r)
				_, _ = conn.Write(p.Bytes())
			}
			_, _ = conn.Write(ldapResult(id, 5, 0).Bytes())
		case 2: // unbind
			return
		}
	}
}

func TestLDAPSignIn(t *testing.T) {
	t.Setenv("FORGE_LDAP_ALLOW_PLAINTEXT", "true") // the test directory has no TLS
	h := newEnforcedHarness(t)
	ctx := context.Background()
	addr := serveLDAP(t, []ldapEntry{{
		dn: "uid=ravi,ou=people,dc=acme,dc=io", uid: "ravi", password: "correct horse",
		attrs: map[string][]string{"mail": {"ravi@acme.io"}, "displayName": {"Ravi K"},
			"memberOf": {"cn=deployers,ou=groups,dc=acme,dc=io", "cn=staff,ou=groups,dc=acme,dc=io"}},
	}}, "cn=forge,dc=acme,dc=io", "svc-pass")
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "PUT", "/api/v1/sso", map[string]any{"provider": "ldap", "enabled": true, "client_secret": "svc-pass",
		"settings": map[string]string{"url": addr, "bind_dn": "cn=forge,dc=acme,dc=io", "base_dn": "dc=acme,dc=io"}}), 204, "")
	h.expect(h.do(admin, "POST", "/api/v1/identity/group-mappings", map[string]any{"group": "deployers", "repo_pattern": "*", "role": "owner"}), 201, "")

	login := func(user, pw string) *httptest.ResponseRecorder {
		return h.do(testCaller{}, "POST", "/api/v1/auth/ldap/login", map[string]string{"username": user, "password": pw})
	}
	if rec := login("ravi", "wrong"); rec.Code != 401 || !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Errorf("bad password = %d %s", rec.Code, rec.Body.String())
	}
	if rec := login("nobody", "x"); rec.Code != 401 {
		t.Errorf("unknown user = %d", rec.Code)
	}
	if rec := login("ravi", ""); rec.Code != 400 {
		t.Errorf("empty password = %d (an unauthenticated bind must never sign anyone in)", rec.Code)
	}
	if rec := login("ravi)(uid=*", "x"); rec.Code != 401 {
		t.Errorf("filter injection = %d", rec.Code)
	}
	rec := login("ravi", "correct horse")
	if rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	var groups struct {
		Groups []string `json:"groups"`
	}
	_ = json.Unmarshal(h.do(testCaller{cookie: cookie}, "GET", "/api/v1/identity/me/groups", nil).Body.Bytes(), &groups)
	if strings.Join(groups.Groups, ",") != "deployers,staff" {
		t.Errorf("groups = %v", groups.Groups)
	}
	if role, _, _ := h.st.RepoAccess(ctx, "any/repo", "ravi@acme.io"); role != "owner" {
		t.Errorf("group role = %q, want owner", role)
	}
	// Throttle: five failures lock the account for a while.
	for i := 0; i < 5; i++ {
		login("ravi", "nope")
	}
	if rec := login("ravi", "correct horse"); rec.Code != 429 {
		t.Errorf("after 5 failures = %d, want 429", rec.Code)
	}
}
