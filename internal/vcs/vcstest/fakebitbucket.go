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

// FakeBitbucket is Bitbucket Cloud's API 2.0 for one repo (acme/shop) over
// the shared core — including its 12-character commit hashes in pull
// requests and a merge call that takes no expected head.
type FakeBitbucket struct {
	*core
	Server *httptest.Server
}

// NewFakeBitbucket starts the fake and points BITBUCKET_API_BASE at it.
func NewFakeBitbucket(t *testing.T) *FakeBitbucket {
	t.Helper()
	f := &FakeBitbucket{core: newCore("forge-bot", false)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	t.Setenv("BITBUCKET_API_BASE", f.Server.URL)
	return f
}

func (f *FakeBitbucket) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "bitbucket", Repo: FakeRepo, CloneURL: "https://bitbucket.org/acme/shop.git", Token: "bb-fake"}
}

func short(sha string) string { return sha[:12] }

func (f *FakeBitbucket) pr(p *cPull) map[string]any {
	state := map[string]string{"open": "OPEN", "merged": "MERGED", "closed": "DECLINED"}[p.state]
	var parts []any
	for login := range p.approved {
		parts = append(parts, map[string]any{"user": map[string]any{"nickname": login}, "approved": true, "state": "approved",
			"participated_on": p.updated})
	}
	for _, rv := range p.reviews {
		if rv.verdict == "request_changes" {
			parts = append(parts, map[string]any{"user": map[string]any{"nickname": rv.login}, "approved": false,
				"state": "changes_requested", "participated_on": rv.at})
		}
	}
	return map[string]any{"id": p.number, "title": p.title, "description": p.body, "state": state,
		"source":      map[string]any{"branch": map[string]any{"name": p.headRef}, "commit": map[string]any{"hash": short(p.head)}},
		"destination": map[string]any{"branch": map[string]any{"name": "main"}, "commit": map[string]any{"hash": short(p.base)}},
		"author":      map[string]any{"nickname": p.author}, "participants": parts,
		"links":      map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/shop/pull-requests/" + strconv.Itoa(p.number)}},
		"created_on": p.created, "updated_on": p.updated}
}

func (f *FakeBitbucket) Delivery(t *testing.T, id, kind string) (http.Header, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	f.delivery++
	hdr := http.Header{}
	hdr.Set("X-Request-UUID", "bb-"+strconv.Itoa(f.delivery))
	key := map[string]string{"change": "pullrequest:updated", "review": "pullrequest:approved", "comment": "pullrequest:comment_created"}[kind]
	if key == "" {
		return nil, nil, false
	}
	hdr.Set("X-Event-Key", key)
	body, _ := json.Marshal(map[string]any{"pullrequest": f.pr(p), "repository": map[string]any{"full_name": FakeRepo}})
	return hdr, body, true
}

func (f *FakeBitbucket) Unrelated(*testing.T) (http.Header, []byte, bool) {
	hdr := http.Header{}
	hdr.Set("X-Event-Key", "repo:push")
	return hdr, []byte(`{"repository":{"full_name":"acme/shop"},"push":{"changes":[]}}`), true
}

func (f *FakeBitbucket) comments(p *cPull) []any {
	out := []any{}
	add := func(c cComment, inline map[string]any, parent int64, resolved bool) {
		m := map[string]any{"id": c.id, "content": map[string]any{"raw": c.body}, "user": map[string]any{"nickname": c.login},
			"created_on": c.at, "updated_on": c.at, "deleted": false}
		if inline != nil {
			m["inline"] = inline
		}
		if parent != 0 {
			m["parent"] = map[string]any{"id": parent}
		}
		if resolved {
			m["resolution"] = map[string]any{"type": "comment_resolution"}
		}
		out = append(out, m)
	}
	for _, th := range p.threads {
		inline := map[string]any{"path": th.path}
		if th.side == "old" {
			inline["from"] = th.line
		} else {
			inline["to"] = th.line
		}
		for i, c := range th.comments {
			if i == 0 {
				add(c, inline, 0, th.resolved)
			} else {
				add(c, inline, th.comments[0].id, false)
			}
		}
	}
	for _, c := range p.convo {
		add(c, nil, 0, false)
	}
	return out
}

func (f *FakeBitbucket) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate(w, r.Method != http.MethodGet) {
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, `{"type":"error","error":{"message":"Unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	login := f.whoami(token)
	prefix := "/2.0/repositories/" + FakeRepo + "/"
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
		if v == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case parts[0] == "commit" && len(parts) == 2:
		for _, p := range f.pulls {
			for _, sha := range []string{p.head, p.base} {
				if strings.HasPrefix(sha, parts[1]) {
					reply(map[string]any{"hash": sha})
					return
				}
			}
		}
		http.Error(w, `{"error":{"message":"no such commit"}}`, http.StatusNotFound)
		return
	case parts[0] == "commit" && len(parts) == 4 && parts[2] == "statuses" && r.Method == http.MethodPost:
		state := map[string]string{"SUCCESSFUL": "success", "FAILED": "failure"}[in["state"].(string)]
		if state == "" {
			state = "pending"
		}
		f.setStatus(parts[1], in["key"].(string), state)
		reply(map[string]any{"state": in["state"]})
		return
	case parts[0] != "pullrequests":
		http.NotFound(w, r)
		return
	case len(parts) == 1:
		out := []any{}
		for _, p := range f.open() {
			out = append(out, f.pr(p))
		}
		reply(map[string]any{"values": out})
		return
	}
	n, _ := strconv.Atoi(parts[1])
	p, ok := f.pulls[n]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	rest := strings.Join(parts[2:], "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		reply(f.pr(p))
	case rest == "comments" && r.Method == http.MethodGet:
		reply(map[string]any{"values": f.comments(p)})
	case rest == "comments" && r.Method == http.MethodPost:
		content, _ := in["content"].(map[string]any)
		body, _ := content["raw"].(string)
		switch {
		case in["parent"] != nil:
			parent := strconv.FormatInt(int64(in["parent"].(map[string]any)["id"].(float64)), 10)
			th := f.threadByID(p, parent)
			if th == nil {
				http.Error(w, `{"error":{"message":"no such parent"}}`, http.StatusNotFound)
				return
			}
			reply(map[string]any{"id": f.reply(p, th, login, body).id})
		case in["inline"] != nil:
			inline := in["inline"].(map[string]any)
			side, line := "new", inline["to"]
			if line == nil {
				side, line = "old", inline["from"]
			}
			_, c := f.addThread(p, login, inline["path"].(string), side, int(line.(float64)), p.head, body)
			reply(map[string]any{"id": c.id})
		default:
			reply(map[string]any{"id": f.addConvo(p, login, body).id})
		}
	case len(parts) == 5 && parts[2] == "comments" && parts[4] == "resolve":
		th := f.threadByID(p, parts[3])
		if th == nil {
			http.Error(w, `{"error":{"message":"no such comment"}}`, http.StatusNotFound)
			return
		}
		th.resolved = r.Method == http.MethodPost
		f.touch(p)
		reply(map[string]any{"type": "comment_resolution"})
	case rest == "approve" && r.Method == http.MethodPost:
		p.approved[login] = true
		f.addReview(p, login, "approve", p.head)
		reply(map[string]any{"approved": true})
	case rest == "request-changes" && r.Method == http.MethodPost:
		f.addReview(p, login, "request_changes", p.head)
		reply(map[string]any{"state": "changes_requested"})
	case rest == "merge" && r.Method == http.MethodPost:
		if msg := f.blocked(p); msg != "" {
			http.Error(w, `{"type":"error","error":{"message":"`+msg+`"}}`, http.StatusBadRequest)
			return
		}
		p.state = "merged"
		f.touch(p)
		reply(map[string]any{"state": "MERGED", "merge_commit": map[string]any{"hash": f.sha()}})
	default:
		http.NotFound(w, r)
	}
}
