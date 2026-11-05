package vcstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// FakeGitLab is GitLab's API v4 for one project (acme/shop) over the shared
// core: merge requests, approvals, discussions and notes, commit statuses,
// and merge with an expected sha.
type FakeGitLab struct {
	*core
	Server *httptest.Server
}

// NewFakeGitLab starts the fake and points GITLAB_API_BASE at it.
func NewFakeGitLab(t *testing.T) *FakeGitLab {
	t.Helper()
	f := &FakeGitLab{core: newCore("forge-bot", true)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	t.Setenv("GITLAB_API_BASE", f.Server.URL)
	return f
}

func (f *FakeGitLab) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "gitlab", Repo: FakeRepo, CloneURL: "https://gitlab.example/acme/shop.git", Token: "glpat-fake"}
}

func (f *FakeGitLab) Delivery(t *testing.T, id, kind string) (http.Header, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pull(t, id)
	f.delivery++
	hdr := http.Header{}
	hdr.Set("X-Gitlab-Event-UUID", "gl-"+strconv.Itoa(f.delivery))
	n, _ := strconv.Atoi(id)
	payload := map[string]any{"project": map[string]any{"path_with_namespace": FakeRepo}}
	switch kind {
	case "change", "review":
		action := "update"
		if kind == "review" {
			action = "approved"
		}
		hdr.Set("X-Gitlab-Event", "Merge Request Hook")
		payload["object_kind"] = "merge_request"
		payload["object_attributes"] = map[string]any{"iid": n, "action": action}
	case "comment":
		hdr.Set("X-Gitlab-Event", "Note Hook")
		payload["object_kind"] = "note"
		payload["object_attributes"] = map[string]any{"noteable_type": "MergeRequest", "note": "x"}
		payload["merge_request"] = map[string]any{"iid": n}
	default:
		return nil, nil, false
	}
	body, _ := json.Marshal(payload)
	return hdr, body, true
}

func (f *FakeGitLab) Unrelated(*testing.T) (http.Header, []byte, bool) {
	hdr := http.Header{}
	hdr.Set("X-Gitlab-Event", "Push Hook")
	return hdr, []byte(`{"object_kind":"push","project":{"path_with_namespace":"acme/shop"}}`), true
}

func (f *FakeGitLab) mr(p *cPull) map[string]any {
	state := map[string]string{"open": "opened", "merged": "merged", "closed": "closed"}[p.state]
	return map[string]any{"iid": p.number, "title": p.title, "description": p.body, "state": state, "draft": false,
		"source_branch": p.headRef, "target_branch": "main", "sha": p.head, "web_url": "https://gitlab.example/acme/shop/-/merge_requests/" + strconv.Itoa(p.number),
		"labels": []string{}, "created_at": p.created, "updated_at": p.updated, "author": map[string]any{"username": p.author},
		"diff_refs": map[string]any{"base_sha": p.base, "head_sha": p.head, "start_sha": p.base}}
}

func glNoteJSON(c cComment, pos map[string]any, resolved bool) map[string]any {
	n := map[string]any{"id": c.id, "body": c.body, "system": false, "resolvable": pos != nil, "resolved": resolved,
		"created_at": c.at, "updated_at": c.at, "author": map[string]any{"username": c.login}}
	if pos != nil {
		n["position"] = pos
	}
	return n
}

func (f *FakeGitLab) discussions(p *cPull) []any {
	out := []any{}
	for _, th := range p.threads {
		pos := map[string]any{"new_path": th.path, "old_path": th.path, "head_sha": th.commit, "position_type": "text"}
		if th.side == "old" {
			pos["old_line"] = th.line
		} else {
			pos["new_line"] = th.line
		}
		var notes []any
		for _, c := range th.comments {
			notes = append(notes, glNoteJSON(c, pos, th.resolved))
		}
		out = append(out, map[string]any{"id": th.id, "individual_note": false, "notes": notes})
	}
	for _, c := range p.convo {
		out = append(out, map[string]any{"id": "conv" + strconv.FormatInt(c.id, 10), "individual_note": true,
			"notes": []any{glNoteJSON(c, nil, false)}})
	}
	return out
}

func (f *FakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := r.Method != http.MethodGet
	if f.gate(w, write) {
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	prefix := "/projects/acme%2Fshop/"
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	login := f.whoami(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) {
		if write && f.lost(w) {
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	if parts[0] == "statuses" && len(parts) == 2 && r.Method == http.MethodPost {
		state := map[string]string{"success": "success", "failed": "failure"}[in["state"].(string)]
		if state == "" {
			state = "pending"
		}
		f.setStatus(parts[1], in["name"].(string), state)
		reply(map[string]any{"status": in["state"]})
		return
	}
	if parts[0] != "merge_requests" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		out := []any{}
		for _, p := range f.open() {
			out = append(out, f.mr(p))
		}
		reply(out)
		return
	}
	n, _ := strconv.Atoi(parts[1])
	p, ok := f.pulls[n]
	if !ok {
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		return
	}
	rest := strings.Join(parts[2:], "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		reply(f.mr(p))
	case rest == "approvals":
		var by []any
		names := make([]string, 0, len(p.approved))
		for l := range p.approved {
			names = append(names, l)
		}
		sort.Strings(names)
		for _, l := range names {
			by = append(by, map[string]any{"user": map[string]any{"username": l}})
		}
		reply(map[string]any{"approved_by": by})
	case rest == "discussions" && r.Method == http.MethodGet:
		reply(f.discussions(p))
	case rest == "discussions" && r.Method == http.MethodPost:
		pos, _ := in["position"].(map[string]any)
		if pos == nil || pos["base_sha"] == "" || pos["start_sha"] == "" {
			http.Error(w, `{"message":"400 position is invalid"}`, http.StatusBadRequest)
			return
		}
		side, line := "new", pos["new_line"]
		if line == nil {
			side, line = "old", pos["old_line"]
		}
		th, c := f.addThread(p, login, pos["new_path"].(string), side, int(line.(float64)), pos["head_sha"].(string), in["body"].(string))
		reply(map[string]any{"id": th.id, "notes": []any{glNoteJSON(c, nil, false)}})
	case len(parts) == 5 && parts[2] == "discussions" && parts[4] == "notes" && r.Method == http.MethodPost:
		th := f.threadByID(p, parts[3])
		if th == nil {
			http.Error(w, `{"message":"404 Discussion Not Found"}`, http.StatusNotFound)
			return
		}
		c := f.reply(p, th, login, in["body"].(string))
		reply(map[string]any{"id": c.id})
	case len(parts) == 4 && parts[2] == "discussions" && r.Method == http.MethodPut:
		th := f.threadByID(p, parts[3])
		if th == nil {
			http.Error(w, `{"message":"404 Discussion Not Found"}`, http.StatusNotFound)
			return
		}
		th.resolved = r.URL.Query().Get("resolved") == "true"
		f.touch(p)
		reply(map[string]any{"id": th.id})
	case rest == "notes" && r.Method == http.MethodPost:
		c := f.addConvo(p, login, in["body"].(string))
		reply(map[string]any{"id": c.id})
	case rest == "approve" && r.Method == http.MethodPost:
		if sha, _ := in["sha"].(string); sha != "" && sha != p.head {
			http.Error(w, `{"message":"SHA does not match HEAD of source branch"}`, http.StatusConflict)
			return
		}
		p.approved[login] = true
		f.addReview(p, login, "approve", p.head)
		reply(map[string]any{"approved": true})
	case rest == "merge" && r.Method == http.MethodPut:
		if sha, _ := in["sha"].(string); sha != "" && sha != p.head {
			http.Error(w, `{"message":"SHA does not match HEAD of source branch"}`, http.StatusConflict)
			return
		}
		if msg := f.blocked(p); msg != "" {
			http.Error(w, `{"message":"405 Method Not Allowed: `+msg+`"}`, http.StatusMethodNotAllowed)
			return
		}
		p.state = "merged"
		f.touch(p)
		reply(map[string]any{"state": "merged", "merge_commit_sha": f.sha()})
	default:
		http.NotFound(w, r)
	}
}
