package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/notify"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Notification routes (admin) and act-by-link approvals.

func (s *Server) registerNotifyRouteRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/notification-routes", s.listNotifyRoutes)
	m.HandleFunc("POST /api/v1/notification-routes", s.saveNotifyRoute)
	m.HandleFunc("PUT /api/v1/notification-routes/{id}", s.saveNotifyRoute)
	m.HandleFunc("DELETE /api/v1/notification-routes/{id}", s.deleteNotifyRoute)
	m.HandleFunc("POST /api/v1/notification-routes/{id}/test", s.testNotifyRoute)
	m.HandleFunc("GET /api/v1/approvals/link", s.approvalLinkPage)
	m.HandleFunc("POST /api/v1/approvals/link", s.approvalLinkAct)
}

func (s *Server) listNotifyRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	routes, err := s.store.ListNotificationRoutes(r.Context(), false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list routes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routes": routes, "kinds": append(notify.Kinds(), "email"), "events": notify.EventKinds,
		"links_enabled": s.store.Cipher().HasKey(),
	})
}

func validateRoute(r *store.NotificationRoute, isNew bool) error {
	r.Name = strings.TrimSpace(r.Name)
	r.Target = strings.TrimSpace(r.Target)
	r.RepoPattern = strings.TrimSpace(r.RepoPattern)
	if r.Name == "" {
		return errors.New("name is required")
	}
	if len(r.Events) == 0 {
		return errors.New("pick at least one event")
	}
	for _, e := range r.Events {
		known := false
		for _, k := range notify.EventKinds {
			known = known || k == e
		}
		if !known {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	if r.Kind == "email" {
		if isNew && r.Target == "" {
			return errors.New(`target is a comma-separated list of addresses, or "approvers"`)
		}
		for _, t := range strings.Split(r.Target, ",") {
			if t = strings.TrimSpace(t); t != "" && t != "approvers" && !strings.Contains(t, "@") {
				return fmt.Errorf("%q is not an email address", t)
			}
		}
		return nil
	}
	n, ok := notify.Lookup(r.Kind)
	if !ok {
		return fmt.Errorf("kind must be one of %s, email", strings.Join(notify.Kinds(), ", "))
	}
	if r.Target == "" {
		if isNew {
			return errors.New("target is required")
		}
		return nil // keep the stored one
	}
	return n.ValidateTarget(r.Target)
}

func (s *Server) saveNotifyRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in store.NotificationRoute
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.ID = 0
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid id")
			return
		}
		in.ID = id
	}
	if err := validateRoute(&in, in.ID == 0); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.store.SaveNotificationRoute(r.Context(), in)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "route not found")
		return
	}
	if err != nil {
		slog.Error("save notification route", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save route")
		return
	}
	// Never the target or secret.
	s.audit(r, "notifications.route.save", out.Name, "", "ok", map[string]any{
		"id": out.ID, "kind": out.Kind, "events": out.Events, "repo_pattern": out.RepoPattern, "enabled": out.Enabled,
	})
	code := http.StatusOK
	if r.Method == http.MethodPost {
		code = http.StatusCreated
	}
	writeJSON(w, code, out)
}

func (s *Server) deleteNotifyRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteNotificationRoute(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "route not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete route")
		return
	}
	s.audit(r, "notifications.route.delete", strconv.FormatInt(id, 10), "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

// testNotifyRoute sends a sample event through one route.
func (s *Server) testNotifyRoute(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	routes, err := s.store.ListNotificationRoutes(r.Context(), true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load routes")
		return
	}
	var route *store.NotificationRoute
	for i := range routes {
		if routes[i].ID == id {
			route = &routes[i]
		}
	}
	if route == nil {
		writeErr(w, http.StatusNotFound, "route not found")
		return
	}
	me := s.sessionActor(r)
	ev := notify.Event{
		Kind: route.Events[0], Repo: "forge/test", Ref: "main", SHA: "0000000000", PipelineID: 0,
		Title: "Test notification from Forge", Text: "This is a test of the route " + route.Name + ".",
		Link: frontendURL(), Fields: map[string]string{"Sent by": me}, At: time.Now(), Approvers: []string{me},
	}
	legacy, _ := s.store.GetNotifySettings(r.Context(), true)
	route.Enabled = true
	if err := (&notify.Sender{}).Dispatch(r.Context(), []store.NotificationRoute{*route}, nil, ev); err != nil && route.Kind != "email" {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	} else if route.Kind == "email" {
		if err := (&notify.Sender{}).Dispatch(r.Context(), []store.NotificationRoute{*route}, legacy, ev); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

// ---- act by link ----

const linkPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Forge · %s</title>
<style>body{font-family:system-ui,sans-serif;background:#0f1216;color:#e6e8eb;display:flex;justify-content:center;padding:48px 16px}
.card{max-width:460px;width:100%%;background:#171b21;border:1px solid #262c35;border-radius:14px;padding:24px}
h1{font-size:18px;margin:0 0 8px}p{margin:8px 0;color:#aab2bd}code{color:#e6e8eb}
button{font:inherit;padding:10px 16px;border-radius:10px;border:0;cursor:pointer;background:%s;color:#fff;margin-top:12px}
a{color:#7aa2ff}</style>
<div class="card"><h1>%s</h1>%s</div>`

func (s *Server) approvalLinkPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	token := r.URL.Query().Get("t")
	jobID, approver, verdict, err := notify.ParseApprovalLink(s.store.Cipher(), token)
	if err != nil {
		fmt.Fprintf(w, linkPage, "link", "#444", "Can't act on this link", "<p>"+html.EscapeString(err.Error())+"</p>")
		return
	}
	job, jerr := s.store.GetJob(r.Context(), jobID)
	if jerr != nil {
		fmt.Fprintf(w, linkPage, "link", "#444", "Job not found", "<p>This job no longer exists.</p>")
		return
	}
	env := ""
	if job.Environment != nil {
		env = *job.Environment
	}
	colour, label := "#1a7f37", "Approve"
	if verdict == "rejected" {
		colour, label = "#cf222e", "Reject"
	}
	// The GET only shows a confirmation: mail scanners and link previews
	// follow GET links, and must never cast a vote.
	body := fmt.Sprintf(`<p>You are <b>%s</b>.</p><p>Job <code>%s</code> wants to deploy to <code>%s</code> (status: %s).</p>
<form method="post" action="/api/v1/approvals/link"><input type="hidden" name="t" value="%s"><button type="submit">%s this deploy</button></form>
<p><a href="%s/jobs/%d?pipeline=%d">Open in Forge instead</a></p>`,
		html.EscapeString(approver), html.EscapeString(job.Name), html.EscapeString(env), html.EscapeString(job.Status),
		html.EscapeString(token), label, html.EscapeString(frontendURL()), job.ID, job.PipelineID)
	fmt.Fprintf(w, linkPage, label, colour, label+" "+html.EscapeString(job.Name)+"?", body)
}

func (s *Server) approvalLinkAct(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad form")
		return
	}
	jobID, approver, verdict, err := notify.ParseApprovalLink(s.store.Cipher(), r.Form.Get("t"))
	if err != nil {
		fmt.Fprintf(w, linkPage, "link", "#444", "Can't act on this link", "<p>"+html.EscapeString(err.Error())+"</p>")
		return
	}
	job, err := s.store.Approve(r.Context(), jobID, proto.ApprovalRequest{Approver: approver, Verdict: verdict, Comment: "via email link"})
	if err != nil {
		s.auditAs(r, approver, "approval.link", strconv.FormatInt(jobID, 10), "", "denied", map[string]any{"verdict": verdict, "error": err.Error()})
		fmt.Fprintf(w, linkPage, "link", "#444", "Not recorded", "<p>"+html.EscapeString(err.Error())+"</p>")
		return
	}
	repo, _ := s.store.JobRepo(r.Context(), jobID)
	s.auditAs(r, approver, "approval."+verdict, strconv.FormatInt(jobID, 10), repo, "ok", map[string]any{"via": "email link"})
	msg := fmt.Sprintf("<p>Your vote (<b>%s</b>) on <code>%s</code> is recorded. The job is now <code>%s</code>.</p><p><a href=\"%s/jobs/%d?pipeline=%d\">Open in Forge</a></p>",
		html.EscapeString(verdict), html.EscapeString(job.Name), html.EscapeString(job.Status), html.EscapeString(frontendURL()), job.ID, job.PipelineID)
	fmt.Fprintf(w, linkPage, "recorded", "#1a7f37", "Recorded", msg)
}
