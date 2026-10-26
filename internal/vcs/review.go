package vcs

// This file is the code-review half of the provider contract. The CI half
// (Provider) reads files and posts statuses; this half lets Forge show and
// gate pull/merge requests ("changes") from any host with one model.
//
// A host implements only what it supports. ChangeSource is the read side
// (webhooks → events, one call per change refresh, a conditional list for
// reconcile); Capabilities says what the host can do natively, and Forge
// fills every gap itself, so the UI branches on capabilities, never on the
// provider's name.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Change is one pull/merge request in Forge's canonical shape.
type Change struct {
	ID           string // the host's per-repo number (or Forge's, for native changes)
	Title        string
	Body         string
	Author       string // host login
	SourceBranch string
	TargetBranch string
	State        string // open | merged | closed
	Draft        bool
	HeadSHA      string
	BaseSHA      string
	Labels       []string
	URL          string // the change on the host's web UI ("" when there is none)
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Reviews is the host's complete review list. nil means the source did not
	// say (a webhook snapshot); an empty slice means there are none.
	Reviews []Review
}

// Review is one verdict on a change.
type Review struct {
	ExternalID  string
	Reviewer    string
	Verdict     string // approve | request_changes | comment | dismissed
	Body        string
	CommitSHA   string // head the review was made on ("" when unknown)
	SubmittedAt time.Time
}

// ChangeRef is a change as a list returns it: enough to decide whether the
// stored copy is stale.
type ChangeRef struct {
	ID        string
	HeadSHA   string
	UpdatedAt time.Time
}

// ChangeEvent is a webhook delivery normalised. Snapshot is set when the
// payload carries the whole change, so the UI can update before the refresh.
type ChangeEvent struct {
	Repo       string
	ChangeID   string
	Kind       string // change | review
	Action     string // the host's action word, for logs
	DeliveryID string
	Snapshot   *Change
}

// Validators are a host's conditional-request validators (ETag and
// Last-Modified), stored per repo so a reconcile that finds nothing new costs
// a 304 — free on GitHub.
type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// Capabilities says what a host can do natively. Forge provides the rest.
type Capabilities struct {
	// Changes: the host has pull/merge requests Forge can list and read.
	Changes bool `json:"changes"`
	// NativeChanges: the host has none, so changes are created and stored by
	// Forge (plain git).
	NativeChanges bool     `json:"native_changes"`
	Threads       bool     `json:"threads"`
	Suggestions   bool     `json:"suggestions"`
	Resolve       bool     `json:"resolve"`
	Drafts        bool     `json:"drafts"`
	MergeMethods  []string `json:"merge_methods"`
	// StatusChecks: the host can show (and require) a commit status, so the
	// merge gate is enforced even when someone merges on the host.
	StatusChecks bool `json:"status_checks"`
	// Diffs: Forge can mirror the host with plain git and diff locally (set
	// for every provider that is Mirrorable; never from a diff API).
	Diffs bool `json:"diffs"`
}

// Mirrorable is implemented by hosts Forge can mirror with plain git. It
// names the refspecs that carry a change's commits, fetched into the
// runner's blobless mirror only when a commit is missing.
type Mirrorable interface {
	MirrorRefs(id, sourceBranch, targetBranch string) []string
}

// MirrorRefsFor returns provider's refspecs for a change, ok=false when the
// host cannot be mirrored (yet).
func MirrorRefsFor(provider, id, source, target string) ([]string, bool) {
	m, ok := Lookup(provider).(Mirrorable)
	if !ok {
		return nil, false
	}
	return m.MirrorRefs(id, source, target), true
}

func headRef(branch string) string {
	return "+refs/heads/" + branch + ":refs/heads/" + branch
}

// ChangeSource is the read side of a host's change API.
type ChangeSource interface {
	// NormalizeEvent turns one webhook delivery into change events. Events
	// that do not concern a change return none (and no error).
	NormalizeEvent(hdr http.Header, body []byte) ([]ChangeEvent, error)
	// GetChange reads one change with its reviews in as few calls as the host
	// allows. prev is the stored copy (nil on first sight); hosts with no
	// change API read the branches it names.
	GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, prev *Change) (*Change, error)
	// ListOpenChanges lists open changes conditionally: ErrNotModified when
	// nothing changed since v. complete=false when the list was cut short.
	ListOpenChanges(ctx context.Context, c *ChangeClient, repo RepoRef, v Validators) (refs []ChangeRef, next Validators, complete bool, err error)
}

// GateContext is the commit-status name the merge gate is published under;
// branch protection on the host requires it.
const GateContext = "forge/merge-gate"

// CheckResult is the merge gate as published on a head commit.
type CheckResult struct {
	State       string // success | pending | failure
	Description string // one line (hosts cap it; GitHub at 140 characters)
	TargetURL   string // the change page in Forge
}

// MergeOpts is a merge request. ExpectedHead makes it safe: the host refuses
// when someone pushed since the gate passed (ErrHeadMoved).
type MergeOpts struct {
	Method       string // one of Capabilities.MergeMethods
	ExpectedHead string
}

// ChangeWriter is the write side. Every call is made by the outbox worker,
// never by a page, and goes through the ChangeClient like reads do.
type ChangeWriter interface {
	PublishCheck(ctx context.Context, c *ChangeClient, repo RepoRef, sha string, res CheckResult) error
	Merge(ctx context.Context, c *ChangeClient, repo RepoRef, id string, opts MergeOpts) (mergeSHA string, err error)
}

// ChangeWriterFor returns the host's change writer, or nil when it has none.
func ChangeWriterFor(provider string) ChangeWriter {
	w, _ := Lookup(provider).(ChangeWriter)
	return w
}

// Capable is implemented by providers that declare their capabilities.
type Capable interface {
	Capabilities() Capabilities
}

var (
	// ErrNotModified: a conditional request found nothing new.
	ErrNotModified = errors.New("not modified")
	// ErrNoChangeAPI: the host has no change list; changes are Forge-native.
	ErrNoChangeAPI = errors.New("provider has no change API")
	// ErrChangeNotFound: the host does not know the change (deleted repo,
	// wrong number, or a native change with no branches recorded).
	ErrChangeNotFound = errors.New("change not found on host")
	// ErrHeadMoved: a merge named a head the change no longer has.
	ErrHeadMoved = errors.New("the change has new commits since it was approved")
	// ErrNotMergeable: the host refused the merge (conflicts, its own rules,
	// a required check). The wrapped StatusError says why.
	ErrNotMergeable = errors.New("the host refused the merge")
	// ErrReadPath: a provider call was attempted while serving a page. Pages
	// read Postgres and the blob store only; the call is refused and counted.
	ErrReadPath = errors.New("provider call refused on the read path")
)

// StatusError: the host answered with an error status (not rate limiting).
// Message is the host's own one-line explanation, when it gave one (e.g.
// GitHub's "Required status check ... is expected").
type StatusError struct {
	Provider   string
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("vcs: %s change API: HTTP %d: %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("vcs: %s change API: HTTP %d", e.Provider, e.StatusCode)
}

// Permanent reports whether retrying cannot help (bad credential, missing
// permission, unknown repo): Forge then stops calling until something new —
// a webhook, a reconnect — asks again, instead of retrying forever.
func Permanent(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && isPermanent(se.StatusCode) || errors.Is(err, ErrChangeNotFound)
}

// RateLimitedError: the host refused the call for rate limiting. RetryAfter
// is when to try again (from Retry-After or the reset header).
type RateLimitedError struct {
	RetryAfter time.Duration
	StatusCode int
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited (HTTP %d), retry after %s", e.StatusCode, e.RetryAfter.Round(time.Second))
}

// ChangeSourceFor returns the host's change source, or nil when the provider
// has none.
func ChangeSourceFor(provider string) ChangeSource {
	cs, _ := Lookup(provider).(ChangeSource)
	return cs
}

// CapabilitiesOf returns what provider supports natively. Providers that do
// not declare capabilities support nothing, and Forge fills every gap.
func CapabilitiesOf(provider string) Capabilities {
	if c, ok := Lookup(provider).(Capable); ok {
		caps := c.Capabilities()
		if caps.MergeMethods == nil {
			caps.MergeMethods = []string{}
		}
		_, caps.Diffs = Lookup(provider).(Mirrorable)
		return caps
	}
	_, diffs := Lookup(provider).(Mirrorable)
	return Capabilities{MergeMethods: []string{}, Diffs: diffs}
}

// ---- call accounting ----

// Call classes, in priority order. When a credential's budget runs low the
// lower classes wait; user actions never do.
const (
	ClassUser      = "user"      // a person clicked something
	ClassWebhook   = "webhook"   // refresh after a webhook
	ClassReconcile = "reconcile" // the periodic conditional list
	ClassBackfill  = "backfill"  // first import of a repo
)

type ctxKey int

const (
	keyClass ctxKey = iota
	keyReadPath
)

// WithClass tags provider calls made with ctx.
func WithClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, keyClass, class)
}

// ClassOf returns ctx's call class (ClassWebhook when unset).
func ClassOf(ctx context.Context) string {
	if c, ok := ctx.Value(keyClass).(string); ok && c != "" {
		return c
	}
	return ClassWebhook
}

// ReadPath marks ctx as serving a page: every provider call made with it is
// refused (ErrReadPath) and counted, so a page can never spend quota.
func ReadPath(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyReadPath, true)
}

// IsReadPath reports whether ctx is serving a page.
func IsReadPath(ctx context.Context) bool {
	v, _ := ctx.Value(keyReadPath).(bool)
	return v
}

// RateInfo is what a host's rate-limit headers said.
type RateInfo struct {
	Remaining int
	Limit     int
	Reset     time.Time
}

// CallInfo describes one provider call for accounting.
type CallInfo struct {
	Provider string
	Class    string
	ReadPath bool
	Result   string    // ok | not_modified | error | rate_limited | refused
	Rate     *RateInfo // nil when the host sent no rate-limit headers
}

// ChangeClient makes change-API calls. Every call goes through do(), which
// refuses read-path calls, reads rate-limit headers and reports each call to
// OnCall, so accounting and budgets cannot be bypassed by a plugin.
type ChangeClient struct {
	client     *http.Client
	githubBase string
	// OnCall, when set, is told about every call (including refused ones).
	OnCall func(ctx context.Context, info CallInfo)

	mu    sync.Mutex
	calls int
}

// NewChangeClient builds a client from the environment (GITHUB_API_BASE, like
// the Fetcher, so tests point it at a stub).
func NewChangeClient() *ChangeClient {
	return &ChangeClient{
		client:     &http.Client{Timeout: 20 * time.Second},
		githubBase: apiBase("GITHUB_API_BASE", "https://api.github.com"),
	}
}

// Calls returns how many provider calls this client attempted.
func (c *ChangeClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// report counts a call and hands it to OnCall.
func (c *ChangeClient) report(ctx context.Context, info CallInfo) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	info.Class, info.ReadPath = ClassOf(ctx), IsReadPath(ctx)
	if c.OnCall != nil {
		c.OnCall(ctx, info)
	}
}

// guard refuses a call made on the read path. Non-HTTP calls (git) use it
// directly; HTTP calls get it through do().
func (c *ChangeClient) guard(ctx context.Context, provider string) error {
	if IsReadPath(ctx) {
		c.report(ctx, CallInfo{Provider: provider, Result: "refused"})
		return ErrReadPath
	}
	return nil
}

// do sends req and returns the response for 2xx and 304. Rate limiting is a
// *RateLimitedError; any other status is an error naming it (never a body).
func (c *ChangeClient) do(ctx context.Context, provider string, req *http.Request) (*http.Response, error) {
	if err := c.guard(ctx, provider); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		c.report(ctx, CallInfo{Provider: provider, Result: "error"})
		return nil, err
	}
	rate := parseRateHeaders(resp.Header)
	switch {
	case resp.StatusCode == http.StatusNotModified:
		c.report(ctx, CallInfo{Provider: provider, Result: "not_modified", Rate: rate})
		return resp, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		c.report(ctx, CallInfo{Provider: provider, Result: "ok", Rate: rate})
		return resp, nil
	}
	msg := hostMessage(resp.Body)
	resp.Body.Close()
	if rl := rateLimited(resp, rate); rl != nil {
		c.report(ctx, CallInfo{Provider: provider, Result: "rate_limited", Rate: rate})
		return nil, rl
	}
	c.report(ctx, CallInfo{Provider: provider, Result: "error", Rate: rate})
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrChangeNotFound
	}
	return nil, &StatusError{Provider: provider, StatusCode: resp.StatusCode, Message: msg}
}

// hostMessage reads the "message" field of a JSON error body (GitHub, Gitea,
// GitLab), capped, for showing why the host refused. Never the request.
func hostMessage(body io.Reader) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 4<<10)).Decode(&out); err != nil {
		return ""
	}
	m := strings.TrimSpace(out.Message)
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

// parseRateHeaders reads the common X-RateLimit-* headers (GitHub, Gitea, and
// GitLab's RateLimit-* spelling). nil when the host sent none.
func parseRateHeaders(h http.Header) *RateInfo {
	get := func(names ...string) string {
		for _, n := range names {
			if v := h.Get(n); v != "" {
				return v
			}
		}
		return ""
	}
	rem := get("X-RateLimit-Remaining", "RateLimit-Remaining")
	if rem == "" {
		return nil
	}
	ri := &RateInfo{}
	ri.Remaining, _ = strconv.Atoi(rem)
	ri.Limit, _ = strconv.Atoi(get("X-RateLimit-Limit", "RateLimit-Limit"))
	if v, err := strconv.ParseInt(get("X-RateLimit-Reset", "RateLimit-Reset"), 10, 64); err == nil && v > 0 {
		ri.Reset = time.Unix(v, 0)
	}
	return ri
}

// rateLimited recognises a rate-limit refusal: 429, or 403 with Retry-After
// or an exhausted primary limit (GitHub's secondary and primary limits).
func rateLimited(resp *http.Response, rate *RateInfo) *RateLimitedError {
	retry := time.Duration(0)
	if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v >= 0 {
		retry = time.Duration(v) * time.Second
	}
	exhausted := rate != nil && rate.Remaining == 0
	if resp.StatusCode != http.StatusTooManyRequests &&
		!(resp.StatusCode == http.StatusForbidden && (retry > 0 || exhausted)) {
		return nil
	}
	if retry == 0 && rate != nil && !rate.Reset.IsZero() {
		retry = time.Until(rate.Reset)
	}
	if retry <= 0 {
		retry = time.Minute
	}
	return &RateLimitedError{RetryAfter: retry, StatusCode: resp.StatusCode}
}

// ---- helpers shared by plugins ----

// nextLink returns the rel="next" URL of an RFC 5988 Link header.
func nextLink(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 || !strings.Contains(segs[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(segs[0]), "<>")
	}
	return ""
}
