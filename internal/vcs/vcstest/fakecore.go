package vcstest

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// core is the state every fake host shares: pull requests with reviews,
// threads and a conversation, commit statuses and branch protection. Each
// fake (GitLab, Gitea, Bitbucket, CodeCommit) only translates its host's API
// onto it, so they all behave alike where the hosts do, and the suite
// compares like with like.
type core struct {
	mu          sync.Mutex
	pulls       map[int]*cPull
	next        int
	seq         int64
	version     int
	statuses    map[string]map[string]string // sha → context → success | pending | failure
	required    []string
	users       map[string]string // token → login
	bot         string
	reject      *rejection
	loseNext    bool
	requests    int
	rate        [2]int
	reset       time.Time
	rateHeaders bool
	delivery    int
}

type cPull struct {
	number   int
	title    string
	body     string
	state    string // open | closed | merged
	head     string
	headRef  string
	base     string
	author   string
	created  time.Time
	updated  time.Time
	reviews  []cReview
	threads  []*cThread
	convo    []cComment
	approved map[string]bool // logins approving the current head (CodeCommit approval states)
}

type cReview struct {
	id      int64
	login   string
	verdict string // approve | request_changes | comment
	commit  string
	at      time.Time
}

type cThread struct {
	id       string
	path     string
	side     string // new | old
	line     int
	commit   string
	resolved bool
	comments []cComment
}

type cComment struct {
	id    int64
	login string
	body  string
	at    time.Time
}

func newCore(bot string, rateHeaders bool) *core {
	return &core{pulls: map[int]*cPull{}, next: 1, statuses: map[string]map[string]string{}, users: map[string]string{},
		bot: bot, rate: [2]int{1999, 2000}, reset: time.Now().Add(time.Hour).Truncate(time.Second), rateHeaders: rateHeaders}
}

func (c *core) id() int64 {
	c.seq++
	return 7000 + c.seq
}

// sha is a commit id that looks like one: abbreviations of different
// commits differ (Bitbucket abbreviates to 12 characters).
func (c *core) sha() string {
	c.seq++
	sum := sha1.Sum([]byte(fmt.Sprintf("commit-%d-%p", c.seq, c)))
	return hex.EncodeToString(sum[:])
}

func (c *core) now() time.Time {
	c.version++
	return time.Now().UTC().Truncate(time.Second).Add(time.Duration(c.version) * time.Second)
}

func (c *core) touch(p *cPull) { p.updated = c.now() }

func (c *core) pull(t *testing.T, id string) *cPull {
	t.Helper()
	n, _ := strconv.Atoi(id)
	p, ok := c.pulls[n]
	if !ok {
		t.Fatalf("fake host: no change %s", id)
	}
	return p
}

// whoami maps a request's token to a login (unknown tokens are the bot).
func (c *core) whoami(token string) string {
	if l, ok := c.users[token]; ok {
		return l
	}
	return c.bot
}

// gate answers a request the test asked to refuse or lose, and counts it.
// It returns true when the request was answered.
func (c *core) gate(w http.ResponseWriter, write bool) bool {
	c.requests++
	if c.rateHeaders {
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(c.rate[0]))
		w.Header().Set("RateLimit-Limit", strconv.Itoa(c.rate[1]))
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(c.reset.Unix(), 10))
	}
	if rj := c.reject; rj != nil {
		c.reject = nil
		w.Header().Set("Retry-After", strconv.Itoa(int(rj.retry.Seconds())))
		http.Error(w, `{"message":"Too Many Requests"}`, rj.status)
		return true
	}
	return false
}

// lost answers 502 after a write when the test asked for a lost response.
func (c *core) lost(w http.ResponseWriter) bool {
	if !c.loseNext {
		return false
	}
	c.loseNext = false
	http.Error(w, `{"message":"Bad Gateway"}`, http.StatusBadGateway)
	return true
}

// blocked says why p cannot merge ("" when it can).
func (c *core) blocked(p *cPull) string {
	if p.state != "open" {
		return "not open"
	}
	for _, ctx := range c.required {
		if c.statuses[p.head][ctx] != "success" {
			return "required check " + ctx + " has not passed"
		}
	}
	return ""
}

func (c *core) setStatus(sha, context, state string) {
	if c.statuses[sha] == nil {
		c.statuses[sha] = map[string]string{}
	}
	c.statuses[sha][context] = state
}

// open lists open pull requests, most recently updated first.
func (c *core) open() []*cPull {
	var out []*cPull
	for _, p := range c.pulls {
		if p.state == "open" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out
}

func (c *core) addThread(p *cPull, login, path, side string, line int, commit, body string) (*cThread, cComment) {
	cm := cComment{id: c.id(), login: login, body: body, at: c.now()}
	th := &cThread{id: strconv.FormatInt(cm.id, 10), path: path, side: side, line: line, commit: commit, comments: []cComment{cm}}
	p.threads = append(p.threads, th)
	c.touch(p)
	return th, cm
}

func (c *core) threadByID(p *cPull, id string) *cThread {
	for _, th := range p.threads {
		if th.id == id {
			return th
		}
		for _, cm := range th.comments {
			if strconv.FormatInt(cm.id, 10) == id {
				return th
			}
		}
	}
	return nil
}

func (c *core) reply(p *cPull, th *cThread, login, body string) cComment {
	cm := cComment{id: c.id(), login: login, body: body, at: c.now()}
	th.comments = append(th.comments, cm)
	c.touch(p)
	return cm
}

func (c *core) addConvo(p *cPull, login, body string) cComment {
	cm := cComment{id: c.id(), login: login, body: body, at: c.now()}
	p.convo = append(p.convo, cm)
	c.touch(p)
	return cm
}

func (c *core) addReview(p *cPull, login, verdict, commit string) cReview {
	rv := cReview{id: c.id(), login: login, verdict: verdict, commit: commit, at: c.now()}
	p.reviews = append(p.reviews, rv)
	c.touch(p)
	return rv
}

// ---- the Host methods every core-based fake shares ----

func (c *core) Open(t *testing.T, title string) (string, *vcs.Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.next
	c.next++
	p := &cPull{number: n, title: title, state: "open", head: c.sha(), base: c.sha(), headRef: "feature/" + strconv.Itoa(n),
		author: "author1", created: time.Now().UTC().Truncate(time.Second), approved: map[string]bool{}}
	c.touch(p)
	c.pulls[n] = p
	return strconv.Itoa(n), nil
}

func (c *core) Push(t *testing.T, id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	p.head = c.sha()
	p.approved = map[string]bool{}
	c.touch(p)
	return p.head
}

func (c *core) Close(t *testing.T, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	p.state = "closed"
	c.touch(p)
}

func (c *core) Review(t *testing.T, id, reviewer, verdict string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	c.addReview(p, reviewer, verdict, p.head)
	if verdict == "approve" {
		p.approved[reviewer] = true
	}
}

func (c *core) Head(t *testing.T, id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pull(t, id).head
}

func (c *core) Protect(contexts ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.required = contexts
}

func (c *core) Check(_ *testing.T, sha, context string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statuses[sha][context]
}

func (c *core) HostMerge(t *testing.T, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	if msg := c.blocked(p); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	p.state = "merged"
	c.touch(p)
	return nil
}

func (c *core) HostThread(t *testing.T, id, login, body string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	th, _ := c.addThread(p, login, "app.go", "new", 1, p.head, body)
	return th.id
}

func (c *core) CommentsOn(t *testing.T, id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pull(t, id)
	var out []string
	for _, th := range p.threads {
		for _, cm := range th.comments {
			out = append(out, cm.login+": "+cm.body)
		}
	}
	for _, cm := range p.convo {
		out = append(out, cm.login+": "+cm.body)
	}
	return out
}

func (c *core) RateLimit(remaining, limit int, reset time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rate, c.reset = [2]int{remaining, limit}, reset
}

func (c *core) RejectNext(status int, retry time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reject = &rejection{status: status, retry: retry}
}

func (c *core) RateHeaders() bool { return c.rateHeaders }

// AddUser makes token act as login.
func (c *core) AddUser(token, login string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[token] = login
}

// LoseNextResponse: the next write succeeds but answers 502.
func (c *core) LoseNextResponse() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loseNext = true
}

// Requests counts API requests served.
func (c *core) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}
