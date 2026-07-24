package vcs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMapGitHubState(t *testing.T) {
	cases := map[string]string{
		"running":  "pending",
		"pending":  "pending",
		"blocked":  "pending",
		"success":  "success",
		"failed":   "failure",
		"canceled": "error",
	}
	for forge, want := range cases {
		got, desc := MapGitHubState(forge)
		if got != want {
			t.Errorf("MapGitHubState(%q) = %q, want %q", forge, got, want)
		}
		if desc == "" {
			t.Errorf("MapGitHubState(%q) description is empty", forge)
		}
	}
}

func TestMapBitbucketState(t *testing.T) {
	cases := map[string]string{
		"running":  "INPROGRESS",
		"pending":  "INPROGRESS",
		"blocked":  "INPROGRESS",
		"success":  "SUCCESSFUL",
		"failed":   "FAILED",
		"canceled": "STOPPED",
	}
	for forge, want := range cases {
		got, desc := MapBitbucketState(forge)
		if got != want {
			t.Errorf("MapBitbucketState(%q) = %q, want %q", forge, got, want)
		}
		if desc == "" {
			t.Errorf("MapBitbucketState(%q) description is empty", forge)
		}
	}
}

// recorded captures one inbound stub request.
type recorded struct {
	path string
	auth string
	body map[string]string
}

func TestPostGitHubShape(t *testing.T) {
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		reqs = append(reqs, recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Poster{client: srv.Client(), githubBase: srv.URL, maxAttempts: 4, backoff: time.Millisecond}
	err := p.Post(context.Background(), PostRequest{
		Provider:  "github",
		Repo:      "acme/checkout",
		SHA:       "deadbeefcafe",
		Token:     "ghp_secrettoken",
		Status:    "success",
		TargetURL: "http://localhost:5173/pipelines/42",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.path != "/repos/acme/checkout/statuses/deadbeefcafe" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer ghp_secrettoken" {
		t.Errorf("auth = %q, want bearer with token", got.auth)
	}
	if got.body["state"] != "success" {
		t.Errorf("state = %q, want success", got.body["state"])
	}
	if got.body["context"] != StatusContext {
		t.Errorf("context = %q, want %q", got.body["context"], StatusContext)
	}
	if got.body["target_url"] != "http://localhost:5173/pipelines/42" {
		t.Errorf("target_url = %q", got.body["target_url"])
	}
}

func TestPostBitbucketShape(t *testing.T) {
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		reqs = append(reqs, recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Poster{client: srv.Client(), bitbucketBase: srv.URL, maxAttempts: 4, backoff: time.Millisecond}
	err := p.Post(context.Background(), PostRequest{
		Provider:  "bitbucket",
		Repo:      "team/service",
		SHA:       "abc123",
		Token:     "bb_token",
		Status:    "failed",
		TargetURL: "http://localhost:5173/pipelines/7",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.path != "/2.0/repositories/team/service/commit/abc123/statuses/build" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer bb_token" {
		t.Errorf("auth = %q", got.auth)
	}
	if got.body["key"] != StatusContext {
		t.Errorf("key = %q, want %q", got.body["key"], StatusContext)
	}
	if got.body["state"] != "FAILED" {
		t.Errorf("state = %q, want FAILED", got.body["state"])
	}
	if got.body["url"] != "http://localhost:5173/pipelines/7" {
		t.Errorf("url = %q", got.body["url"])
	}
}

func TestPostRetriesOn401ThenGivesUp(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := &Poster{client: srv.Client(), githubBase: srv.URL, maxAttempts: 4, backoff: time.Millisecond}
	err := p.Post(context.Background(), PostRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc", Token: "bad", Status: "success",
	})
	if err == nil {
		t.Fatal("want error on persistent 401, got nil")
	}
	pe, ok := err.(*PostError)
	if !ok {
		t.Fatalf("want *PostError, got %T", err)
	}
	if pe.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", pe.StatusCode)
	}
	if !pe.Permanent {
		t.Error("401 should be classified Permanent")
	}
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Errorf("stub saw %d requests, want 4 (bounded retries)", got)
	}
}

func TestPostRetriesOn503ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p := &Poster{client: srv.Client(), githubBase: srv.URL, maxAttempts: 4, backoff: time.Millisecond}
	err := p.Post(context.Background(), PostRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc", Token: "t", Status: "success",
	})
	if err != nil {
		t.Fatalf("want success after transient 503s, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("stub saw %d requests, want 3", got)
	}
}

func TestPostErrorNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	p := &Poster{client: srv.Client(), githubBase: srv.URL, maxAttempts: 1, backoff: time.Millisecond}
	err := p.Post(context.Background(), PostRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc", Token: "ghp_supersecret", Status: "success",
	})
	if err == nil {
		t.Fatal("want error")
	}
	if got := err.Error(); contains(got, "ghp_supersecret") {
		t.Errorf("error text leaked the token: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
