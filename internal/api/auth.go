package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const (
	sessionCookie = "forge_session"
	stateCookie   = "forge_oauth_state"
)

// oauthProvider describes one supported identity provider. Endpoints follow
// the plain OAuth2 authorization-code flow; userinfo normalizes to
// (email, name).
type oauthProvider struct {
	authURL  func(tenant string) string
	tokenURL func(tenant string) string
	scopes   string
	userinfo func(ctx context.Context, accessToken string) (email, name string, err error)
}

var oauthProviders = map[string]oauthProvider{
	"google": {
		authURL:  func(string) string { return "https://accounts.google.com/o/oauth2/v2/auth" },
		tokenURL: func(string) string { return "https://oauth2.googleapis.com/token" },
		scopes:   "openid email profile",
		userinfo: oidcUserinfo("https://openidconnect.googleapis.com/v1/userinfo"),
	},
	"microsoft": {
		authURL: func(tenant string) string {
			return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/authorize"
		},
		tokenURL: func(tenant string) string {
			return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
		},
		scopes:   "openid email profile",
		userinfo: oidcUserinfo("https://graph.microsoft.com/oidc/userinfo"),
	},
	"github": {
		authURL:  func(string) string { return "https://github.com/login/oauth/authorize" },
		tokenURL: func(string) string { return "https://github.com/login/oauth/access_token" },
		scopes:   "read:user user:email",
		userinfo: githubUserinfo,
	},
}

func externalURL() string {
	if v := os.Getenv("EXTERNAL_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://localhost:8080"
}

func frontendURL() string {
	if v := os.Getenv("FRONTEND_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://localhost:5173"
}

func redirectURI(provider string) string {
	return externalURL() + "/api/v1/auth/callback/" + provider
}

func (s *Server) registerAuthRoutes() {
	m := s.mux
	// Public.
	m.HandleFunc("GET /api/v1/auth/providers", s.authProviders)
	m.HandleFunc("GET /api/v1/auth/login/{provider}", s.authLogin)
	m.HandleFunc("GET /api/v1/auth/callback/{provider}", s.authCallback)
	m.HandleFunc("GET /api/v1/auth/me", s.authMe)
	m.HandleFunc("POST /api/v1/auth/logout", s.authLogout)
	// Admin config.
	m.HandleFunc("GET /api/v1/sso", s.listSSO)
	m.HandleFunc("PUT /api/v1/sso", s.upsertSSO)
}

// ---- enforcement ----

// authExempt paths keep working without a session: the runner protocol and
// webhooks authenticate their own way, and auth endpoints must be reachable
// to log in at all.
func authExempt(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/v1/runner/"),
		strings.HasPrefix(path, "/api/v1/webhooks/"),
		strings.HasPrefix(path, "/api/v1/auth/"),
		strings.HasPrefix(path, "/api/v1/downloads/"),
		// OIDC discovery + JWKS must be reachable by AWS/GCP (no session) even
		// when SSO is enforced, so cloud providers can validate job ID tokens.
		path == "/.well-known/openid-configuration",
		path == "/.well-known/jwks.json",
		path == "/api/v1/healthz",
		path == "/api/v1/metrics":
		return true
	}
	return false
}

// ssoEnabled caches the enforcement flag briefly so every request doesn't
// hit the database.
type ssoCache struct {
	mu      sync.Mutex
	val     bool
	checked time.Time
}

func (s *Server) ssoEnforced(ctx context.Context) bool {
	s.sso.mu.Lock()
	defer s.sso.mu.Unlock()
	if time.Since(s.sso.checked) < 5*time.Second {
		return s.sso.val
	}
	enabled, err := s.store.AnySSOEnabled(ctx)
	if err != nil {
		return s.sso.val // fail open to the last-known value
	}
	s.sso.val, s.sso.checked = enabled, time.Now()
	return enabled
}

// currentSession returns the logged-in session, if any: the SSO cookie
// session, or else a pseudo-session for the owner of a personal API token
// (Authorization: Bearer fci_…), which acts on that user's behalf.
func (s *Server) currentSession(r *http.Request) *store.Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return s.tokenSession(r)
	}
	sess, err := s.store.GetSession(r.Context(), c.Value)
	if err != nil {
		return s.tokenSession(r)
	}
	return sess
}

// requireAuth is called from ServeHTTP for non-exempt paths.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !s.ssoEnforced(r.Context()) {
		return true
	}
	if authExempt(r.URL.Path) {
		return true
	}
	if s.currentSession(r) != nil {
		return true
	}
	// Record unauthenticated attempts to mutate as denied. Safe methods (GET/
	// HEAD/OPTIONS) are not audited to avoid flooding the trail with anonymous
	// reads that are simply bounced to the login flow.
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		s.audit(r, "auth.denied", r.Method+" "+r.URL.Path, r.URL.Query().Get("repo"), "denied", nil)
	}
	writeErr(w, http.StatusUnauthorized, "authentication required — sign in via SSO")
	return false
}

// ---- login flow ----

func (s *Server) authProviders(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListSSOProviders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load providers")
		return
	}
	enabled := []string{}
	type info struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Kind  string `json:"kind"` // redirect | password
	}
	infos := []info{}
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		enabled = append(enabled, p.Provider)
		i := info{Name: p.Provider, Label: p.Settings["display_name"], Kind: "redirect"}
		if p.Provider == "ldap" {
			i.Kind = "password"
		}
		if i.Label == "" {
			i.Label = map[string]string{"google": "Google", "microsoft": "Microsoft", "github": "GitHub",
				"oidc": "Single sign-on", "ldap": "Directory account"}[p.Provider]
		}
		infos = append(infos, i)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": enabled, "provider_info": infos, "enforced": len(enabled) > 0})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	if name == "oidc" {
		cfg, err := s.store.GetEnabledSSOProvider(r.Context(), name)
		if err != nil {
			writeErr(w, http.StatusNotFound, "provider not enabled")
			return
		}
		s.oidcLogin(w, r, cfg)
		return
	}
	meta, ok := oauthProviders[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown provider")
		return
	}
	cfg, err := s.store.GetEnabledSSOProvider(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "provider not enabled")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load provider")
		return
	}

	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	state := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/api/v1/auth",
		MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})

	q := url.Values{
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {redirectURI(name)},
		"response_type": {"code"},
		"scope":         {meta.scopes},
		"state":         {state},
	}
	if name == "google" && cfg.AllowedDomain != "" {
		q.Set("hd", cfg.AllowedDomain)
	}
	http.Redirect(w, r, meta.authURL(cfg.Tenant)+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	if name == "oidc" {
		cfg, err := s.store.GetEnabledSSOProvider(r.Context(), name)
		if err != nil {
			writeErr(w, http.StatusNotFound, "provider not enabled")
			return
		}
		s.oidcCallback(w, r, cfg)
		return
	}
	meta, ok := oauthProviders[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown provider")
		return
	}
	cfg, err := s.store.GetEnabledSSOProvider(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "provider not enabled")
		return
	}
	stateC, err := r.Cookie(stateCookie)
	if err != nil || stateC.Value == "" || r.URL.Query().Get("state") != stateC.Value {
		writeErr(w, http.StatusBadRequest, "state mismatch — restart login")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, "provider returned no code: "+r.URL.Query().Get("error_description"))
		return
	}

	token, err := exchangeCode(r.Context(), meta.tokenURL(cfg.Tenant), cfg.ClientID, cfg.ClientSecret, code, redirectURI(name))
	if err != nil {
		slog.Error("oauth exchange failed", "provider", name, "err", err)
		writeErr(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	email, displayName, err := meta.userinfo(r.Context(), token)
	if err != nil || email == "" {
		slog.Error("userinfo failed", "provider", name, "err", err)
		writeErr(w, http.StatusBadGateway, "could not read user profile")
		return
	}
	// These providers report no groups; recording none clears stale ones.
	if err := s.finishLogin(r.Context(), w, name, email, displayName, nil, cfg.AllowedDomain); err != nil {
		if errors.Is(err, errForbiddenDomain) {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	http.Redirect(w, r, frontendURL(), http.StatusFound)
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	// A non-expiring API token has no expiry; report null, not year 1.
	var expires any = sess.Expires
	if sess.Expires.IsZero() {
		expires = nil
	}
	// is_admin lets the UI hide admin-only navigation.
	writeJSON(w, http.StatusOK, map[string]any{
		"email":      sess.Email,
		"name":       sess.Name,
		"provider":   sess.Provider,
		"expires_at": expires,
		"is_admin":   s.isAdmin(r),
	})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// ---- provider config (admin) ----

func (s *Server) listSSO(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListSSOProviders(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load SSO config")
		return
	}
	// Include redirect URIs so the UI can show exactly what to paste into
	// each provider console.
	uris := map[string]string{"oidc": redirectURI("oidc")}
	for name := range oauthProviders {
		uris[name] = redirectURI(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "redirect_uris": uris})
}

func (s *Server) upsertSSO(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var p store.SSOProvider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateSSO(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpsertSSOProvider(r.Context(), p); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save SSO config")
		return
	}
	slog.Info("sso config updated", "provider", p.Provider, "enabled", p.Enabled)
	// NEVER record client_secret; only safe metadata.
	s.audit(r, "sso.upsert", p.Provider, "", "ok", map[string]any{
		"provider":       p.Provider,
		"enabled":        p.Enabled,
		"client_id_set":  p.ClientID != "",
		"allowed_domain": p.AllowedDomain,
		"settings":       p.Settings, // non-secret by construction
	})
	w.WriteHeader(http.StatusNoContent)
}

// validateSSO checks a provider config before it is saved.
func validateSSO(p *store.SSOProvider) error {
	if p.Settings == nil {
		p.Settings = map[string]string{}
	}
	for k, v := range p.Settings {
		p.Settings[k] = strings.TrimSpace(v)
	}
	switch p.Provider {
	case "google", "microsoft", "github":
		if p.Enabled && p.ClientID == "" {
			return errors.New("client_id is required to enable a provider")
		}
	case "oidc":
		if !p.Enabled {
			return nil
		}
		if p.ClientID == "" {
			return errors.New("client_id is required to enable a provider")
		}
		if err := oidcIssuerOK(p.Settings["issuer"]); err != nil {
			return fmt.Errorf("issuer: %w", err)
		}
	case "ldap":
		if !p.Enabled {
			return nil
		}
		u, err := url.Parse(p.Settings["url"])
		if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
			return errors.New("url must be ldap://host:389 or ldaps://host:636")
		}
		if u.Scheme == "ldap" && p.Settings["start_tls"] != "true" && os.Getenv("FORGE_LDAP_ALLOW_PLAINTEXT") != "true" {
			return errors.New("plain ldap:// would send passwords unencrypted: use ldaps://, turn on StartTLS, or set FORGE_LDAP_ALLOW_PLAINTEXT=true for a lab directory")
		}
		if p.Settings["base_dn"] == "" {
			return errors.New("base_dn is required")
		}
		if f := p.Settings["user_filter"]; f != "" && !strings.Contains(f, "{username}") {
			return errors.New("user_filter must contain {username}")
		}
	default:
		return errors.New("provider must be google, microsoft, github, oidc or ldap")
	}
	return nil
}

// ---- oauth plumbing ----

func exchangeCode(ctx context.Context, tokenURL, clientID, clientSecret, code, redirect string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"redirect_uri":  {redirect},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // github defaults to form encoding otherwise
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, out.Error)
	}
	return out.AccessToken, nil
}

func oidcUserinfo(endpoint string) func(context.Context, string) (string, string, error) {
	return func(ctx context.Context, token string) (string, string, error) {
		var out struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := getJSON(ctx, endpoint, token, &out); err != nil {
			return "", "", err
		}
		return out.Email, out.Name, nil
	}
}

func githubUserinfo(ctx context.Context, token string) (string, string, error) {
	var user struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := getJSON(ctx, "https://api.github.com/user", token, &user); err != nil {
		return "", "", err
	}
	email := user.Email
	if email == "" {
		var emails []struct {
			Email   string `json:"email"`
			Primary bool   `json:"primary"`
		}
		if err := getJSON(ctx, "https://api.github.com/user/emails", token, &emails); err == nil {
			for _, e := range emails {
				if e.Primary {
					email = e.Email
					break
				}
			}
		}
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	return email, name, nil
}

func getJSON(ctx context.Context, endpoint, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}
