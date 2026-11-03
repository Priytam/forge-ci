package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
)

// TestCacheSaveIsAtomic: a save that fails the size cap must leave the cache
// that was already there. The old code wrote every save for a key over one
// fixed object, so an over-cap upload overwrote the good archive and then
// deleted it, and every later run missed until a save under the cap succeeded.
// Replaced objects are deleted, so blobs don't pile up.
func TestCacheSaveIsAtomic(t *testing.T) {
	st, db := newCFRStore(t, "")
	dir := t.TempDir()
	srv := New(st, blob.NewLocal(dir), nil)
	srv.runnerAuth = "off"
	srv.maxCacheBytes = 16
	h := &apiHarness{t: t, st: st, db: db, srv: srv}
	p := h.seedPipeline("acme/cache", "main", prodYAML)
	jobID := h.jobID(p.ID, "ship")
	cacheURL := "/api/v1/runner/jobs/" + pid(jobID) + "/cache?key=gomod-abc"

	put := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, cacheURL, strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	get := func(url string) (int, string, string) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		b, _ := io.ReadAll(rec.Body)
		return rec.Code, string(b), rec.Header().Get("X-Cache-Key")
	}

	if code := put("first"); code != http.StatusCreated {
		t.Fatalf("first save = %d", code)
	}
	if code := put("second"); code != http.StatusCreated {
		t.Fatalf("second save = %d", code)
	}
	if code := put(strings.Repeat("x", 64)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap save = %d, want 413", code)
	}
	if code, body, _ := get(cacheURL); code != 200 || body != "second" {
		t.Fatalf("restore after over-cap save = %d %q, want 200 \"second\"", code, body)
	}
	var objects int
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			objects++
		}
		return nil
	})
	if objects != 1 {
		t.Errorf("%d cache objects on disk, want 1 (replaced ones deleted)", objects)
	}

	// A lockfile change: the new hashed key misses, and the bare prefix
	// fallback warms from the newest gomod-* cache.
	code, body, matched := get("/api/v1/runner/jobs/" + pid(jobID) + "/cache?key=gomod-def&fallback=gomod")
	if code != 200 || body != "second" || matched != "gomod-abc" {
		t.Fatalf("prefix fallback = %d %q (key %q), want 200 \"second\" from gomod-abc", code, body, matched)
	}
}
