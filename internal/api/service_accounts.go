package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Service accounts (admin only): identities for automation that belong to the
// org, not to a person, so a token keeps working when someone leaves. A
// service account acts as "svc:<name>": add that as a repo member to give it
// a role, or mark it admin for platform-wide rights.

func (s *Server) registerServiceAccountRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/service-accounts", s.listServiceAccounts)
	m.HandleFunc("POST /api/v1/service-accounts", s.createServiceAccount)
	m.HandleFunc("PATCH /api/v1/service-accounts/{id}", s.updateServiceAccount)
	m.HandleFunc("DELETE /api/v1/service-accounts/{id}", s.deleteServiceAccount)
	m.HandleFunc("GET /api/v1/service-accounts/{id}/tokens", s.listServiceTokens)
	m.HandleFunc("POST /api/v1/service-accounts/{id}/tokens", s.createServiceToken)
}

// requireHumanAdmin: managing service accounts needs an admin signed in as a
// person — a service account (even an admin one) cannot mint more of itself.
func (s *Server) requireHumanAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAdmin(w, r) {
		return false
	}
	if sess := s.currentSession(r); sess != nil && (sess.Provider == apiTokenProvider || sess.Provider == serviceAccountProvider) {
		writeErr(w, http.StatusForbidden, "manage service accounts from a signed-in browser session, not with a token")
		return false
	}
	return true
}

func (s *Server) loadServiceAccount(w http.ResponseWriter, r *http.Request) *store.ServiceAccount {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid service account id")
		return nil
	}
	a, err := s.store.GetServiceAccount(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "service account not found")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load service account")
		return nil
	}
	return a
}

func (s *Server) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out, err := s.store.ListServiceAccounts(r.Context())
	if err != nil {
		slog.Error("list service accounts", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list service accounts")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanAdmin(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		IsAdmin     bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	a, err := s.store.CreateServiceAccount(r.Context(), req.Name, req.Description, req.IsAdmin, s.actorLabel(r))
	switch {
	case errors.Is(err, store.ErrInvalidServiceName):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrDuplicateService):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		slog.Error("create service account", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create service account")
		return
	}
	s.audit(r, "service-account.create", a.Identity, "", "ok", map[string]any{"is_admin": a.IsAdmin})
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) updateServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanAdmin(w, r) {
		return
	}
	a := s.loadServiceAccount(w, r)
	if a == nil {
		return
	}
	var req struct {
		Description *string `json:"description"`
		IsAdmin     *bool   `json:"is_admin"`
		Disabled    *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	out, err := s.store.UpdateServiceAccount(r.Context(), a.ID, req.Description, req.IsAdmin, req.Disabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update service account")
		return
	}
	s.audit(r, "service-account.update", out.Identity, "", "ok", map[string]any{
		"is_admin": out.IsAdmin, "disabled": out.Disabled,
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteServiceAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanAdmin(w, r) {
		return
	}
	a := s.loadServiceAccount(w, r)
	if a == nil {
		return
	}
	if err := s.store.DeleteServiceAccount(r.Context(), a.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete service account")
		return
	}
	s.audit(r, "service-account.delete", a.Identity, "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listServiceTokens(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	a := s.loadServiceAccount(w, r)
	if a == nil {
		return
	}
	tokens, err := s.store.ListServiceTokens(r.Context(), a, s.tokenPolicy(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// createServiceToken mints a token for the account, under the same admin
// token policy as personal tokens (non-expiring only when allowed).
func (s *Server) createServiceToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireHumanAdmin(w, r) {
		return
	}
	a := s.loadServiceAccount(w, r)
	if a == nil {
		return
	}
	if a.Disabled {
		writeErr(w, http.StatusConflict, "service account is disabled")
		return
	}
	var req struct {
		Description   string `json:"description"`
		ExpiresInDays int    `json:"expires_in_days"`
		NoExpiry      bool   `json:"no_expiry"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	exp, err := tokenExpiry(req.NoExpiry, req.ExpiresInDays, s.tokenPolicy(r.Context()), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := s.store.CreateServiceToken(r.Context(), a, strings.TrimSpace(req.Description), exp)
	if err != nil {
		slog.Error("create service token", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create token")
		return
	}
	s.audit(r, "service-account.token-create", a.Identity, "", "ok", map[string]any{
		"token_id": strconv.FormatInt(t.ID, 10), "token_suffix": t.TokenSuffix, "no_expiry": exp == nil,
	})
	writeJSON(w, http.StatusCreated, t)
}
