package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// reviewHarness: acme/shop connected to a fake GitHub that counts every
// request — contents, GraphQL, REST, App tokens — so "no provider call" is
// checked at the host, not just at Forge's client.
type reviewHarness struct {
	*apiHarness
	gh *vcstest.FakeGitHub
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	gh := vcstest.NewFakeGitHub(t)
	st, db := newCFRStore(t, gh.Server.URL)
	repo := gh.Repo()
	if err := st.RegisterRepo(context.Background(), proto.RepoRegistration{Repo: repo.Repo, Provider: "github",
		CloneURL: repo.CloneURL, Token: repo.Token, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	return &reviewHarness{apiHarness: &apiHarness{t: t, st: st, db: db, srv: New(st, nil, nil)}, gh: gh}
}

// webhook posts a GitHub delivery through the real handler.
func (h *reviewHarness) webhook(id, kind string) {
	h.t.Helper()
	hdr, body, _ := h.gh.Delivery(h.t, id, kind)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = v
	}
	h.srv.ServeHTTP(httptest.NewRecorder(), req)
}

// seedChange runs a webhook and its refresh, returning the change id.
func (h *reviewHarness) seedChange() int64 {
	h.t.Helper()
	id, _ := h.gh.Open(h.t, "Seat-level lock")
	h.gh.Review(h.t, id, "ashwin", "approve")
	h.webhook(id, "change")
	if _, err := h.db.Exec(context.Background(), `UPDATE changes SET refresh_due_at = now()`); err != nil {
		h.t.Fatal(err)
	}
	if n := review.New(h.st).RefreshDue(context.Background()); n != 1 {
		h.t.Fatalf("refresh claimed %d", n)
	}
	list, _, err := h.st.ListChanges(context.Background(), store.ChangeQuery{})
	if err != nil || len(list) != 1 || list[0].Approvals != 1 {
		h.t.Fatalf("seeded inbox = %+v %v", list, err)
	}
	return list[0].ID
}

// TestChangeReadRoutesMakeNoProviderCalls walks EVERY registered read route
// (changeReadRoutes) and fails if any of them reaches the host. A new view
// route is covered by registering it in that table.
func TestChangeReadRoutesMakeNoProviderCalls(t *testing.T) {
	h := newReviewHarness(t)
	changeID := h.seedChange()
	before := h.gh.Requests()
	for pattern := range h.srv.changeReadRoutes() {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{id}", strconv.FormatInt(changeID, 10))
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		req := httptest.NewRequest(method, path, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, req)
		cancel()
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", pattern, rec.Code, rec.Body.String())
		}
		if got := h.gh.Requests(); got != before {
			t.Fatalf("%s made %d provider call(s) — pages must read Postgres only", pattern, got-before)
		}
	}
	var m codeReviewStats
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/code-review/metrics", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.ReadPathCalls != 0 || m.TotalCalls == 0 || m.OpenChanges != 1 {
		t.Errorf("metrics = %+v %v", m, err)
	}
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/metrics", nil)
	if !strings.Contains(rec.Body.String(), "forge_provider_read_path_calls_24h 0\n") ||
		!strings.Contains(rec.Body.String(), `forge_provider_calls_24h{provider="github",class="webhook",result="ok"} 1`) {
		t.Errorf("prometheus metrics missing code review:\n%s", rec.Body.String())
	}
}

// TestReadPathGuardRefusesProviderCalls: a read handler that DID call the
// host — through the change client or by resolving a credential — is refused
// before any request leaves, and the refusal shows up in the metric.
func TestReadPathGuardRefusesProviderCalls(t *testing.T) {
	h := newReviewHarness(t)
	id, _ := h.gh.Open(t, "x")
	before := h.gh.Requests()
	var errs []error
	bad := readPath(func(w http.ResponseWriter, r *http.Request) {
		_, _, _, err := h.st.ReviewTarget(r.Context(), vcstest.FakeRepo)
		errs = append(errs, err)
		_, err = vcs.ChangeSourceFor("github").GetChange(r.Context(), review.New(h.st).Client(), h.gh.Repo(), id, nil)
		errs = append(errs, err)
	})
	bad(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	for _, err := range errs {
		if !errors.Is(err, vcs.ErrReadPath) {
			t.Errorf("read-path call err = %v, want ErrReadPath", err)
		}
	}
	if h.gh.Requests() != before {
		t.Fatal("a read-path call reached the host")
	}
	var m codeReviewStats
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/code-review/metrics", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.ReadPathCalls != 2 {
		t.Errorf("read_path_calls = %d (%v), want 2", m.ReadPathCalls, err)
	}
}

func TestChangeListETagAndDetail(t *testing.T) {
	h := newReviewHarness(t)
	id := h.seedChange()
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/changes", nil)
	h.expect(rec, http.StatusOK, "list")
	etag := rec.Header().Get("ETag")
	var list []store.ChangeSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || etag == "" ||
		rec.Header().Get("X-Total-Count") != "1" {
		t.Fatalf("list = %s (etag %q)", rec.Body.String(), etag)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/changes", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Errorf("unchanged list = %d (%d bytes), want 304", rec2.Code, rec2.Body.Len())
	}
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/changes/"+strconv.FormatInt(id, 10), nil)
	var detail struct {
		Change       store.ChangeDetail `json:"change"`
		Capabilities vcs.Capabilities   `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.Change.Reviews[0].Reviewer != "ashwin" ||
		!detail.Capabilities.Changes || len(detail.Change.RevisionList) != 1 {
		t.Errorf("detail = %s", rec.Body.String())
	}
	h.expect(h.do(testCaller{}, http.MethodGet, "/api/v1/changes/999999", nil), http.StatusNotFound, "missing change")
	h.expect(h.do(testCaller{}, http.MethodGet, "/api/v1/changes?state=nope", nil), http.StatusBadRequest, "bad state")
}

// TestChangeStreamPushesUpdates: a change written by a webhook reaches an
// open page as one SSE event, with no polling.
func TestChangeStreamPushesUpdates(t *testing.T) {
	h := newReviewHarness(t)
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/changes/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	waitFor := func(prefix string) string {
		for lines.Scan() {
			if strings.HasPrefix(lines.Text(), prefix) {
				return lines.Text()
			}
		}
		t.Fatalf("stream ended before %q: %v", prefix, lines.Err())
		return ""
	}
	waitFor("event: ready")
	// The listener connects asynchronously; give LISTEN a moment.
	time.Sleep(200 * time.Millisecond)
	id, _ := h.gh.Open(t, "Live")
	h.webhook(id, "change")
	waitFor("event: change")
	if data := waitFor("data: "); !strings.Contains(data, `"id":`) {
		t.Errorf("event data = %q", data)
	}
}
