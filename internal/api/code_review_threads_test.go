package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// TestThreadAPI: comment on a line and on the conversation, reply, resolve —
// all saved at once and queued for GitHub, shown on the change page.
func TestThreadAPI(t *testing.T) {
	h := newReviewHarness(t)
	id := h.seedChange()
	path := "/api/v1/changes/" + strconv.FormatInt(id, 10)
	open := testCaller{}
	h.expect(h.do(open, http.MethodPost, path+"/threads", map[string]any{"body": " "}), http.StatusBadRequest, "empty")
	h.expect(h.do(open, http.MethodPost, path+"/threads", map[string]any{"path": "app.go", "line": 0, "body": "x"}),
		http.StatusBadRequest, "no line")
	rec := h.do(open, http.MethodPost, path+"/threads", map[string]any{"path": "app.go", "line": 3, "body": "Why?", "author": "a@acme.io"})
	h.expect(rec, http.StatusCreated, "line comment")
	var created struct {
		ThreadID int64 `json:"thread_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	h.expect(h.do(open, http.MethodPost, path+"/threads", map[string]any{"body": "Ready for review", "author": "a@acme.io"}),
		http.StatusCreated, "conversation comment")
	tp := path + "/threads/" + strconv.FormatInt(created.ThreadID, 10)
	h.expect(h.do(open, http.MethodPost, tp+"/comments", map[string]any{"body": "Because.", "author": "b@acme.io"}), http.StatusCreated, "reply")
	h.expect(h.do(open, http.MethodPost, path+"/threads/999999/comments", map[string]any{"body": "x"}), http.StatusNotFound, "no thread")
	h.expect(h.do(open, http.MethodPost, tp+"/resolve", map[string]any{"resolved": true}), http.StatusOK, "resolve")

	var page struct {
		Threads     []store.ChangeThread `json:"threads"`
		Permissions map[string]bool      `json:"permissions"`
	}
	_ = json.Unmarshal(h.do(open, http.MethodGet, path, nil).Body.Bytes(), &page)
	if len(page.Threads) != 2 || !page.Permissions["can_comment"] {
		t.Fatalf("page threads = %+v perms %v", page.Threads, page.Permissions)
	}
	var line store.ChangeThread
	for _, th := range page.Threads {
		if th.Path == "app.go" {
			line = th
		}
	}
	if line.Line != 3 || line.Side != "new" || !line.Resolved || !line.ResolvePending || len(line.Comments) != 2 ||
		line.Comments[0].State != "pending" || line.Comments[1].Author != "b@acme.io" {
		t.Errorf("line thread = %+v", line)
	}
	var queued int
	_ = h.db.QueryRow(context.Background(), `SELECT count(*) FROM provider_outbox WHERE kind IN ('post_comment','resolve_thread')`).Scan(&queued)
	if queued != 4 {
		t.Errorf("queued writes = %d, want 4 (3 comments + 1 resolve)", queued)
	}
}

// TestThreadPermissionsAndLocalHosts: with members, only people with a role
// comment; on a host without threads comments stay in Forge (local).
func TestThreadPermissionsAndLocalHosts(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	id, _, err := h.st.ApplyChange(ctx, "acme/plain", "git", &vcs.Change{ID: "1", Title: "x", Author: "dev",
		SourceBranch: "f", TargetBranch: "main", State: "open", HeadSHA: strings.Repeat("a", 40), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.AddMember(ctx, "acme/plain", "dev@acme.io", "developer"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/changes/" + strconv.FormatInt(id, 10) + "/threads"
	h.expect(h.do(h.login("eve@else.io"), http.MethodPost, path, map[string]any{"body": "hi"}), http.StatusForbidden, "outsider")
	h.expect(h.do(h.login("dev@acme.io"), http.MethodPost, path, map[string]any{"path": "a.go", "line": 1, "body": "hi"}),
		http.StatusCreated, "member")
	ts, _ := h.st.ChangeThreads(ctx, id)
	if len(ts) != 1 || ts[0].Comments[0].State != "local" || ts[0].Comments[0].AuthorEmail != "dev@acme.io" {
		t.Errorf("plain-git thread = %+v", ts)
	}
	var queued int
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM provider_outbox`).Scan(&queued)
	if queued != 0 {
		t.Errorf("a host with no comments got %d writes queued", queued)
	}
}

// TestLinkGitHubAccount drives the OAuth link against a stub GitHub: the
// user's token is stored encrypted, their login mapped to them, and a
// verdict they give in Forge is then queued as their own review.
func TestLinkGitHubAccount(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			if r.Form.Get("code") != "good" || r.Form.Get("client_secret") != "sek" {
				_, _ = w.Write([]byte(`{"error_description":"bad code"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_dev"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer gho_dev" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"login":"Dev-GH"}`))
		}
	}))
	defer gh.Close()
	t.Setenv("GITHUB_OAUTH_BASE", gh.URL)
	h := newEnforcedHarness(t)
	t.Setenv("GITHUB_API_BASE", gh.URL)
	t.Setenv("GITHUB_LINK_CLIENT_ID", "cid")
	t.Setenv("GITHUB_LINK_CLIENT_SECRET", "sek")
	dev := h.login("dev@acme.io")

	h.expect(h.do(testCaller{}, http.MethodGet, "/api/v1/auth/link/github", nil), http.StatusUnauthorized, "anonymous start")
	rec := h.do(dev, http.MethodGet, "/api/v1/auth/link/github", nil)
	h.expect(rec, http.StatusFound, "start")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("scope") != "repo" || !strings.HasSuffix(loc.Query().Get("redirect_uri"), "/api/v1/auth/callback/github/link") {
		t.Fatalf("authorize redirect = %s", loc)
	}
	state := loc.Query().Get("state")
	callback := func(code, st string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/callback/github/link?code="+code+"&state="+st, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: dev.cookie})
		req.AddCookie(&http.Cookie{Name: linkStateCookie, Value: state})
		out := httptest.NewRecorder()
		h.srv.ServeHTTP(out, req)
		return out
	}
	h.expect(callback("good", "forged"), http.StatusBadRequest, "state mismatch")
	h.expect(callback("bad", state), http.StatusBadGateway, "bad code")
	rec = callback("good", state)
	h.expect(rec, http.StatusFound, "callback")

	ctx := context.Background()
	login, token, ok, err := h.st.UserHostToken(ctx, "dev@acme.io", "github")
	if err != nil || !ok || login != "dev-gh" || token != "gho_dev" {
		t.Fatalf("stored link = %q %q %v %v", login, token, ok, err)
	}
	var raw string
	_ = h.db.QueryRow(ctx, `SELECT token FROM user_host_tokens`).Scan(&raw)
	if raw == "gho_dev" {
		t.Error("the user's token is stored in plain text")
	}
	if email, _ := h.st.HostIdentityEmail(ctx, "github", "dev-gh"); email != "dev@acme.io" {
		t.Errorf("login not mapped: %q", email)
	}
	var links struct {
		Links []store.HostLink `json:"links"`
	}
	_ = json.Unmarshal(h.do(dev, http.MethodGet, "/api/v1/account/links", nil).Body.Bytes(), &links)
	if len(links.Links) != 1 || links.Links[0].Login != "dev-gh" || strings.Contains(h.do(dev, http.MethodGet, "/api/v1/account/links", nil).Body.String(), "gho_") {
		t.Errorf("links = %+v", links)
	}

	// A verdict from a linked reviewer is queued as their own review.
	id, _, err := h.st.ApplyChange(ctx, "acme/shop", "github", &vcs.Change{ID: "9", Title: "x", Author: "someone",
		SourceBranch: "f", TargetBranch: "main", State: "open", HeadSHA: "h9", UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(dev, http.MethodPost, "/api/v1/changes/"+strconv.FormatInt(id, 10)+"/reviews", map[string]any{"verdict": "approve"}),
		http.StatusOK, "approve")
	var kind string
	_ = h.db.QueryRow(ctx, `SELECT kind FROM provider_outbox WHERE requested_by = 'dev@acme.io'`).Scan(&kind)
	if kind != review.KindPostReview {
		t.Errorf("queued = %q, want the reviewer's own review", kind)
	}
	h.expect(h.do(dev, http.MethodDelete, "/api/v1/account/links/github", nil), http.StatusNoContent, "unlink")
	if _, _, ok, _ := h.st.UserHostToken(ctx, "dev@acme.io", "github"); ok {
		t.Error("unlink kept the token")
	}
}
