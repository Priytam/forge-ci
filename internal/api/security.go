package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// ---- shared env helpers ----

// envBytes reads an integer byte count from key, falling back to def.
func envBytes(key string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ---- runner authentication (blocker 1) ----

func runnerAuthMode() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_AUTH")), "on") {
		return "on"
	}
	return "off"
}

// initRunnerAuth logs the current mode and, in enforced mode with no tokens,
// auto-generates a bootstrap token and prints it so an operator can configure
// runners.
func (s *Server) initRunnerAuth() {
	ctx := context.Background()
	if s.runnerAuth != "on" {
		slog.Warn("RUNNER_AUTH=off — runner protocol and artifact upload are UNAUTHENTICATED. " +
			"Set RUNNER_AUTH=on and issue tokens (POST /api/v1/runner-tokens) for production.")
		return
	}
	n, err := s.store.CountRunnerTokens(ctx)
	if err != nil {
		slog.Error("runner-auth: could not count tokens", "err", err)
		return
	}
	if n == 0 {
		rt, err := s.store.CreateRunnerToken(ctx, "auto-generated bootstrap token")
		if err != nil {
			slog.Error("runner-auth: could not create bootstrap token", "err", err)
			return
		}
		slog.Warn("RUNNER_AUTH=on with no tokens — generated a bootstrap runner token. "+
			"Configure runners with RUNNER_TOKEN and revoke this if leaked.",
			"runner_token", rt.Token)
		return
	}
	slog.Info("RUNNER_AUTH=on — runner protocol requires a bearer token", "tokens", n)
}

// requireRunnerAuth enforces a valid runner bearer token when RUNNER_AUTH=on.
// In off mode it always passes (backward compatible).
func (s *Server) requireRunnerAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.runnerAuth != "on" {
		return true
	}
	tok := bearerToken(r)
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "runner token required (Authorization: Bearer <token>)")
		return false
	}
	ok, err := s.store.ValidateRunnerToken(r.Context(), tok)
	if err != nil {
		slog.Error("runner-auth: token validation", "err", err)
		writeErr(w, http.StatusInternalServerError, "token validation failed")
		return false
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or revoked runner token")
		return false
	}
	return true
}

// hasValidRunnerToken reports whether the request carries a usable runner token
// (used to exempt token-authenticated calls from CSRF). It does not stamp
// last_used_at to avoid double-counting; requireRunnerAuth does that.
func (s *Server) hasValidRunnerToken(r *http.Request) bool {
	tok := bearerToken(r)
	if tok == "" {
		return false
	}
	ok, err := s.store.ValidateRunnerToken(r.Context(), tok)
	return err == nil && ok
}

// ---- admin authorization (blocker 2) ----

// adminEmails returns the set of platform-admin emails from ADMIN_EMAILS.
func adminEmails() map[string]bool {
	out := map[string]bool{}
	for _, e := range strings.Split(os.Getenv("ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out[e] = true
		}
	}
	return out
}

// isAdmin reports whether the request is from a platform admin. In open
// (bootstrap) mode — SSO not enforced — every caller is treated as admin. When
// SSO is enforced, the caller must have a session whose email is in
// ADMIN_EMAILS.
func (s *Server) isAdmin(r *http.Request) bool {
	if !s.ssoEnforced(r.Context()) {
		return true
	}
	sess := s.currentSession(r)
	if sess == nil {
		return false
	}
	return adminEmails()[strings.ToLower(sess.Email)]
}

// requireAdmin gates a mutating admin endpoint.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.isAdmin(r) {
		return true
	}
	writeErr(w, http.StatusForbidden, "admin privileges required")
	return false
}

// ---- CSRF protection (blocker 4) ----

// allowedOrigins is the set of origins accepted for cookie-authenticated
// mutations: the server's external URL and the frontend URL.
func allowedOrigins() map[string]bool {
	return map[string]bool{
		externalURL(): true,
		frontendURL(): true,
	}
}

// checkCSRF enforces same-origin on cookie-authenticated state-changing
// requests. Safe methods, requests without a session cookie, the runner
// protocol, webhooks, and valid runner-token requests are exempt. Returns
// false (and writes 403) when the request must be rejected.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	// Only cookie-authenticated requests are vulnerable to CSRF.
	if c, err := r.Cookie(sessionCookie); err != nil || c.Value == "" {
		return true
	}
	// Bearer/HMAC-authenticated surfaces don't rely on the cookie.
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/v1/runner/") || strings.HasPrefix(p, "/api/v1/webhooks/") {
		return true
	}
	if s.hasValidRunnerToken(r) {
		return true
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
				origin = u.Scheme + "://" + u.Host
			}
		}
	}
	if origin != "" && allowedOrigins()[origin] {
		return true
	}
	writeErr(w, http.StatusForbidden, "cross-origin request blocked (CSRF protection)")
	return false
}

// ---- runner token admin API (blocker 1) ----

func (s *Server) registerRunnerTokenRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/runner-tokens", s.listRunnerTokens)
	m.HandleFunc("POST /api/v1/runner-tokens", s.createRunnerToken)
	m.HandleFunc("POST /api/v1/runner-tokens/{tid}/revoke", s.revokeRunnerToken)
}

func (s *Server) listRunnerTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListRunnerTokens(r.Context())
	if err != nil {
		slog.Error("list runner tokens", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list runner tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (s *Server) createRunnerToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Description string `json:"description"`
	}
	// Body is optional; ignore decode errors on an empty body.
	_ = json.NewDecoder(r.Body).Decode(&req)
	rt, err := s.store.CreateRunnerToken(r.Context(), strings.TrimSpace(req.Description))
	if err != nil {
		slog.Error("create runner token", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create runner token")
		return
	}
	// rt.Token is returned exactly once, here.
	writeJSON(w, http.StatusCreated, rt)
}

func (s *Server) revokeRunnerToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	err := s.store.RevokeRunnerToken(r.Context(), r.PathValue("tid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "runner token not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revoke runner token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
