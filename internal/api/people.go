package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Admin → People: everyone who has signed in, with their platform role and
// repo roles, searchable, and a platform-role switch per person. The switch
// grants and revokes exactly like the platform-admin endpoints (same guards,
// same audit actions); admins from ADMIN_EMAILS or an IdP group are locked.

func (s *Server) registerPeopleRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/people", s.listPeople)
	m.HandleFunc("PUT /api/v1/people/{email}/platform-role", s.setPlatformRole)
}

// listPeople returns a page of people as a JSON array. ?q searches email and
// name, ?role=admin keeps platform admins, ?limit (default 50, max 200) and
// ?offset page; X-Total-Count and X-Has-More are set like listPipelines.
func (s *Server) listPeople(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit, offset := pageParams(r)
	q := store.PeopleQuery{
		Search:     r.URL.Query().Get("q"),
		AdminsOnly: r.URL.Query().Get("role") == "admin",
		Limit:      limit, Offset: offset,
	}
	env := make([]string, 0, len(adminEmails()))
	for e := range adminEmails() {
		env = append(env, e)
	}
	sort.Strings(env)
	people, total, err := s.store.ListPeople(r.Context(), q, env)
	if err != nil {
		slog.Error("list people", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list people")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(people) < total))
	writeJSON(w, http.StatusOK, people)
}

// setPlatformRole sets a person's platform role to "admin" or "member".
func (s *Server) setPlatformRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireBrowserAdmin(w, r) {
		return
	}
	email, ok := normalizeAdminEmail(r.PathValue("email"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch strings.ToLower(body.Role) {
	case "admin":
		if _, code, msg := s.grantAdmin(r, email); code != 0 {
			writeErr(w, code, msg)
			return
		}
	case "member":
		if code, msg := s.revokeAdmin(r, email); code != 0 {
			if code == http.StatusNotFound {
				msg = email + " is not an admin granted here; admins from an IdP group are changed in Sign-in & identity"
			}
			writeErr(w, code, msg)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, `role must be "admin" or "member"`)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "role": strings.ToLower(body.Role)})
}
