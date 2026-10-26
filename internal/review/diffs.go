package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Diffs are computed once by a runner from its git mirror, stored here in
// the blob store under a key made of commit shas (so the bytes can never go
// stale), and served with immutable cache headers. A page opening a diff
// reads one small manifest; big, generated and binary files are separate
// chunks fetched only when expanded.

const (
	inlinePatchMax  = 32 << 10  // per file: larger patches become a chunk
	inlineTotalMax  = 512 << 10 // per manifest
	diffRetention   = 30 * 24 * time.Hour
	expireBatchSize = 200
)

// DiffManifest is the diff a page loads first.
type DiffManifest struct {
	Version   int            `json:"version"`
	Repo      string         `json:"repo"`
	Mode      string         `json:"mode"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	MergeBase string         `json:"merge_base"`
	Additions int            `json:"additions"`
	Deletions int            `json:"deletions"`
	Truncated bool           `json:"truncated,omitempty"`
	Files     []ManifestFile `json:"files"`
}

// ManifestFile is one file: its patch inline when small, else Chunk names
// the blob (relative to the manifest) holding it.
type ManifestFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
	Generated bool   `json:"generated,omitempty"`
	TooLarge  bool   `json:"too_large,omitempty"`
	Patch     string `json:"patch,omitempty"`
	Chunk     string `json:"chunk,omitempty"`
}

// RepoKey is the repo's path segment in blob keys and URLs (a hash, so repo
// names never shape a storage path).
func RepoKey(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:8])
}

// BlobPrefix is where a diff's blobs live: a "change" diff is keyed by its
// merge base and head (so every target tip with that merge base shares it),
// a "range" diff by its two commits.
func BlobPrefix(k store.DiffKey, mergeBase string) string {
	if k.Mode == "range" {
		return fmt.Sprintf("diffs/v1/%s/range/%s..%s", RepoKey(k.Repo), k.From, k.To)
	}
	return fmt.Sprintf("diffs/v1/%s/change/%s..%s", RepoKey(k.Repo), mergeBase, k.To)
}

// BuildManifest lays a runner's result out as a manifest plus chunk bodies
// (chunk name → patch).
func BuildManifest(k store.DiffKey, res *proto.DiffResult) (*DiffManifest, map[string][]byte) {
	m := &DiffManifest{Version: 1, Repo: k.Repo, Mode: k.Mode, From: k.From, To: k.To,
		MergeBase: res.MergeBase, Truncated: res.Truncated, Files: make([]ManifestFile, 0, len(res.Files))}
	chunks := map[string][]byte{}
	inline := 0
	for i, f := range res.Files {
		mf := ManifestFile{Path: f.Path, OldPath: f.OldPath, Status: f.Status, Additions: f.Additions,
			Deletions: f.Deletions, Binary: f.Binary, Generated: f.Generated, TooLarge: f.TooLarge}
		m.Additions += f.Additions
		m.Deletions += f.Deletions
		switch {
		case f.Patch == "":
		case !f.Generated && len(f.Patch) <= inlinePatchMax && inline+len(f.Patch) <= inlineTotalMax:
			mf.Patch = f.Patch
			inline += len(f.Patch)
		default:
			mf.Chunk = fmt.Sprintf("f/%d.patch", i)
			chunks[mf.Chunk] = []byte(f.Patch)
		}
		m.Files = append(m.Files, mf)
	}
	return m, chunks
}

// StoreDiff writes a computed diff to the blob store and marks it ready.
func StoreDiff(ctx context.Context, st *store.Store, blobs blob.Store, k store.DiffKey, res *proto.DiffResult) error {
	if blobs == nil {
		return fmt.Errorf("no blob store configured")
	}
	m, chunks := BuildManifest(k, res)
	prefix := BlobPrefix(k, res.MergeBase)
	var total int64
	for name, body := range chunks {
		n, err := blobs.Put(ctx, prefix+"/"+name, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("store diff chunk: %w", err)
		}
		total += n
	}
	body, _ := json.Marshal(m)
	n, err := blobs.Put(ctx, prefix+"/manifest.json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("store diff manifest: %w", err)
	}
	return st.MarkDiffReady(ctx, k, store.DiffReady{
		MergeBase: res.MergeBase, BlobPrefix: prefix, Chunks: len(res.Files), Files: len(res.Files),
		Additions: m.Additions, Deletions: m.Deletions, Bytes: total + n, Fetched: res.Fetched, ComputeMS: res.ComputeMS,
	})
}

// ExpireDiffs deletes diffs nobody has opened for 30 days and no open
// change shows (they are recomputed if someone opens one again).
func ExpireDiffs(ctx context.Context, st *store.Store, blobs blob.Store) int {
	gone, err := st.ExpireDiffs(ctx, diffRetention, expireBatchSize)
	if err != nil {
		slog.Error("code review: expire diffs", "err", err)
		return 0
	}
	for _, d := range gone {
		if d.Shared || d.BlobPrefix == "" || blobs == nil {
			continue
		}
		_ = blobs.Delete(ctx, d.BlobPrefix+"/manifest.json")
		for i := 0; i < d.Chunks; i++ {
			_ = blobs.Delete(ctx, fmt.Sprintf("%s/f/%d.patch", d.BlobPrefix, i))
		}
	}
	if len(gone) > 0 {
		slog.Info("code review: expired diffs", "diffs", len(gone))
	}
	return len(gone)
}

// BlobKeyFromURL maps the public path below /api/v1/diff-blobs/ back to a
// blob key, accepting only what StoreDiff writes (hex segments, a manifest
// or a numbered chunk), so the route can never reach another blob.
func BlobKeyFromURL(repoKey, mode, rng, rest string) (string, bool) {
	isHex := func(s string, min, max int) bool {
		if len(s) < min || len(s) > max {
			return false
		}
		for _, c := range s {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
		return true
	}
	from, to, ok := strings.Cut(rng, "..")
	if !isHex(repoKey, 16, 16) || (mode != "change" && mode != "range") || !ok ||
		!isHex(from, 40, 64) || !isHex(to, 40, 64) {
		return "", false
	}
	if rest != "manifest.json" {
		n, ok := strings.CutPrefix(rest, "f/")
		num, ok2 := strings.CutSuffix(n, ".patch")
		if !ok || !ok2 || num == "" || len(num) > 6 || strings.Trim(num, "0123456789") != "" {
			return "", false
		}
	}
	return fmt.Sprintf("diffs/v1/%s/%s/%s/%s", repoKey, mode, rng, rest), true
}
