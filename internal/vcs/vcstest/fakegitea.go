package vcstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// FakeGitea is Gitea's API v1 for one repo (acme/shop) over the shared core.
// Line comments are one-comment reviews, as Gitea stores them.
type FakeGitea struct {
	*core
	Server *httptest.Server
}

// NewFakeGitea starts the fake and points GITEA_API_BASE at it.
func NewFakeGitea(t *testing.T) *FakeGitea {
	t.Helper()
	f := &FakeGitea{core: newCore("forge-bot", false)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	t.Setenv("GITEA_API_BASE", f.Server.URL)
	return f
}

func (f *FakeGitea) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "gitea", Repo: FakeRepo, CloneURL: "https://gitea.example/acme/shop.git", Token: "gitea-fake"}
}

func (f *FakeGitea) pr(p *cPull) map[string]any {
	state := p.state
	if state == "merged" {
		state = "closed"
	}
	return map[string]any{"number": p.number, "title": p.title, "body": p.body, "state": state, "merged": p.state == "merged",
		"html_url": "https://gitea.example/acme/shop/pulls/" + strconv.Itoa(p.number), "user": map[string]any{"login": p.author},
		"head": map[string]any{"ref": p.headRef, "sha": p.head}, "base": map[string]any{"ref": "main", "sha": p.base},
		"labels": []any{}, "created_at": p.created, "updated_at": p.updated}
}

func (f *FakeGitea) Delivery(t *testing.T, id, kind string) (http.Header, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	f.delivery++
	hdr := http.Header{}
	hdr.Set("X-Gitea-Delivery", "gt-"+strconv.Itoa(f.delivery))
	payload := map[string]any{"repository": map[string]any{"full_name": FakeRepo}}
	switch kind {
	case "change":
		hdr.Set("X-Gitea-Event", "pull_request")
		payload["action"], payload["pull_request"] = "synchronized", f.pr(p)
	case "review":
		hdr.Set("X-Gitea-Event", "pull_request_review_approved")
		payload["action"], payload["pull_request"] = "reviewed", f.pr(p)
	case "comment":
		hdr.Set("X-Gitea-Event", "issue_comment")
		payload["action"] = "created"
		payload["issue"] = map[string]any{"number": p.number, "pull_request": map[string]any{"merged": false}}
	default:
		return nil, nil, false
	}
	body, _ := json.Marshal(payload)
	return hdr, body, true
}

func (f *FakeGitea) Unrelated(*testing.T) (http.Header, []byte, bool) {
	hdr := http.Header{}
	hdr.Set("X-Gitea-Event", "push")
	return hdr, []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/shop"}}`), true
}

func gtCommentJSON(c cComment, th *cThread) map[string]any {
	m := map[string]any{"id": c.id, "body": c.body, "created_at": c.at, "updated_at": c.at, "user": map[string]any{"login": c.login}}
	if th != nil {
		m["path"], m["commit_id"] = th.path, th.commit
		if th.side == "old" {
			m["original_position"] = th.line
		} else {
			m["position"] = th.line
		}
	}
	return m
}

func (f *FakeGitea) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate(w, r.Method != http.MethodGet) {
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok {
		http.Error(w, `{"message":"token is required"}`, http.StatusUnauthorized)
		return
	}
	login := f.whoami(token)
	prefix := "/repos/" + FakeRepo + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) {
		if r.Method != http.MethodGet && f.lost(w) {
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	if parts[0] == "statuses" && len(parts) == 2 && r.Method == http.MethodPost {
		f.setStatus(parts[1], in["context"].(string), in["state"].(string))
		reply(map[string]any{"id": f.id()})
		return
	}
	if len(parts) == 1 && parts[0] == "pulls" {
		out := []any{}
		for _, p := range f.open() {
			out = append(out, f.pr(p))
		}
		reply(out)
		return
	}
	if len(parts) < 2 || (parts[0] != "pulls" && parts[0] != "issues") {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.Atoi(parts[1])
	p, ok := f.pulls[n]
	if !ok {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	rest := strings.Join(parts[2:], "/")
	switch {
	case parts[0] == "pulls" && rest == "" && r.Method == http.MethodGet:
		reply(f.pr(p))
	case parts[0] == "issues" && rest == "comments" && r.Method == http.MethodGet:
		out := []any{}
		for _, c := range p.convo {
			out = append(out, gtCommentJSON(c, nil))
		}
		reply(out)
	case parts[0] == "issues" && rest == "comments" && r.Method == http.MethodPost:
		c := f.addConvo(p, login, in["body"].(string))
		reply(map[string]any{"id": c.id})
	case parts[0] == "pulls" && rest == "reviews" && r.Method == http.MethodGet:
		out := []any{}
		for _, rv := range p.reviews {
			state := map[string]string{"approve": "APPROVED", "request_changes": "REQUEST_CHANGES", "comment": "COMMENT"}[rv.verdict]
			out = append(out, map[string]any{"id": rv.id, "state": state, "body": "", "commit_id": rv.commit,
				"submitted_at": rv.at, "comments_count": 0, "user": map[string]any{"login": rv.login}})
		}
		// Every line comment is its own one-comment review.
		for _, th := range p.threads {
			for _, c := range th.comments {
				out = append(out, map[string]any{"id": c.id, "state": "COMMENT", "body": "", "commit_id": th.commit,
					"submitted_at": c.at, "comments_count": 1, "user": map[string]any{"login": c.login}})
			}
		}
		reply(out)
	case parts[0] == "pulls" && len(parts) == 5 && parts[2] == "reviews" && parts[4] == "comments":
		for _, th := range p.threads {
			for _, c := range th.comments {
				if strconv.FormatInt(c.id, 10) == parts[3] {
					reply([]any{gtCommentJSON(c, th)})
					return
				}
			}
		}
		reply([]any{})
	case parts[0] == "pulls" && rest == "reviews" && r.Method == http.MethodPost:
		event, _ := in["event"].(string)
		commit, _ := in["commit_id"].(string)
		comments, _ := in["comments"].([]any)
		if len(comments) == 0 {
			verdict := map[string]string{"APPROVED": "approve", "REQUEST_CHANGES": "request_changes", "COMMENT": "comment"}[event]
			rv := f.addReview(p, login, verdict, commit)
			if verdict == "approve" {
				p.approved[login] = true
			}
			reply(map[string]any{"id": rv.id})
			return
		}
		cm := comments[0].(map[string]any)
		side, line := "new", cm["new_position"]
		if line == nil {
			side, line = "old", cm["old_position"]
		}
		path, ln := cm["path"].(string), int(line.(float64))
		for _, th := range p.threads {
			if th.path == path && th.side == side && th.line == ln {
				c := f.reply(p, th, login, cm["body"].(string))
				reply(map[string]any{"id": c.id})
				return
			}
		}
		_, c := f.addThread(p, login, path, side, ln, commit, cm["body"].(string))
		reply(map[string]any{"id": c.id})
	case parts[0] == "pulls" && rest == "merge" && r.Method == http.MethodPost:
		if head, _ := in["head_commit_id"].(string); head != "" && head != p.head {
			http.Error(w, `{"message":"head out of date"}`, http.StatusConflict)
			return
		}
		if msg := f.blocked(p); msg != "" {
			http.Error(w, `{"message":"Please try again later: `+msg+`"}`, http.StatusMethodNotAllowed)
			return
		}
		p.state = "merged"
		f.touch(p)
		if f.lost(w) {
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

// HostThread: Gitea has no thread ids; a thread is known by its line.
func (f *FakeGitea) HostThread(t *testing.T, id, login, body string) string {
	f.core.HostThread(t, id, login, body)
	return "app.go:new:1"
}
