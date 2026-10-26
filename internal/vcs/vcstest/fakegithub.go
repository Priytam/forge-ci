package vcstest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// FakeGitHub is a stateful GitHub for one repo (acme/shop): the GraphQL
// pull-request query, the REST pulls list with ETags, rate-limit headers and
// webhook payloads in GitHub's shape. It counts every request it serves, so a
// test can prove a code path made none.
type FakeGitHub struct {
	Server *httptest.Server

	mu         sync.Mutex
	pulls      map[int]*fakePull
	next       int
	version    int
	seq        int
	rate       [2]int // remaining, limit
	reset      time.Time
	reject     *rejection
	requests   int
	delivery   int
	statuses   map[string]map[string]fakeStatus // sha → context → status
	required   []string                         // branch protection: contexts that must be success
	commentSeq int64
	loseNext   bool              // apply the next write, then answer 502 (a lost response)
	users      map[string]string // token → login; any other token is the App bot
}

// BotLogin is who writes with the repo's (App) token.
const BotLogin = "forge-ci-travafa[bot]"

// AddUser makes token act as login (a user who linked their account).
func (f *FakeGitHub) AddUser(token, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[token] = login
}

// LoseNextResponse makes the next write succeed on the host but answer 502,
// as when a response is lost after the host acted.
func (f *FakeGitHub) LoseNextResponse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseNext = true
}

func (f *FakeGitHub) login(r *http.Request) string {
	if l, ok := f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
		return l
	}
	return BotLogin
}

func (f *FakeGitHub) newComment(r *http.Request, body string) fakeComment {
	f.commentSeq++
	return fakeComment{id: 5000 + f.commentSeq, login: f.login(r), body: body, created: time.Now().UTC().Truncate(time.Second)}
}

// lost answers 502 when the response is to be lost; the write already happened.
func (f *FakeGitHub) lost(w http.ResponseWriter) bool {
	if !f.loseNext {
		return false
	}
	f.loseNext = false
	http.Error(w, `{"message":"Bad Gateway"}`, http.StatusBadGateway)
	return true
}

// HostThread is someone commenting on a line on GitHub; returns the thread id.
func (f *FakeGitHub) HostThread(t *testing.T, id, login, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	f.commentSeq++
	th := &fakeThread{id: fmt.Sprintf("PRRT_%d", f.commentSeq), path: "app.go", side: "RIGHT", line: 1, commit: p.head,
		comments: []fakeComment{{id: 5000 + f.commentSeq, login: login, body: body, created: time.Now().UTC().Truncate(time.Second)}}}
	p.threads = append(p.threads, th)
	f.touch(p)
	return th.id
}

// HostResolve resolves a thread on GitHub.
func (f *FakeGitHub) HostResolve(t *testing.T, id, threadID string, resolved bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, th := range f.pull(t, id).threads {
		if th.id == threadID {
			th.resolved = resolved
		}
	}
}

// CommentsOn lists every comment body on the pull request, threads and
// conversation, as "login: body".
func (f *FakeGitHub) CommentsOn(t *testing.T, id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	var out []string
	for _, th := range p.threads {
		for _, c := range th.comments {
			out = append(out, c.login+": "+c.body)
		}
	}
	for _, c := range p.convo {
		out = append(out, c.login+": "+c.body)
	}
	return out
}

// ThreadState returns a thread's resolved flag and comment count.
func (f *FakeGitHub) ThreadState(t *testing.T, id, threadID string) (resolved bool, comments int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, th := range f.pull(t, id).threads {
		if th.id == threadID {
			return th.resolved, len(th.comments)
		}
	}
	return false, 0
}

// ReviewsBy lists the reviews a login submitted, as GraphQL states.
func (f *FakeGitHub) ReviewsBy(t *testing.T, id, login string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, rv := range f.pull(t, id).reviews {
		if rv.login == login {
			out = append(out, rv.state)
		}
	}
	return out
}

type fakeStatus struct {
	State, Description, TargetURL string
}

type rejection struct {
	status int
	retry  time.Duration
}

type fakePull struct {
	number    int
	title     string
	state     string // open | closed | merged
	head      string
	headRef   string
	base      string
	created   time.Time
	updated   time.Time
	reviews   []fakeReview
	reviewSeq int64
	threads   []*fakeThread
	convo     []fakeComment // the conversation (issue comments)
}

type fakeThread struct {
	id       string // GraphQL node id
	path     string
	side     string // RIGHT | LEFT
	line     int
	commit   string
	resolved bool
	comments []fakeComment
}

type fakeComment struct {
	id      int64
	login   string
	body    string
	created time.Time
}

type fakeReview struct {
	id        int64
	login     string
	state     string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	commit    string
	submitted time.Time
}

// FakeRepo is the repo FakeGitHub serves.
const FakeRepo = "acme/shop"

// NewFakeGitHub starts the fake and points GITHUB_API_BASE at it for the test.
func NewFakeGitHub(t *testing.T) *FakeGitHub {
	t.Helper()
	f := &FakeGitHub{pulls: map[int]*fakePull{}, next: 1, rate: [2]int{4999, 5000}, statuses: map[string]map[string]fakeStatus{}, users: map[string]string{},
		reset: time.Now().Add(time.Hour).Truncate(time.Second)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	t.Setenv("GITHUB_API_BASE", f.Server.URL)
	return f
}

// Requests is how many API requests the fake has served.
func (f *FakeGitHub) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *FakeGitHub) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "github", Repo: FakeRepo, CloneURL: "https://github.com/acme/shop.git", Token: "ghs_fake"}
}

func (f *FakeGitHub) sha() string {
	f.seq++
	return fmt.Sprintf("%040x", f.seq)
}

func (f *FakeGitHub) touch(p *fakePull) {
	f.version++
	p.updated = time.Now().UTC().Truncate(time.Second).Add(time.Duration(f.version) * time.Second)
}

func (f *FakeGitHub) pull(t *testing.T, id string) *fakePull {
	t.Helper()
	n, _ := strconv.Atoi(id)
	p, ok := f.pulls[n]
	if !ok {
		t.Fatalf("fake github: no pull request %s", id)
	}
	return p
}

func (f *FakeGitHub) Open(t *testing.T, title string) (string, *vcs.Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.next
	f.next++
	p := &fakePull{number: n, title: title, state: "open", head: f.sha(), base: f.sha(),
		headRef: "feature/" + strconv.Itoa(n), created: time.Now().UTC().Truncate(time.Second)}
	f.touch(p)
	f.pulls[n] = p
	return strconv.Itoa(n), nil
}

func (f *FakeGitHub) Push(t *testing.T, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	p.head = f.sha()
	f.touch(p)
	return p.head
}

func (f *FakeGitHub) Close(t *testing.T, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	p.state = "closed"
	f.touch(p)
}

func (f *FakeGitHub) Review(t *testing.T, id, reviewer, verdict string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	state := map[string]string{"approve": "APPROVED", "request_changes": "CHANGES_REQUESTED",
		"comment": "COMMENTED"}[verdict]
	p.reviewSeq++
	p.reviews = append(p.reviews, fakeReview{id: int64(p.number)*1000 + p.reviewSeq, login: reviewer,
		state: state, commit: p.head, submitted: time.Now().UTC().Truncate(time.Second)})
	f.touch(p)
}

// Protect requires these status contexts to be success before any merge —
// GitHub branch protection's "required status checks".
func (f *FakeGitHub) Protect(contexts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.required = contexts
}

// Check returns the state of a commit status ("" when none was posted).
func (f *FakeGitHub) Check(_ *testing.T, sha, context string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[sha][context].State
}

// StatusOf returns the full commit status (state, description, link).
func (f *FakeGitHub) StatusOf(sha, context string) (state, description, targetURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.statuses[sha][context]
	return s.State, s.Description, s.TargetURL
}

// Head returns a pull request's current head sha.
func (f *FakeGitHub) Head(t *testing.T, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pull(t, id).head
}

// State returns a pull request's state (open | closed | merged).
func (f *FakeGitHub) State(t *testing.T, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pull(t, id).state
}

// HostMerge is someone clicking Merge on GitHub itself: refused while a
// required check is not success, exactly like branch protection.
func (f *FakeGitHub) HostMerge(t *testing.T, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	if msg := f.blocked(p); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	p.state = "merged"
	f.touch(p)
	return nil
}

// blocked says why p cannot merge now ("" when it can).
func (f *FakeGitHub) blocked(p *fakePull) string {
	if p.state != "open" {
		return "Pull Request is not mergeable"
	}
	for _, ctx := range f.required {
		if f.statuses[p.head][ctx].State != "success" {
			return `Required status check "` + ctx + `" is expected.`
		}
	}
	return ""
}

func (f *FakeGitHub) RateLimit(remaining, limit int, reset time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rate, f.reset = [2]int{remaining, limit}, reset
}

func (f *FakeGitHub) RejectNext(status int, retry time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reject = &rejection{status: status, retry: retry}
}

// restPull is the REST/webhook JSON for a pull request.
func (f *FakeGitHub) restPull(p *fakePull) map[string]any {
	state := p.state
	var mergedAt any
	if state == "merged" {
		state, mergedAt = "closed", p.updated
	}
	return map[string]any{
		"number": p.number, "title": p.title, "body": "", "state": state, "draft": false,
		"merged": p.state == "merged", "merged_at": mergedAt,
		"html_url":   "https://github.com/" + FakeRepo + "/pull/" + strconv.Itoa(p.number),
		"user":       map[string]any{"login": "author1"},
		"head":       map[string]any{"ref": p.headRef, "sha": p.head},
		"base":       map[string]any{"ref": "main", "sha": p.base},
		"labels":     []any{},
		"created_at": p.created, "updated_at": p.updated,
	}
}

func (f *FakeGitHub) Delivery(t *testing.T, id, kind string) (http.Header, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	f.delivery++
	hdr := http.Header{}
	hdr.Set("X-GitHub-Delivery", "delivery-"+strconv.Itoa(f.delivery))
	payload := map[string]any{
		"number": p.number, "pull_request": f.restPull(p),
		"repository": map[string]any{"full_name": FakeRepo},
	}
	switch kind {
	case "change":
		hdr.Set("X-GitHub-Event", "pull_request")
		payload["action"] = "synchronize"
	case "comment":
		hdr.Set("X-GitHub-Event", "pull_request_review_comment")
		payload["action"] = "created"
		payload["comment"] = map[string]any{"id": 1, "body": "x", "user": map[string]any{"login": "reviewer1"}}
	case "review":
		hdr.Set("X-GitHub-Event", "pull_request_review")
		payload["action"] = "submitted"
		payload["review"] = map[string]any{"id": 1, "state": "approved", "user": map[string]any{"login": "reviewer1"}}
	default:
		return nil, nil, false
	}
	body, _ := json.Marshal(payload)
	return hdr, body, true
}

func (f *FakeGitHub) Unrelated(t *testing.T) (http.Header, []byte, bool) {
	hdr := http.Header{}
	hdr.Set("X-GitHub-Event", "push")
	hdr.Set("X-GitHub-Delivery", "delivery-push")
	return hdr, []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"` + FakeRepo + `"}}`), true
}

func (f *FakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(f.rate[0]))
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(f.rate[1]))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.reset.Unix(), 10))
	if rj := f.reject; rj != nil {
		f.reject = nil
		w.Header().Set("Retry-After", strconv.Itoa(int(rj.retry.Seconds())))
		http.Error(w, `{"message":"rate limited"}`, rj.status)
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, `{"message":"Requires authentication"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		f.graphql(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/"+FakeRepo+"/pulls":
		f.list(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/"+FakeRepo+"/statuses/"):
		f.postStatus(w, r, strings.TrimPrefix(r.URL.Path, "/repos/"+FakeRepo+"/statuses/"))
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/repos/"+FakeRepo+"/pulls/") &&
		strings.HasSuffix(r.URL.Path, "/merge"):
		f.merge(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"+FakeRepo+"/pulls/"), "/merge"))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/"+FakeRepo+"/pulls/"):
		f.pullWrite(w, r, strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"+FakeRepo+"/pulls/"), "/"))
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/"+FakeRepo+"/issues/") &&
		strings.HasSuffix(r.URL.Path, "/comments"):
		f.issueComment(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"+FakeRepo+"/issues/"), "/comments"))
	default:
		http.NotFound(w, r)
	}
}

func (f *FakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string `json:"query"`
		Variables struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			Number int    `json:"number"`
			ID     string `json:"id"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Query, "ReviewThread(") {
		f.resolveMutation(w, req.Variables.ID, strings.Contains(req.Query, "mutation($id:ID!){resolveReviewThread"))
		return
	}
	if !strings.Contains(req.Query, "pullRequest") {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	p, ok := f.pulls[req.Variables.Number]
	if !ok || req.Variables.Owner+"/"+req.Variables.Name != FakeRepo {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":   map[string]any{"repository": map[string]any{"pullRequest": nil}},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest"}},
		})
		return
	}
	reviews := []any{}
	for _, rv := range p.reviews {
		reviews = append(reviews, map[string]any{
			"databaseId": rv.id, "state": rv.state, "body": "", "submittedAt": rv.submitted,
			"author": map[string]any{"login": rv.login}, "commit": map[string]any{"oid": rv.commit},
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
		"pullRequest": map[string]any{
			"number": p.number, "title": p.title, "body": "", "state": strings.ToUpper(p.state),
			"url":     "https://github.com/" + FakeRepo + "/pull/" + strconv.Itoa(p.number),
			"isDraft": false, "createdAt": p.created, "updatedAt": p.updated,
			"headRefName": p.headRef, "headRefOid": p.head, "baseRefName": "main", "baseRefOid": p.base,
			"author":        map[string]any{"login": "author1"},
			"labels":        map[string]any{"nodes": []any{}},
			"reviews":       map[string]any{"nodes": reviews},
			"reviewThreads": map[string]any{"nodes": f.threadNodes(p)},
			"comments":      map[string]any{"nodes": commentNodes(p.convo, "")},
		},
	}}})
}

func (f *FakeGitHub) list(w http.ResponseWriter, r *http.Request) {
	etag := `W/"v` + strconv.Itoa(f.version) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	open := []*fakePull{}
	for _, p := range f.pulls {
		if p.state == "open" {
			open = append(open, p)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].updated.After(open[j].updated) })
	out := []any{}
	for _, p := range open {
		out = append(out, f.restPull(p))
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *FakeGitHub) postStatus(w http.ResponseWriter, r *http.Request, sha string) {
	var in struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Context == "" ||
		(in.State != "success" && in.State != "pending" && in.State != "failure" && in.State != "error") {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	if f.statuses[sha] == nil {
		f.statuses[sha] = map[string]fakeStatus{}
	}
	f.statuses[sha][in.Context] = fakeStatus{State: in.State, Description: in.Description, TargetURL: in.TargetURL}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"state":"` + in.State + `"}`))
}

func (f *FakeGitHub) merge(w http.ResponseWriter, r *http.Request, id string) {
	n, _ := strconv.Atoi(id)
	p, ok := f.pulls[n]
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	w.Header().Set("Content-Type", "application/json")
	if in.SHA != "" && in.SHA != p.head {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
		return
	}
	if msg := f.blocked(p); msg != "" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		return
	}
	p.state = "merged"
	f.touch(p)
	_ = json.NewEncoder(w).Encode(map[string]any{"sha": f.sha(), "merged": true, "message": "Pull Request successfully merged"})
}

func commentNodes(cs []fakeComment, commit string) []any {
	out := []any{}
	for _, c := range cs {
		n := map[string]any{"databaseId": c.id, "body": c.body, "createdAt": c.created, "updatedAt": c.created,
			"author": map[string]any{"login": c.login}}
		if commit != "" {
			n["commit"], n["originalCommit"] = map[string]any{"oid": commit}, map[string]any{"oid": commit}
		}
		out = append(out, n)
	}
	return out
}

func (f *FakeGitHub) threadNodes(p *fakePull) []any {
	out := []any{}
	for _, th := range p.threads {
		outdated := th.commit != p.head
		var line any = th.line
		if outdated {
			line = nil
		}
		out = append(out, map[string]any{"id": th.id, "isResolved": th.resolved, "isOutdated": outdated,
			"path": th.path, "line": line, "startLine": nil, "originalLine": th.line, "diffSide": th.side,
			"comments": map[string]any{"nodes": commentNodes(th.comments, th.commit)}})
	}
	return out
}

func (f *FakeGitHub) resolveMutation(w http.ResponseWriter, threadID string, resolved bool) {
	w.Header().Set("Content-Type", "application/json")
	for _, p := range f.pulls {
		for _, th := range p.threads {
			if th.id == threadID {
				th.resolved = resolved
				f.touch(p)
				if f.lost(w) {
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"x": map[string]any{
					"thread": map[string]any{"id": th.id, "isResolved": resolved}}}})
				return
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"type": "NOT_FOUND", "message": "no thread"}}})
}

// pullWrite handles POST pulls/{n}/comments, pulls/{n}/comments/{id}/replies
// and pulls/{n}/reviews.
func (f *FakeGitHub) pullWrite(w http.ResponseWriter, r *http.Request, parts []string) {
	n, _ := strconv.Atoi(parts[0])
	p, ok := f.pulls[n]
	if !ok || len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Side     string `json:"side"`
		Event    string `json:"event"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case len(parts) == 2 && parts[1] == "reviews":
		state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[in.Event]
		p.reviewSeq++
		rv := fakeReview{id: int64(p.number)*1000 + p.reviewSeq, login: f.login(r), state: state,
			commit: in.CommitID, submitted: time.Now().UTC().Truncate(time.Second)}
		p.reviews = append(p.reviews, rv)
		f.touch(p)
		if f.lost(w) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": rv.id})
	case len(parts) == 2 && parts[1] == "comments":
		if in.Path == "" || in.Line <= 0 || in.CommitID == "" {
			http.Error(w, `{"message":"Validation Failed: path, line and commit_id are required"}`, http.StatusUnprocessableEntity)
			return
		}
		c := f.newComment(r, in.Body)
		p.threads = append(p.threads, &fakeThread{id: fmt.Sprintf("PRRT_%d", c.id), path: in.Path, side: in.Side,
			line: in.Line, commit: in.CommitID, comments: []fakeComment{c}})
		f.touch(p)
		if f.lost(w) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": c.id})
	case len(parts) == 4 && parts[1] == "comments" && parts[3] == "replies":
		to, _ := strconv.ParseInt(parts[2], 10, 64)
		for _, th := range p.threads {
			for _, existing := range th.comments {
				if existing.id == to {
					c := f.newComment(r, in.Body)
					th.comments = append(th.comments, c)
					f.touch(p)
					if f.lost(w) {
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": c.id})
					return
				}
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *FakeGitHub) issueComment(w http.ResponseWriter, r *http.Request, id string) {
	n, _ := strconv.Atoi(id)
	p, ok := f.pulls[n]
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	c := f.newComment(r, in.Body)
	p.convo = append(p.convo, c)
	f.touch(p)
	if f.lost(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": c.id})
}

// HostReply is someone replying in a thread on GitHub.
func (f *FakeGitHub) HostReply(t *testing.T, id, threadID, login, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	for _, th := range p.threads {
		if th.id == threadID {
			f.commentSeq++
			th.comments = append(th.comments, fakeComment{id: 5000 + f.commentSeq, login: login, body: body,
				created: time.Now().UTC().Truncate(time.Second).Add(time.Duration(f.commentSeq) * time.Second)})
			f.touch(p)
			return
		}
	}
	t.Fatalf("fake github: no thread %s", threadID)
}

// HostConversation is someone commenting on the pull request's conversation.
func (f *FakeGitHub) HostConversation(t *testing.T, id, login, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pull(t, id)
	f.commentSeq++
	p.convo = append(p.convo, fakeComment{id: 5000 + f.commentSeq, login: login, body: body, created: time.Now().UTC().Truncate(time.Second)})
	f.touch(p)
}

// RateHeaders: GitHub sends X-RateLimit-* on every response.
func (f *FakeGitHub) RateHeaders() bool { return true }
