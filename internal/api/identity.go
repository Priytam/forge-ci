package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Sign-in with any identity provider: a generic OpenID Connect client (Okta,
// Keycloak, Auth0, Entra ID, Authentik, Dex, Google Workspace, Zitadel...),
// LDAP / Active Directory, and the built-in Google / Microsoft / GitHub
// buttons. Every path ends in finishLogin, which records the user's IdP
// groups (for group → role mappings) and starts a session.

func (s *Server) registerIdentityRoutes() {
	m := s.mux
	m.HandleFunc("POST /api/v1/auth/ldap/login", s.ldapLogin)
	m.HandleFunc("GET /api/v1/identity/group-mappings", s.listGroupMappings)
	m.HandleFunc("POST /api/v1/identity/group-mappings", s.createGroupMapping)
	m.HandleFunc("DELETE /api/v1/identity/group-mappings/{id}", s.deleteGroupMapping)
	m.HandleFunc("GET /api/v1/identity/session-settings", s.getSessionSettings)
	m.HandleFunc("PUT /api/v1/identity/session-settings", s.putSessionSettings)
	m.HandleFunc("GET /api/v1/identity/me/groups", s.myGroups)
}

// sessionLifetime is the lifetime of a new sign-in.
func (s *Server) sessionLifetime(ctx context.Context) time.Duration {
	env, _ := time.ParseDuration(os.Getenv("SESSION_TTL"))
	return s.store.SessionTTL(ctx, env)
}

// finishLogin is the common end of every sign-in: domain check, groups,
// session, cookie.
func (s *Server) finishLogin(ctx context.Context, w http.ResponseWriter, provider, email, name string, groups []string, allowedDomain string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("the identity provider did not return an email address")
	}
	if allowedDomain != "" && !strings.HasSuffix(strings.ToLower(email), "@"+strings.ToLower(allowedDomain)) {
		return errForbiddenDomain
	}
	if err := s.store.RecordUserGroups(ctx, email, provider, groups); err != nil {
		return err
	}
	ttl := s.sessionLifetime(ctx)
	sess, err := s.store.CreateSession(ctx, email, name, provider, ttl)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.Token, Path: "/",
		MaxAge: int(ttl.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(externalURL(), "https://"),
	})
	slog.Info("sign-in", "provider", provider, "email", email, "groups", len(groups))
	return nil
}

var errForbiddenDomain = errors.New("email domain not allowed for this workspace")

// ---- generic OpenID Connect ----

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

var (
	discoveryMu    sync.Mutex
	discoveryCache = map[string]struct {
		doc oidcDiscovery
		at  time.Time
	}{}
)

// oidcIssuerOK requires https, except for a loopback issuer (local IdPs in
// development).
func oidcIssuerOK(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return errors.New("issuer must be a URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return errors.New("issuer must use https")
}

func discover(ctx context.Context, issuer string) (oidcDiscovery, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	discoveryMu.Lock()
	if c, ok := discoveryCache[issuer]; ok && time.Since(c.at) < time.Hour {
		discoveryMu.Unlock()
		return c.doc, nil
	}
	discoveryMu.Unlock()
	if err := oidcIssuerOK(issuer); err != nil {
		return oidcDiscovery{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return oidcDiscovery{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcDiscovery{}, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}
	var doc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return oidcDiscovery{}, err
	}
	// The document must describe this issuer (OIDC Discovery §4.3).
	if strings.TrimSuffix(doc.Issuer, "/") != issuer || doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return oidcDiscovery{}, errors.New("discovery document does not match the issuer")
	}
	discoveryMu.Lock()
	discoveryCache[issuer] = struct {
		doc oidcDiscovery
		at  time.Time
	}{doc, time.Now()}
	discoveryMu.Unlock()
	return doc, nil
}

const (
	pkceCookie  = "forge_oidc_pkce"
	nonceCookie = "forge_oidc_nonce"
)

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request, cfg *store.SSOProvider) {
	doc, err := discover(r.Context(), cfg.Settings["issuer"])
	if err != nil {
		slog.Error("oidc discovery", "err", err)
		writeErr(w, http.StatusBadGateway, "identity provider discovery failed: "+err.Error())
		return
	}
	state, verifier, nonce := randomToken(), randomToken(), randomToken()
	for name, v := range map[string]string{stateCookie: state, pkceCookie: verifier, nonceCookie: nonce} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: v, Path: "/api/v1/auth", MaxAge: 600, HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(externalURL(), "https://")})
	}
	sum := sha256.Sum256([]byte(verifier))
	scopes := cfg.Settings["scopes"]
	if scopes == "" {
		scopes = "openid email profile"
	}
	q := url.Values{
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {redirectURI("oidc")},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(doc.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, doc.AuthorizationEndpoint+sep+q.Encode(), http.StatusFound)
}

func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// claimStrings reads a claim that may be a string or a list of strings.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := []string{}
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// claimAt reads a claim by name; a dotted name walks nested objects
// (Keycloak's "realm_access.roles").
func claimAt(claims map[string]any, name string) any {
	var cur any = claims
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request, cfg *store.SSOProvider) {
	if st := cookieValue(r, stateCookie); st == "" || r.URL.Query().Get("state") != st {
		writeErr(w, http.StatusBadRequest, "state mismatch — restart login")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, "provider returned no code: "+r.URL.Query().Get("error_description"))
		return
	}
	issuer := strings.TrimSuffix(cfg.Settings["issuer"], "/")
	doc, err := discover(r.Context(), issuer)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "identity provider discovery failed")
		return
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"redirect_uri":  {redirectURI("oidc")},
		"code_verifier": {cookieValue(r, pkceCookie)},
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil || tok.IDToken == "" {
		slog.Error("oidc token exchange", "status", resp.StatusCode, "err", err, "detail", tok.Error)
		writeErr(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	claims, err := idTokenClaims(tok.IDToken, issuer, cfg.ClientID, cookieValue(r, nonceCookie))
	if err != nil {
		slog.Error("oidc id token rejected", "err", err)
		writeErr(w, http.StatusUnauthorized, "sign-in rejected: "+err.Error())
		return
	}
	// Userinfo fills anything the ID token left out (many IdPs keep groups
	// and email there).
	if doc.UserinfoEndpoint != "" && tok.AccessToken != "" {
		var ui map[string]any
		if getJSON(r.Context(), doc.UserinfoEndpoint, tok.AccessToken, &ui) == nil {
			if sub, _ := ui["sub"].(string); sub == claims["sub"] { // OIDC Core §5.3.2
				for k, v := range ui {
					if _, have := claims[k]; !have {
						claims[k] = v
					}
				}
			}
		}
	}
	if v, ok := claims["email_verified"].(bool); ok && !v {
		writeErr(w, http.StatusForbidden, "your email address is not verified with the identity provider")
		return
	}
	email, _ := claims["email"].(string)
	if email == "" {
		email, _ = claims["preferred_username"].(string)
		if !strings.Contains(email, "@") {
			email = ""
		}
	}
	name, _ := claims["name"].(string)
	groupsClaim := cfg.Settings["groups_claim"]
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	groups := claimStrings(claimAt(claims, groupsClaim))
	if err := s.finishLogin(r.Context(), w, "oidc", email, name, groups, cfg.AllowedDomain); err != nil {
		code := http.StatusForbidden
		if !errors.Is(err, errForbiddenDomain) && email != "" {
			code = http.StatusInternalServerError
		}
		writeErr(w, code, err.Error())
		return
	}
	http.Redirect(w, r, afterLoginURL(w, r), http.StatusFound)
}

// idTokenClaims decodes an ID token received directly from the token
// endpoint over TLS and checks iss, aud, exp and nonce. Its signature is not
// re-verified: for the authorization-code flow, OIDC Core §3.1.3.7 lets the
// client rely on the TLS-authenticated token endpoint instead.
func idTokenClaims(idToken, issuer, clientID, nonce string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed id token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed id token")
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("malformed id token")
	}
	if iss, _ := claims["iss"].(string); strings.TrimSuffix(iss, "/") != issuer {
		return nil, errors.New("id token issuer mismatch")
	}
	audOK := false
	for _, a := range claimStrings(claims["aud"]) {
		audOK = audOK || a == clientID
	}
	if !audOK {
		return nil, errors.New("id token is for another client")
	}
	if exp, _ := claims["exp"].(float64); exp == 0 || time.Unix(int64(exp), 0).Before(time.Now().Add(-time.Minute)) {
		return nil, errors.New("id token expired")
	}
	if n, _ := claims["nonce"].(string); nonce == "" || n != nonce {
		return nil, errors.New("id token nonce mismatch")
	}
	return claims, nil
}

// ---- LDAP / Active Directory ----

// loginThrottle limits failed password sign-ins per user and per client IP.
type loginThrottle struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

var ldapThrottle = &loginThrottle{failures: map[string][]time.Time{}}

const (
	maxLoginFailures = 5
	failureWindow    = 5 * time.Minute
)

func (t *loginThrottle) blocked(keys ...string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		recent := t.failures[k][:0]
		for _, at := range t.failures[k] {
			if time.Since(at) < failureWindow {
				recent = append(recent, at)
			}
		}
		t.failures[k] = recent
		if len(recent) >= maxLoginFailures {
			return true
		}
	}
	return false
}

func (t *loginThrottle) fail(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		t.failures[k] = append(t.failures[k], time.Now())
	}
}

func (t *loginThrottle) clear(key string) {
	t.mu.Lock()
	delete(t.failures, key)
	t.mu.Unlock()
}

// ldapUser is what a directory says about a user who authenticated.
type ldapUser struct {
	Email, Name string
	Groups      []string
}

// errBadCredentials is the one error a user sees for any credential problem,
// so the form never reveals whether the account exists.
var errBadCredentials = errors.New("invalid username or password")

func ldapSetting(cfg *store.SSOProvider, key, def string) string {
	if v := strings.TrimSpace(cfg.Settings[key]); v != "" {
		return v
	}
	return def
}

// ldapAuthenticate binds as the service account, finds the user, binds as
// the user with their password, and reads email, name and groups.
func ldapAuthenticate(cfg *store.SSOProvider, username, password string) (*ldapUser, error) {
	addr := ldapSetting(cfg, "url", "")
	conn, err := ldap.DialURL(addr, ldap.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	conn.SetTimeout(15 * time.Second)
	if ldapSetting(cfg, "start_tls", "") == "true" {
		u, _ := url.Parse(addr)
		if err := conn.StartTLS(&tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}); err != nil {
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}
	if bindDN := ldapSetting(cfg, "bind_dn", ""); bindDN != "" {
		if err := conn.Bind(bindDN, cfg.ClientSecret); err != nil {
			return nil, fmt.Errorf("service bind: %w", err)
		}
	}
	emailAttr := ldapSetting(cfg, "email_attribute", "mail")
	nameAttr := ldapSetting(cfg, "name_attribute", "displayName")
	groupAttr := ldapSetting(cfg, "group_attribute", "memberOf")
	filter := strings.ReplaceAll(ldapSetting(cfg, "user_filter", "(|(uid={username})(sAMAccountName={username})(mail={username}))"),
		"{username}", ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(ldapSetting(cfg, "base_dn", ""), ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 2, 15, false, filter, []string{emailAttr, nameAttr, "cn", groupAttr}, nil))
	if err != nil {
		return nil, fmt.Errorf("user search: %w", err)
	}
	if len(res.Entries) != 1 {
		return nil, errBadCredentials // none, or ambiguous
	}
	entry := res.Entries[0]
	if err := conn.Bind(entry.DN, password); err != nil {
		return nil, errBadCredentials
	}
	user := &ldapUser{Email: entry.GetAttributeValue(emailAttr), Name: entry.GetAttributeValue(nameAttr)}
	if user.Name == "" {
		user.Name = entry.GetAttributeValue("cn")
	}
	for _, g := range entry.GetAttributeValues(groupAttr) {
		user.Groups = append(user.Groups, groupName(g))
	}
	// Directories without memberOf: search groups that list the user.
	if base := ldapSetting(cfg, "group_base_dn", ""); base != "" {
		if bindDN := ldapSetting(cfg, "bind_dn", ""); bindDN != "" {
			_ = conn.Bind(bindDN, cfg.ClientSecret) // search as the service account again
		}
		gf := fmt.Sprintf("(|(member=%s)(uniqueMember=%s)(memberUid=%s))",
			ldap.EscapeFilter(entry.DN), ldap.EscapeFilter(entry.DN), ldap.EscapeFilter(username))
		if gr, err := conn.Search(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			500, 15, false, gf, []string{"cn"}, nil)); err == nil {
			for _, g := range gr.Entries {
				user.Groups = append(user.Groups, g.GetAttributeValue("cn"))
			}
		}
	}
	return user, nil
}

// groupName turns a group DN (cn=deployers,ou=groups,dc=acme,dc=io) into its
// CN; anything else is used as is.
func groupName(dnOrName string) string {
	if dn, err := ldap.ParseDN(dnOrName); err == nil && len(dn.RDNs) > 0 {
		for _, a := range dn.RDNs[0].Attributes {
			if strings.EqualFold(a.Type, "cn") {
				return a.Value
			}
		}
	}
	return dnOrName
}

func (s *Server) ldapLogin(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetEnabledSSOProvider(r.Context(), "ldap")
	if err != nil {
		writeErr(w, http.StatusNotFound, "LDAP sign-in is not enabled")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	// An empty password is an "unauthenticated bind" that many servers accept.
	if req.Username == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	userKey, ipKey := "u:"+strings.ToLower(req.Username), "ip:"+clientIP(r)
	if ldapThrottle.blocked(userKey, ipKey) {
		writeErr(w, http.StatusTooManyRequests, "too many failed sign-ins; wait a few minutes")
		return
	}
	user, err := ldapAuthenticate(cfg, req.Username, req.Password)
	if err != nil {
		ldapThrottle.fail(userKey, ipKey)
		if !errors.Is(err, errBadCredentials) {
			slog.Error("ldap sign-in", "err", err) // directory problem: log it, don't show it
		}
		s.audit(r, "auth.ldap.failed", req.Username, "", "denied", nil)
		writeErr(w, http.StatusUnauthorized, errBadCredentials.Error())
		return
	}
	ldapThrottle.clear(userKey)
	if err := s.finishLogin(r.Context(), w, "ldap", user.Email, user.Name, user.Groups, cfg.AllowedDomain); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"email": user.Email, "name": user.Name})
}

// ---- group mappings & session settings (admin) ----

func (s *Server) listGroupMappings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ms, err := s.store.ListGroupMappings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list mappings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": ms})
}

func (s *Server) createGroupMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var m store.GroupMapping
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	out, err := s.store.CreateGroupMapping(r.Context(), m)
	if errors.Is(err, store.ErrDuplicateMapping) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "identity.group-mapping.create", out.Group, "", "ok", map[string]any{
		"provider": out.Provider, "platform_admin": out.PlatformAdmin, "repo_pattern": out.RepoPattern, "role": out.Role,
	})
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteGroupMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteGroupMapping(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "mapping not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete mapping")
		return
	}
	s.audit(r, "identity.group-mapping.delete", strconv.FormatInt(id, 10), "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSessionSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lifetime_hours": int(s.sessionLifetime(r.Context()).Hours()),
		"min_hours":      int(store.MinSessionTTL.Hours()),
		"max_hours":      int(store.MaxSessionTTL.Hours()),
	})
}

func (s *Server) putSessionSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		LifetimeHours int `json:"lifetime_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.store.SetSessionTTLHours(r.Context(), req.LifetimeHours); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "identity.session-lifetime", strconv.Itoa(req.LifetimeHours), "", "ok", nil)
	s.getSessionSettings(w, r)
}

// myGroups shows a signed-in user the groups Forge knows for them, so a
// missing role can be debugged without an admin.
func (s *Server) myGroups(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	provider, groups, err := s.store.UserGroups(r.Context(), sess.Email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load groups")
		return
	}
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": provider, "groups": groups})
}
