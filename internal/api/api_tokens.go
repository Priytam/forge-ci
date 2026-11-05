package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Personal API tokens let a user call the API from scripts and CI on their own
// behalf: a request carrying `Authorization: Bearer fci_…` resolves to the
// token owner's identity exactly like their SSO session would (see
// currentSession), so every existing admin and repo check applies unchanged.

const (
	apiTokenProvider       = "api-token"
	serviceAccountProvider = "service-account"
	defaultTokenLifetime   = 90 // days, when the caller names no lifetime
	tokenPolicyCacheMaxAge = 5 * time.Second
)

// policyCache caches the token policy briefly; it is consulted on every
// token-authenticated request.
type policyCache struct {
	mu      sync.Mutex
	val     store.TokenPolicy
	checked time.Time
}

func (s *Server) tokenPolicy(ctx context.Context) store.TokenPolicy {
	s.policy.mu.Lock()
	defer s.policy.mu.Unlock()
	if !s.policy.checked.IsZero() && time.Since(s.policy.checked) < tokenPolicyCacheMaxAge {
		return s.policy.val
	}
	p, err := s.store.GetTokenPolicy(ctx)
	if err != nil {
		slog.Error("load token policy", "err", err)
		if s.policy.checked.IsZero() {
			return store.DefaultTokenPolicy
		}
		return s.policy.val
	}
	s.policy.val, s.policy.checked = p, time.Now()
	return p
}

// tokenSession resolves a bearer API token to a pseudo-session for its owner,
// or nil when the request carries no usable token.
func (s *Server) tokenSession(r *http.Request) *store.Session {
	raw := bearerToken(r)
	if !strings.HasPrefix(raw, store.APITokenPrefix) {
		return nil
	}
	t, err := s.store.LookupAPIToken(r.Context(), raw, s.tokenPolicy(r.Context()))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("api token lookup", "err", err)
		}
		return nil
	}
	sess := &store.Session{Email: t.Owner, Name: t.OwnerName, Provider: apiTokenProvider}
	if t.ServiceAccountID != nil {
		sess.Provider = serviceAccountProvider
		sess.ServiceAdmin = t.ServiceAdmin
	}
	if t.ExpiresAt != nil {
		sess.Expires = *t.ExpiresAt
	}
	return sess
}

func (s *Server) registerAPITokenRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/api-tokens", s.listAPITokens)
	m.HandleFunc("POST /api/v1/api-tokens", s.createAPIToken)
	m.HandleFunc("POST /api/v1/api-tokens/{id}/revoke", s.revokeAPIToken)
	m.HandleFunc("GET /api/v1/api-token-policy", s.getTokenPolicy)
	m.HandleFunc("PUT /api/v1/api-token-policy", s.putTokenPolicy)
}

// listAPITokens lists the caller's own tokens; an admin may pass ?all=1 to see
// everyone's (to find and revoke a leaked one).
func (s *Server) listAPITokens(w http.ResponseWriter, r *http.Request) {
	owner := s.sessionActor(r)
	if r.URL.Query().Get("all") == "1" {
		if !s.requireAdmin(w, r) {
			return
		}
		owner = ""
	} else if owner == "" {
		writeJSON(w, http.StatusOK, []store.APIToken{})
		return
	}
	tokens, err := s.store.ListAPITokens(r.Context(), owner, s.tokenPolicy(r.Context()))
	if err != nil {
		slog.Error("list api tokens", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list API tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// createAPIToken mints a token for the signed-in user. It needs a real SSO
// session: a token cannot mint further tokens (so a leaked short-lived token
// cannot be upgraded to a permanent one), and in open bootstrap mode there is
// no identity to act on behalf of — nor any need for a token.
func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeErr(w, http.StatusBadRequest,
			"API tokens act on behalf of a signed-in user — enable SSO and sign in first")
		return
	}
	if sess.Provider == apiTokenProvider || sess.Provider == serviceAccountProvider {
		writeErr(w, http.StatusForbidden, "API tokens cannot create other tokens — sign in to the UI")
		return
	}
	var req struct {
		Description   string `json:"description"`
		ExpiresInDays int    `json:"expires_in_days"`
		NoExpiry      bool   `json:"no_expiry"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	policy := s.tokenPolicy(r.Context())
	expiresAt, err := tokenExpiry(req.NoExpiry, req.ExpiresInDays, policy, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.store.CreateAPIToken(r.Context(), sess.Email, sess.Name,
		strings.TrimSpace(req.Description), expiresAt)
	if err != nil {
		slog.Error("create api token", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create API token")
		return
	}
	// NEVER record the raw token; the last-4 suffix is safe metadata.
	s.audit(r, "api-token.create", strconv.FormatInt(t.ID, 10), "", "ok", map[string]any{
		"description":  t.Description,
		"token_suffix": t.TokenSuffix,
		"no_expiry":    expiresAt == nil,
	})
	writeJSON(w, http.StatusCreated, t)
}

// tokenExpiry validates a requested lifetime against policy and returns the
// expiry (nil = never).
func tokenExpiry(noExpiry bool, days int, p store.TokenPolicy, now time.Time) (*time.Time, error) {
	if noExpiry {
		if !p.AllowNoExpiry {
			return nil, errors.New("non-expiring tokens are disabled by your admin — pick a lifetime")
		}
		if p.MaxLifetimeDays > 0 {
			return nil, errors.New("tokens are limited to " + strconv.Itoa(p.MaxLifetimeDays) +
				" days by your admin — pick a lifetime")
		}
		return nil, nil
	}
	if days < 0 {
		return nil, errors.New("expires_in_days must be positive")
	}
	if days == 0 {
		days = defaultTokenLifetime
		if p.MaxLifetimeDays > 0 && days > p.MaxLifetimeDays {
			days = p.MaxLifetimeDays
		}
	}
	if p.MaxLifetimeDays > 0 && days > p.MaxLifetimeDays {
		return nil, errors.New("tokens are limited to " + strconv.Itoa(p.MaxLifetimeDays) + " days by your admin")
	}
	exp := now.AddDate(0, 0, days)
	return &exp, nil
}

// revokeAPIToken revokes one of the caller's tokens; admins may revoke any.
func (s *Server) revokeAPIToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid token id")
		return
	}
	owner := s.sessionActor(r)
	if s.isAdmin(r) {
		owner = ""
	} else if owner == "" {
		writeErr(w, http.StatusUnauthorized, "sign in to revoke tokens")
		return
	}
	t, err := s.store.RevokeAPIToken(r.Context(), id, owner)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "API token not found")
		return
	}
	if err != nil {
		slog.Error("revoke api token", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to revoke API token")
		return
	}
	s.audit(r, "api-token.revoke", strconv.FormatInt(t.ID, 10), "", "ok", map[string]any{
		"owner":        t.Owner,
		"token_suffix": t.TokenSuffix,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getTokenPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetTokenPolicy(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load token policy")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) putTokenPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var p store.TokenPolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.MaxLifetimeDays < 0 {
		writeErr(w, http.StatusBadRequest, "max_lifetime_days must be 0 (no cap) or positive")
		return
	}
	if err := s.store.SetTokenPolicy(r.Context(), p); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save token policy")
		return
	}
	// Apply immediately on this replica rather than after the cache window.
	s.policy.mu.Lock()
	s.policy.val, s.policy.checked = p, time.Now()
	s.policy.mu.Unlock()
	s.audit(r, "api-token.policy", "", "", "ok", map[string]any{
		"allow_no_expiry":   p.AllowNoExpiry,
		"max_lifetime_days": p.MaxLifetimeDays,
	})
	writeJSON(w, http.StatusOK, p)
}
