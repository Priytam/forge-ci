// Package vcs posts Forge pipeline results back to the origin version-control
// host so a commit or pull request shows Forge's status (the green tick / red
// X). It knows two provider status APIs — GitHub commit statuses and Bitbucket
// build statuses — and maps Forge's pipeline phase onto each. Provider "other"
// has no status API and is never routed here.
//
// The store owns token decryption; this package receives a plaintext token per
// call and uses it only in the outbound Authorization header. Tokens are never
// logged and never appear in the errors returned here.
package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// StatusContext is the GitHub commit-status "context" and the Bitbucket build
// "key" — the stable identifier under which Forge's result appears on a commit.
const StatusContext = "forge-ci"

// MapGitHubState maps a Forge pipeline phase to a GitHub commit-status state
// (pending|success|failure|error) plus a human description.
//
//	running/pending/blocked -> pending
//	success                 -> success
//	failed                  -> failure
//	canceled                -> error   (the run did not complete; not a test failure)
func MapGitHubState(status string) (state, description string) {
	switch status {
	case "success":
		return "success", "Pipeline succeeded"
	case "failed":
		return "failure", "Pipeline failed"
	case "canceled":
		return "error", "Pipeline canceled"
	case "blocked":
		return "pending", "Waiting for approval"
	case "pending":
		return "pending", "Pipeline queued"
	default: // running (and any unknown phase) reads as in-progress
		return "pending", "Pipeline running"
	}
}

// MapBitbucketState maps a Forge pipeline phase to a Bitbucket build state
// (INPROGRESS|SUCCESSFUL|FAILED|STOPPED) plus a human description.
//
//	running/pending/blocked -> INPROGRESS
//	success                 -> SUCCESSFUL
//	failed                  -> FAILED
//	canceled                -> STOPPED
func MapBitbucketState(status string) (state, description string) {
	switch status {
	case "success":
		return "SUCCESSFUL", "Pipeline succeeded"
	case "failed":
		return "FAILED", "Pipeline failed"
	case "canceled":
		return "STOPPED", "Pipeline canceled"
	case "blocked":
		return "INPROGRESS", "Waiting for approval"
	case "pending":
		return "INPROGRESS", "Pipeline queued"
	default:
		return "INPROGRESS", "Pipeline running"
	}
}

// PostRequest is one commit-status write. Token is a plaintext VCS token used
// only in the Authorization header; it is never logged.
type PostRequest struct {
	Provider  string // github | bitbucket
	Repo      string // owner/name (github) or workspace/slug (bitbucket)
	SHA       string
	Token     string
	Status    string // Forge pipeline phase (mapped per provider)
	TargetURL string // link back to the Forge pipeline page
}

// PostError describes a delivery that failed after all attempts. Permanent is
// true for client-side configuration errors (bad token/scope, wrong repo/sha)
// where retrying will not help; the scheduler uses it to decide whether to keep
// the dedup claim (permanent) or release it for a later retry (transient). The
// error text never contains the token.
type PostError struct {
	StatusCode int // last HTTP status (0 = transport error before a response)
	Permanent  bool
	Attempts   int
	Err        error // last transport error, if any
}

func (e *PostError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("commit-status delivery failed after %d attempt(s): HTTP %d", e.Attempts, e.StatusCode)
	}
	return fmt.Sprintf("commit-status delivery failed after %d attempt(s): %v", e.Attempts, e.Err)
}

func (e *PostError) Unwrap() error { return e.Err }

// isPermanent reports whether an HTTP status is a client configuration error
// that retrying cannot fix. 429 and 5xx are transient.
func isPermanent(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// Poster delivers commit statuses with a bounded, backed-off retry policy. The
// provider API base URLs are overridable via GITHUB_API_BASE / BITBUCKET_API_BASE
// (for testing against a local stub).
type Poster struct {
	client        *http.Client
	githubBase    string
	bitbucketBase string
	maxAttempts   int
	backoff       time.Duration
}

// NewPoster builds a Poster from the environment. GITHUB_API_BASE and
// BITBUCKET_API_BASE default to the public API hosts.
func NewPoster() *Poster {
	return &Poster{
		client:        &http.Client{Timeout: 10 * time.Second},
		githubBase:    apiBase("GITHUB_API_BASE", "https://api.github.com"),
		bitbucketBase: apiBase("BITBUCKET_API_BASE", "https://api.bitbucket.org"),
		maxAttempts:   4,
		backoff:       300 * time.Millisecond,
	}
}

func apiBase(env, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return fallback
}

// Post delivers one status, retrying transient failures up to maxAttempts with
// exponential backoff. It returns nil on a 2xx, ctx.Err() if the context is
// canceled mid-flight, or a *PostError once attempts are exhausted. Every
// failure (including 401/403/404) is retried per policy; the returned
// PostError.Permanent lets the caller decide whether a further retry on a later
// tick is worthwhile.
func (p *Poster) Post(ctx context.Context, req PostRequest) error {
	url, body, err := p.build(req)
	if err != nil {
		return err
	}
	var last *PostError
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		code, perr := p.attempt(ctx, url, req.Token, body)
		if perr == nil && code >= 200 && code < 300 {
			return nil
		}
		last = &PostError{StatusCode: code, Attempts: attempt, Err: perr, Permanent: isPermanent(code)}
		if attempt < p.maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.backoff * time.Duration(int64(1)<<(attempt-1))):
			}
		}
	}
	return last
}

func (p *Poster) attempt(ctx context.Context, url, token string, body []byte) (int, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain (bounded) so the connection can be reused; the body is not needed.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// build produces the provider-specific request URL and JSON body.
func (p *Poster) build(req PostRequest) (url string, body []byte, err error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return "", nil, fmt.Errorf("invalid repo %q (want owner/name)", req.Repo)
	}
	switch req.Provider {
	case "github":
		state, desc := MapGitHubState(req.Status)
		body, _ = json.Marshal(map[string]string{
			"state":       state,
			"target_url":  req.TargetURL,
			"context":     StatusContext,
			"description": desc,
		})
		url = fmt.Sprintf("%s/repos/%s/%s/statuses/%s", p.githubBase, owner, name, req.SHA)
	case "bitbucket":
		state, desc := MapBitbucketState(req.Status)
		body, _ = json.Marshal(map[string]string{
			"key":         StatusContext,
			"state":       state,
			"url":         req.TargetURL,
			"name":        "Forge CI",
			"description": desc,
		})
		url = fmt.Sprintf("%s/2.0/repositories/%s/%s/commit/%s/statuses/build", p.bitbucketBase, owner, name, req.SHA)
	default:
		return "", nil, fmt.Errorf("provider %q has no commit-status API", req.Provider)
	}
	return url, body, nil
}

// splitRepo splits "owner/name" on the first slash.
func splitRepo(repo string) (owner, name string, ok bool) {
	i := strings.Index(repo, "/")
	if i <= 0 || i == len(repo)-1 {
		return "", "", false
	}
	return repo[:i], repo[i+1:], true
}
