package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Platform admins, managed from the Admin UI. Admin comes from three places:
// ADMIN_EMAILS (server config: the bootstrap / break-glass list, read-only
// here), grants made on this page (platform_admins), and IdP groups mapped to
// platform admin (Sign-in & identity). Only a person signed in through the
// browser may grant or revoke, never an API token.

func (s *Server) registerPlatformAdminRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/platform-admins", s.listPlatformAdmins)
	m.HandleFunc("POST /api/v1/platform-admins", s.addPlatformAdmin)
	m.HandleFunc("DELETE /api/v1/platform-admins/{email}", s.removePlatformAdmin)
}

// platformAdminEntry is one row of the admins page.
type platformAdminEntry struct {
	Email     string     `json:"email"`
	Source    string     `json:"source"` // "env" (ADMIN_EMAILS) or "ui"
	AddedBy   string     `json:"added_by,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// adminGroupEntry is an IdP group that grants platform admin.
type adminGroupEntry struct {
	Group    string `json:"group"`
	Provider string `json:"provider"`
}

func (s *Server) listPlatformAdmins(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	granted, err := s.store.ListPlatformAdmins(r.Context())
	if err != nil {
		slog.Error("list platform admins", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list platform admins")
		return
	}
	mappings, err := s.store.ListGroupMappings(r.Context())
	if err != nil {
		slog.Error("list group mappings", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list platform admins")
		return
	}

	env := adminEmails()
	admins := []platformAdminEntry{}
	for e := range env {
		admins = append(admins, platformAdminEntry{Email: e, Source: "env"})
	}
	for _, a := range granted {
		if env[a.Email] {
			continue // already shown as an ADMIN_EMAILS admin
		}
		admins = append(admins, platformAdminEntry{Email: a.Email, Source: "ui", AddedBy: a.AddedBy, CreatedAt: &a.CreatedAt})
	}
	sort.Slice(admins, func(i, j int) bool { return admins[i].Email < admins[j].Email })

	groups := []adminGroupEntry{}
	for _, m := range mappings {
		if m.PlatformAdmin {
			groups = append(groups, adminGroupEntry{Group: m.Group, Provider: m.Provider})
		}
	}
	me := ""
	if sess := s.currentSession(r); sess != nil {
		me = strings.ToLower(sess.Email)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"admins":       admins,
		"admin_groups": groups,
		"me":           me,
		"sso_enforced": s.ssoEnforced(r.Context()),
	})
}

// requireBrowserAdmin: granting or revoking admin needs an admin signed in as
// a person — not an API token or a service account, so a leaked token can't
// mint admins.
func (s *Server) requireBrowserAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAdmin(w, r) {
		return false
	}
	if sess := s.currentSession(r); sess != nil && (sess.Provider == apiTokenProvider || sess.Provider == serviceAccountProvider) {
		writeErr(w, http.StatusForbidden, "manage platform admins from a signed-in browser session, not with a token")
		return false
	}
	return true
}

// normalizeAdminEmail validates a bare address (no display name) and lower-cases it.
func normalizeAdminEmail(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Address != raw || a.Name != "" {
		return "", false
	}
	return strings.ToLower(raw), true
}

// grantAdmin makes email (already normalized) a platform admin from the UI.
// On refusal it returns the HTTP status and message to send.
func (s *Server) grantAdmin(r *http.Request, email string) (*store.PlatformAdmin, int, string) {
	if adminEmails()[email] {
		return nil, http.StatusConflict, email + " is already an admin through ADMIN_EMAILS"
	}
	a, err := s.store.AddPlatformAdmin(r.Context(), email, s.sessionActor(r))
	if errors.Is(err, store.ErrAlreadyAdmin) {
		return nil, http.StatusConflict, email + " is already a platform admin"
	}
	if err != nil {
		slog.Error("add platform admin", "err", err)
		return nil, http.StatusInternalServerError, "failed to add platform admin"
	}
	s.audit(r, "platform-admin.add", email, "", "ok", nil)
	return a, 0, ""
}

// revokeAdmin removes a UI-granted platform admin. ADMIN_EMAILS admins, the
// caller themself, and the last admin are refused.
func (s *Server) revokeAdmin(r *http.Request, email string) (int, string) {
	if adminEmails()[email] {
		return http.StatusConflict, email + " is an admin through ADMIN_EMAILS; remove it from the server config instead"
	}
	if email == strings.ToLower(s.sessionActor(r)) {
		return http.StatusConflict, "you can't remove yourself; ask another admin"
	}
	// Never leave the platform without an admin: once SSO is enforced nobody
	// could get back into the admin pages.
	if len(adminEmails()) == 0 {
		granted, err := s.store.ListPlatformAdmins(r.Context())
		if err != nil {
			return http.StatusInternalServerError, "failed to remove platform admin"
		}
		groups, err := s.store.HasGroupAdminMapping(r.Context())
		if err != nil {
			return http.StatusInternalServerError, "failed to remove platform admin"
		}
		if !groups && len(granted) == 1 && granted[0].Email == email {
			return http.StatusConflict, "this is the last platform admin; add another one first"
		}
	}
	if err := s.store.RemovePlatformAdmin(r.Context(), email); errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound, email + " is not an admin granted here"
	} else if err != nil {
		slog.Error("remove platform admin", "err", err)
		return http.StatusInternalServerError, "failed to remove platform admin"
	}
	s.audit(r, "platform-admin.remove", email, "", "ok", nil)
	return 0, ""
}

func (s *Server) addPlatformAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireBrowserAdmin(w, r) {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email, ok := normalizeAdminEmail(body.Email)
	if !ok {
		writeErr(w, http.StatusBadRequest, "enter one email address, e.g. alice@acme.io")
		return
	}
	a, code, msg := s.grantAdmin(r, email)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusCreated, platformAdminEntry{Email: a.Email, Source: "ui", AddedBy: a.AddedBy, CreatedAt: &a.CreatedAt})
}

func (s *Server) removePlatformAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireBrowserAdmin(w, r) {
		return
	}
	if code, msg := s.revokeAdmin(r, strings.ToLower(strings.TrimSpace(r.PathValue("email")))); code != 0 {
		writeErr(w, code, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
