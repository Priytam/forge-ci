package api

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// clientIP extracts the caller's IP for the audit trail. X-Forwarded-For (first
// hop) is preferred when present — Forge is expected to run behind a reverse
// proxy — otherwise the connection's RemoteAddr (host part) is used.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := xff
		if i := strings.IndexByte(xff, ','); i >= 0 {
			first = xff[:i]
		}
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// actorLabel returns the identity to record for a request:
//   - a live session → the authenticated email (the trusted identity, never a
//     client-asserted value);
//   - no session, SSO enforced → "anonymous" (e.g. a denied unauthenticated
//     mutation);
//   - no session, open bootstrap mode → "bootstrap".
func (s *Server) actorLabel(r *http.Request) string {
	if sess := s.currentSession(r); sess != nil {
		return sess.Email
	}
	if s.ssoEnforced(r.Context()) {
		return "anonymous"
	}
	return "bootstrap"
}

// audit records one mutating admin/settings action, deriving actor, source IP
// and (default) result from the request. Best-effort: a failed audit write is
// logged and swallowed so it never fails the underlying request.
func (s *Server) audit(r *http.Request, action, target, repo, result string, detail map[string]any) {
	s.auditAs(r, s.actorLabel(r), action, target, repo, result, detail)
}

// auditAs is audit with an explicit actor, for the few call sites where the
// acting identity is not simply the session/bootstrap label (e.g. an approval
// vote whose actor is the resolved approver).
func (s *Server) auditAs(r *http.Request, actor, action, target, repo, result string, detail map[string]any) {
	e := store.AuditEntry{
		Actor:    actor,
		Action:   action,
		Target:   target,
		Repo:     repo,
		Detail:   detail,
		SourceIP: clientIP(r),
		Result:   result,
	}
	if err := s.store.InsertAudit(r.Context(), e); err != nil {
		slog.Error("audit write failed", "err", err, "action", action, "target", target)
	}
}

// registerAuditRoutes wires the read API for the audit trail (admin-only).
func (s *Server) registerAuditRoutes() {
	s.mux.HandleFunc("GET /api/v1/audit-log", s.listAuditLog)
}

// listAuditLog returns a page of audit entries, newest first, admin-only.
// Filterable by ?repo and ?actor; paginated by ?limit (default 50, cap 200) and
// ?offset. Secret detail is never stored, so responses are secret-free.
func (s *Server) listAuditLog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	limit, offset := pageParams(r)
	entries, err := s.store.ListAudit(r.Context(), store.AuditFilter{
		Repo:   r.URL.Query().Get("repo"),
		Actor:  r.URL.Query().Get("actor"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		slog.Error("list audit log", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list audit log")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
