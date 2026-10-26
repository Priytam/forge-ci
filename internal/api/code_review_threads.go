package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Code review threads (phase D): comments on lines and on the change, made
// in Forge or on the host, kept in sync both ways, and the host accounts
// people link so Forge writes as them.

func (s *Server) registerThreadRoutes() {
	s.mux.HandleFunc("POST /api/v1/changes/{id}/threads", s.createThread)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/threads/{tid}/comments", s.replyThread)
	s.mux.HandleFunc("POST /api/v1/changes/{id}/threads/{tid}/resolve", s.resolveThread)
	s.mux.HandleFunc("GET /api/v1/auth/link/github", s.linkGitHubStart)
	s.mux.HandleFunc("GET /api/v1/auth/callback/github/link", s.linkGitHubCallback)
	s.mux.HandleFunc("GET /api/v1/account/links", s.listHostLinks)
	s.mux.HandleFunc("DELETE /api/v1/account/links/{provider}", s.unlinkHost)
}

// commenter is who is writing: the session, or (SSO off) the named email.
func (s *Server) commenter(r *http.Request, claimed string) (name, email string) {
	if sess := s.currentSession(r); sess != nil {
		name = sess.Name
		if name == "" {
			name = sess.Email
		}
		return name, strings.ToLower(sess.Email)
	}
	if s.ssoEnforced(r.Context()) {
		return "", ""
	}
	email = strings.ToLower(strings.TrimSpace(claimed))
	if email == "" {
		email = "anonymous"
	}
	return email, email
}

// threadChange loads the change a thread write targets and checks the caller
// may write on its repo.
func (s *Server) threadChange(w http.ResponseWriter, r *http.Request) (*store.ReviewTargetChange, bool) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return nil, false
	}
	c, err := s.store.ChangeForAction(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return nil, false
	}
	if !s.requireOperate(w, r, c.Repo) {
		return nil, false
	}
	return c, true
}

// maxCommentBytes bounds a comment (GitHub caps bodies at 65,536 characters).
const maxCommentBytes = 60000

// createThread: POST /api/v1/changes/{id}/threads {path, side, line,
// start_line, body} for a line comment, or {body} for the conversation.
// Saved at once; sent to the host through the outbox when it has threads.
func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	c, ok := s.threadChange(w, r)
	if !ok {
		return
	}
	var req struct {
		Path      string `json:"path"`
		Side      string `json:"side"`
		Line      int    `json:"line"`
		StartLine int    `json:"start_line"`
		Body      string `json:"body"`
		Author    string `json:"author"` // SSO off only
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" || len(req.Body) > maxCommentBytes {
		writeErr(w, http.StatusBadRequest, "body is required (at most 60,000 characters)")
		return
	}
	name, email := s.commenter(r, req.Author)
	if email == "" {
		writeErr(w, http.StatusUnauthorized, "sign in to comment")
		return
	}
	sync := vcs.CapabilitiesOf(c.Provider).Threads
	a := store.NewThreadAnchor{Path: strings.TrimPrefix(req.Path, "/"), Side: req.Side, Line: req.Line, StartLine: req.StartLine}
	if a.Path != "" {
		if a.Side == "" {
			a.Side = "new"
		}
		d, err := s.store.GetChangeDetail(r.Context(), c.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to load change")
			return
		}
		// The line is on the head (new side) or on the target (old side).
		a.CommitSHA, a.AnchorHead = d.HeadSHA, d.HeadSHA
		if a.Side == "old" {
			a.CommitSHA = d.BaseSHA
		}
		if !sync {
			a.Fingerprint = s.lineText(r.Context(), d, a.Path, a.Side, a.Line)
		}
	}
	tid, cid, _, err := s.store.CreateThread(r.Context(), c.ID, a, store.NewComment{Author: name, AuthorEmail: email, Body: req.Body, Sync: sync})
	if errors.Is(err, store.ErrThreadLine) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		slog.Error("code review: create thread", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save comment")
		return
	}
	if sync {
		if err := review.QueueComment(r.Context(), s.store, c.ID, c.Repo, cid, email); err != nil {
			slog.Error("code review: queue comment", "err", err)
		}
	}
	s.audit(r, "change.comment", c.Repo+"#"+c.Number, c.Repo, "ok", map[string]any{"thread": tid, "path": a.Path, "line": a.Line})
	writeJSON(w, http.StatusCreated, map[string]any{"thread_id": tid, "comment_id": cid})
}

// replyThread: POST /api/v1/changes/{id}/threads/{tid}/comments {body}.
func (s *Server) replyThread(w http.ResponseWriter, r *http.Request) {
	c, ok := s.threadChange(w, r)
	if !ok {
		return
	}
	tid, err := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	var req struct {
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Body) == "" || len(req.Body) > maxCommentBytes {
		writeErr(w, http.StatusBadRequest, "body is required (at most 60,000 characters)")
		return
	}
	name, email := s.commenter(r, req.Author)
	if email == "" {
		writeErr(w, http.StatusUnauthorized, "sign in to comment")
		return
	}
	sync := vcs.CapabilitiesOf(c.Provider).Threads
	cid, err := s.store.AddReply(r.Context(), c.ID, tid, store.NewComment{Author: name, AuthorEmail: email, Body: req.Body, Sync: sync})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "thread not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save comment")
		return
	}
	if sync {
		if err := review.QueueComment(r.Context(), s.store, c.ID, c.Repo, cid, email); err != nil {
			slog.Error("code review: queue reply", "err", err)
		}
	}
	s.audit(r, "change.comment", c.Repo+"#"+c.Number, c.Repo, "ok", map[string]any{"thread": tid})
	writeJSON(w, http.StatusCreated, map[string]any{"comment_id": cid})
}

// resolveThread: POST /api/v1/changes/{id}/threads/{tid}/resolve {resolved}.
func (s *Server) resolveThread(w http.ResponseWriter, r *http.Request) {
	c, ok := s.threadChange(w, r)
	if !ok {
		return
	}
	tid, err := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid thread id")
		return
	}
	var req struct {
		Resolved bool `json:"resolved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_, email := s.commenter(r, "")
	sync := vcs.CapabilitiesOf(c.Provider).Resolve
	if err := s.store.SetThreadResolved(r.Context(), c.ID, tid, req.Resolved, email, sync); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "thread not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save")
		return
	}
	if sync {
		if err := review.QueueResolve(r.Context(), s.store, c.ID, c.Repo, tid, email); err != nil {
			slog.Error("code review: queue resolve", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread_id": tid, "resolved": req.Resolved})
}

// lineText is the text of a line in the change's stored diff ("" when it is
// not there), the fingerprint Forge re-anchors a thread by.
func (s *Server) lineText(ctx context.Context, d *store.ChangeDetail, path, side string, line int) string {
	row, err := s.store.GetDiff(ctx, store.DiffKey{Repo: d.Repo, From: d.BaseSHA, To: d.HeadSHA, Mode: "change"})
	if err != nil || row.State != "ready" || s.blobs == nil {
		return ""
	}
	rc, err := s.blobs.Get(ctx, row.BlobPrefix+"/manifest.json")
	if err != nil {
		return ""
	}
	defer rc.Close()
	var m review.DiffManifest
	if json.NewDecoder(rc).Decode(&m) != nil {
		return ""
	}
	for _, f := range m.Files {
		if f.Path == path {
			return review.LineText(f.Patch, side, line)
		}
	}
	return ""
}

// ---- linking a host account ----

// githubLinkApp is the OAuth app used to link GitHub accounts: its own
// (GITHUB_LINK_CLIENT_ID / _SECRET) or the GitHub sign-in app.
func (s *Server) githubLinkApp(ctx context.Context) (id, secret string, ok bool) {
	if id, secret = os.Getenv("GITHUB_LINK_CLIENT_ID"), os.Getenv("GITHUB_LINK_CLIENT_SECRET"); id != "" && secret != "" {
		return id, secret, true
	}
	cfg, err := s.store.GetEnabledSSOProvider(ctx, "github")
	if err != nil || cfg.ClientID == "" {
		return "", "", false
	}
	return cfg.ClientID, cfg.ClientSecret, true
}

func githubOAuthBase() string {
	if v := os.Getenv("GITHUB_OAUTH_BASE"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://github.com"
}

func githubAPIBase() string {
	if v := os.Getenv("GITHUB_API_BASE"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://api.github.com"
}

// linkRedirect sits under the sign-in callback path, so one GitHub OAuth app
// registration (callback .../auth/callback/github) serves both.
func linkRedirect() string { return externalURL() + "/api/v1/auth/callback/github/link" }

const linkStateCookie = "forge_link_state"

// linkGitHubStart sends a signed-in user to GitHub to link their account,
// asking for the repo scope Forge needs to comment, review and merge as them.
func (s *Server) linkGitHubStart(w http.ResponseWriter, r *http.Request) {
	if s.currentSession(r) == nil {
		writeErr(w, http.StatusUnauthorized, "sign in to Forge first")
		return
	}
	id, _, ok := s.githubLinkApp(r.Context())
	if !ok {
		writeErr(w, http.StatusNotFound, "GitHub account linking is not configured (set GITHUB_LINK_CLIENT_ID/_SECRET, or enable GitHub sign-in)")
		return
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	state := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: linkStateCookie, Value: state, Path: "/api/v1/auth", MaxAge: 600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(externalURL(), "https://")})
	q := url.Values{"client_id": {id}, "redirect_uri": {linkRedirect()}, "scope": {"repo"}, "state": {state}}
	http.Redirect(w, r, githubOAuthBase()+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

// linkGitHubCallback stores the user's token (encrypted) and maps their
// login to them, then returns to their account page.
func (s *Server) linkGitHubCallback(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "sign in to Forge first")
		return
	}
	st, err := r.Cookie(linkStateCookie)
	if err != nil || st.Value == "" || r.URL.Query().Get("state") != st.Value {
		writeErr(w, http.StatusBadRequest, "state mismatch — start linking again")
		return
	}
	id, secret, ok := s.githubLinkApp(r.Context())
	if !ok {
		writeErr(w, http.StatusNotFound, "GitHub account linking is not configured")
		return
	}
	token, err := exchangeCode(r.Context(), githubOAuthBase()+"/login/oauth/access_token", id, secret,
		r.URL.Query().Get("code"), linkRedirect())
	if err != nil {
		slog.Error("link github: token exchange", "err", err)
		writeErr(w, http.StatusBadGateway, "GitHub did not grant access")
		return
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := getJSON(r.Context(), githubAPIBase()+"/user", token, &user); err != nil || user.Login == "" {
		writeErr(w, http.StatusBadGateway, "could not read your GitHub login")
		return
	}
	if err := s.store.LinkHostAccount(r.Context(), sess.Email, "github", user.Login, token, "repo"); err != nil {
		slog.Error("link github: save", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save the link")
		return
	}
	s.audit(r, "account.link", "github:"+user.Login, "", "ok", nil)
	http.SetCookie(w, &http.Cookie{Name: linkStateCookie, Path: "/api/v1/auth", MaxAge: -1})
	http.Redirect(w, r, frontendURL()+"/account?linked=github", http.StatusFound)
}

func (s *Server) listHostLinks(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"links": []store.HostLink{}, "github_linking": false})
		return
	}
	links, err := s.store.HostLinks(r.Context(), sess.Email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list links")
		return
	}
	_, _, ok := s.githubLinkApp(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"links": links, "github_linking": ok})
}

func (s *Server) unlinkHost(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		writeErr(w, http.StatusUnauthorized, "sign in first")
		return
	}
	err := s.store.UnlinkHostAccount(r.Context(), sess.Email, r.PathValue("provider"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not linked")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to unlink")
		return
	}
	s.audit(r, "account.unlink", r.PathValue("provider"), "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}
