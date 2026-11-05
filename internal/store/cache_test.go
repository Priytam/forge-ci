package store

import (
	"context"
	"errors"
	"testing"
)

// A fallback key matches itself or the newest "<key>-…" entry, literally: LIKE
// metacharacters in the key don't widen the match, and the primary key never
// prefix-matches.
func TestLookupCacheFallbackPrefix(t *testing.T) {
	ctx := context.Background()
	st := newDeployTestStore(t)
	const repo = "acme/cache"
	for _, k := range []string{"gomod-111", "gomod-222", "npm_x-1", "npmzx-9"} {
		if _, err := st.SaveCache(ctx, repo, k, "blob/"+k, 1); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := st.LookupCache(ctx, repo, []string{"gomod"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("primary key prefix-matched: err = %v, want ErrNotFound", err)
	}
	_, matched, err := st.LookupCache(ctx, repo, []string{"gomod-333", "gomod"})
	if err != nil || matched != "gomod-222" {
		t.Errorf("fallback = %q, %v; want newest gomod-222", matched, err)
	}
	_, matched, err = st.LookupCache(ctx, repo, []string{"none", "npm_x"})
	if err != nil || matched != "npm_x-1" {
		t.Errorf("fallback npm_x = %q, %v; want npm_x-1 (not npmzx-9)", matched, err)
	}
	if _, _, err := st.LookupCache(ctx, "other/repo", []string{"x", "gomod"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-repo fallback: err = %v, want ErrNotFound", err)
	}

	// SaveCache hands back the object it replaced so the caller can delete it.
	prev, err := st.SaveCache(ctx, repo, "gomod-222", "blob/new", 2)
	if err != nil || prev != "blob/gomod-222" {
		t.Errorf("SaveCache previous = %q, %v; want blob/gomod-222", prev, err)
	}
}
