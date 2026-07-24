package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// quietLogger returns a logStreamer that discards output (nil client, never
// flushed) — enough for the pure key-resolution helpers under test.
func quietLogger() *logStreamer {
	return &logStreamer{done: make(chan struct{})}
}

func TestResolveCacheKeyLiteral(t *testing.T) {
	job := &proto.RunnerJob{CachePaths: []string{"vendor/"}, CacheKey: "v1"}
	primary, restore := resolveCacheKey(quietLogger(), job, t.TempDir())
	if primary != "v1" {
		t.Errorf("primary = %q, want v1", primary)
	}
	if len(restore) != 1 || restore[0] != "v1" {
		t.Errorf("restore = %v, want [v1]", restore)
	}
}

func TestResolveCacheKeyDefaultsWhenEmpty(t *testing.T) {
	job := &proto.RunnerJob{CachePaths: []string{"vendor/"}}
	primary, restore := resolveCacheKey(quietLogger(), job, t.TempDir())
	if primary != "default" || len(restore) != 1 || restore[0] != "default" {
		t.Errorf("primary=%q restore=%v, want default", primary, restore)
	}
}

func TestResolveCacheKeyNoCache(t *testing.T) {
	job := &proto.RunnerJob{}
	primary, restore := resolveCacheKey(quietLogger(), job, t.TempDir())
	if primary != "" || restore != nil {
		t.Errorf("expected empty for no-cache job, got primary=%q restore=%v", primary, restore)
	}
}

// TestResolveCacheKeyFilesHashHitMiss is the core content-addressing evidence:
// the same file contents produce the same key (a hit), a changed file produces
// a different key (a clean miss), and the bare prefix is always the fallback.
func TestResolveCacheKeyFilesHashHitMiss(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "go.sum")
	if err := os.WriteFile(lock, []byte("hash-abc-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &proto.RunnerJob{
		CachePaths:    []string{"vendor/"},
		CacheKey:      "v1",
		CacheKeyFiles: []string{"go.sum"},
	}

	p1, r1 := resolveCacheKey(quietLogger(), job, dir)
	if p1 == "v1" {
		t.Fatalf("files-key should append a hash, got bare prefix %q", p1)
	}
	if len(r1) != 2 || r1[0] != p1 || r1[1] != "v1" {
		t.Fatalf("restore chain = %v, want [%s v1]", r1, p1)
	}

	// Unchanged contents -> identical key (a HIT).
	p2, _ := resolveCacheKey(quietLogger(), job, dir)
	if p2 != p1 {
		t.Errorf("unchanged lockfile changed the key: %q vs %q", p1, p2)
	}

	// Changed contents -> different key (a clean MISS).
	if err := os.WriteFile(lock, []byte("hash-def-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p3, _ := resolveCacheKey(quietLogger(), job, dir)
	if p3 == p1 {
		t.Errorf("changed lockfile produced same key %q (should miss)", p3)
	}
}

func TestHashCacheFilesOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1 := hashCacheFiles(quietLogger(), dir, []string{"a.txt", "b.txt"})
	h2 := hashCacheFiles(quietLogger(), dir, []string{"b.txt", "a.txt"})
	if h1 != h2 {
		t.Errorf("hash depends on file order: %q vs %q", h1, h2)
	}
}

func TestCachePolicyHelpers(t *testing.T) {
	cases := []struct {
		policy        string
		restore, save bool
	}{
		{"", true, true},
		{"pull-push", true, true},
		{"pull", true, false},
		{"push", false, true},
	}
	for _, c := range cases {
		if got := cachePolicyRestores(c.policy); got != c.restore {
			t.Errorf("cachePolicyRestores(%q) = %v, want %v", c.policy, got, c.restore)
		}
		if got := cachePolicySaves(c.policy); got != c.save {
			t.Errorf("cachePolicySaves(%q) = %v, want %v", c.policy, got, c.save)
		}
	}
}
