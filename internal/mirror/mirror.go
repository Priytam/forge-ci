// Package mirror keeps a blobless partial git mirror of a repo on a runner's
// disk and computes diffs from it (code review, batch 7). Forge never asks a
// host's API for a diff: the mirror fetches commits and trees only when a
// diff names one it lacks, blobs arrive lazily for just the files the diff
// touches, and the same code works for every host git can reach.
//
// The credential is set on the mirror's remote only while a task runs and
// removed afterwards, so it is never left on disk; it is redacted from every
// error.
package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const (
	gitTimeout = 5 * time.Minute
	// MaxFilePatch: a single file's patch beyond this is dropped (too_large);
	// the file is still listed with its counts.
	MaxFilePatch = 2 << 20
	// MaxTotalPatch: patches beyond this total are dropped (truncated).
	MaxTotalPatch = 40 << 20
)

// Supported reports whether this machine's git can keep a partial mirror
// (git 2.27+: --filter on fetch with promisor remotes).
func Supported() bool {
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		return false
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 2 || (major == 2 && minor >= 27)
}

// Root is where mirrors live: RUNNER_MIRROR_DIR, else the user cache dir.
func Root() string {
	if d := os.Getenv("RUNNER_MIRROR_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "forge-runner", "mirrors")
	}
	return filepath.Join(os.TempDir(), "forge-runner-mirrors")
}

var locks sync.Map // dir → *sync.Mutex: one task at a time per mirror

// Mirror is one repo's mirror directory.
type Mirror struct {
	Dir    string
	secret []string
}

// Open returns repo's mirror under root (created on first use).
func Open(root, repo string) *Mirror {
	sum := sha256.Sum256([]byte(repo))
	return &Mirror{Dir: filepath.Join(root, hex.EncodeToString(sum[:8])+".git")}
}

func (m *Mirror) git(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = m.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		for _, s := range m.secret {
			if s != "" {
				msg = strings.ReplaceAll(msg, s, "***")
			}
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// stripUser drops credentials from a clone URL (what is left on disk).
func stripUser(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func (m *Mirror) ensure(ctx context.Context, cloneURL string) error {
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--bare", "-q"},
		{"remote", "add", "origin", stripUser(cloneURL)},
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
		{"config", "remote.origin.tagOpt", "--no-tags"},
	} {
		if _, err := m.git(ctx, args...); err != nil {
			_ = os.RemoveAll(m.Dir)
			return err
		}
	}
	return nil
}

// has reports whether the mirror holds commit sha, strictly locally: in a
// partial clone any missing-object lookup would otherwise fetch the object
// from the host one at a time, instead of the one batched fetch below.
func (m *Mirror) has(ctx context.Context, sha string) bool {
	cmd := exec.CommandContext(ctx, "git", "-c", "remote.origin.promisor=false", "cat-file", "-e", sha+"^{commit}")
	cmd.Dir = m.Dir
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	return cmd.Run() == nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Diff computes t on the mirror, fetching first only when a commit is
// missing. redact lists values (the token) that must never appear in errors.
func (m *Mirror) Diff(ctx context.Context, cloneURL string, redact []string, t proto.DiffTask) (*proto.DiffResult, error) {
	if !shaRe.MatchString(t.From) || !shaRe.MatchString(t.To) {
		return nil, errors.New("diff endpoints must be full commit shas")
	}
	start := time.Now()
	mu, _ := locks.LoadOrStore(m.Dir, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	m.secret = redact
	if err := m.ensure(ctx, cloneURL); err != nil {
		return nil, err
	}
	// The remote carries the credential only while this task runs (the fetch
	// and the lazy blob fetches the diff makes), then goes back to bare.
	if _, err := m.git(ctx, "remote", "set-url", "origin", cloneURL); err != nil {
		return nil, err
	}
	defer m.git(context.WithoutCancel(ctx), "remote", "set-url", "origin", stripUser(cloneURL)) //nolint:errcheck
	res := &proto.DiffResult{}
	if !m.has(ctx, t.From) || !m.has(ctx, t.To) {
		res.Fetched = true
		var refErr error
		if len(t.FetchRefs) > 0 {
			// A ref may be gone (a deleted branch) while its commits are not:
			// carry on to fetching by sha before giving up.
			args := append([]string{"fetch", "-q", "--filter=blob:none", "--no-tags", "origin"}, t.FetchRefs...)
			_, refErr = m.git(ctx, args...)
		}
		for _, sha := range []string{t.From, t.To} {
			if m.has(ctx, sha) {
				continue
			}
			// Hosts that allow it serve a reachable commit by sha.
			if _, err := m.git(ctx, "fetch", "-q", "--filter=blob:none", "--no-tags", "origin", sha); err != nil || !m.has(ctx, sha) {
				if refErr != nil {
					return nil, fmt.Errorf("commit %s is not on the host (%v)", sha[:12], refErr)
				}
				return nil, fmt.Errorf("commit %s is not on the host (force-pushed away?)", sha[:12])
			}
		}
	}
	from := t.From
	if t.Mode != "range" {
		out, err := m.git(ctx, "merge-base", t.From, t.To)
		if err != nil {
			return nil, fmt.Errorf("no common history between %s and %s", t.From[:12], t.To[:12])
		}
		from = strings.TrimSpace(string(out))
	}
	res.MergeBase = from
	files, err := m.diffFiles(ctx, from, t.To)
	if err != nil {
		return nil, err
	}
	res.Files, res.Truncated = files, capPatches(files)
	res.ComputeMS = int(time.Since(start).Milliseconds())
	return res, nil
}

// diffFiles lists every changed file with its status, counts and patch. Three
// git calls with the same options list the files in the same order: --raw
// (status and exact paths), --numstat (counts), and the patch.
func (m *Mirror) diffFiles(ctx context.Context, from, to string) ([]proto.DiffFile, error) {
	base := []string{"diff", "--no-color", "--no-ext-diff", "-M"}
	raw, err := m.git(ctx, append(base, "--raw", "-z", "--no-abbrev", from, to)...)
	if err != nil {
		return nil, err
	}
	files, err := parseRaw(raw)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return []proto.DiffFile{}, nil
	}
	num, err := m.git(ctx, append(base, "--numstat", "-z", from, to)...)
	if err != nil {
		return nil, err
	}
	if err := applyNumstat(files, num); err != nil {
		return nil, err
	}
	patch, err := m.git(ctx, append(base, "--full-index", "-U3", from, to)...)
	if err != nil {
		return nil, err
	}
	parts := splitPatch(patch)
	if len(parts) != len(files) {
		return nil, fmt.Errorf("diff: %d patches for %d files", len(parts), len(files))
	}
	for i := range files {
		p := parts[i]
		if files[i].Binary || bytes.Contains(firstLines(p, 6), []byte("Binary files ")) ||
			bytes.Contains(firstLines(p, 6), []byte("GIT binary patch")) {
			files[i].Binary = true
			continue
		}
		if len(p) > MaxFilePatch {
			files[i].TooLarge = true
			continue
		}
		files[i].Patch = string(p)
	}
	return files, nil
}

func firstLines(b []byte, n int) []byte {
	end := 0
	for i := 0; i < n && end < len(b); i++ {
		j := bytes.IndexByte(b[end:], '\n')
		if j < 0 {
			return b
		}
		end += j + 1
	}
	return b[:end]
}

var statusWords = map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'R': "renamed", 'C': "copied", 'T': "type_changed"}

// parseRaw reads `git diff --raw -z`: ":m1 m2 sha1 sha2 S\0path\0" (two paths
// for renames and copies).
func parseRaw(b []byte) ([]proto.DiffFile, error) {
	fields := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	var out []proto.DiffFile
	for i := 0; i < len(fields); {
		meta := fields[i]
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("diff --raw: unexpected %q", meta)
		}
		parts := strings.Fields(meta)
		if len(parts) < 5 || len(parts[4]) == 0 || i+1 >= len(fields) {
			return nil, fmt.Errorf("diff --raw: short record %q", meta)
		}
		st := parts[4][0]
		f := proto.DiffFile{Status: statusWords[st]}
		if f.Status == "" {
			f.Status = "modified"
		}
		if st == 'R' || st == 'C' {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("diff --raw: short rename record")
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 3
		} else {
			f.Path = fields[i+1]
			i += 2
		}
		f.Generated = Generated(f.Path)
		out = append(out, f)
	}
	return out, nil
}

// applyNumstat fills counts from `git diff --numstat -z` ("a\td\tpath\0", or
// "a\td\t\0old\0new\0" for renames; "-\t-" for binary).
func applyNumstat(files []proto.DiffFile, b []byte) error {
	fields := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	n := 0
	for i := 0; i < len(fields) && n < len(files); n++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("diff --numstat: unexpected %q", fields[i])
		}
		if parts[0] == "-" {
			files[n].Binary = true
		} else {
			files[n].Additions, _ = strconv.Atoi(parts[0])
			files[n].Deletions, _ = strconv.Atoi(parts[1])
		}
		if parts[2] == "" {
			i += 3 // rename: old and new path follow
		} else {
			i++
		}
	}
	if n != len(files) {
		return fmt.Errorf("diff --numstat: %d entries for %d files", n, len(files))
	}
	return nil
}

// splitPatch cuts a patch into one part per file at each "diff --git" line.
func splitPatch(p []byte) [][]byte {
	var parts [][]byte
	start := -1
	for i := 0; i < len(p); {
		if bytes.HasPrefix(p[i:], []byte("diff --git ")) {
			if start >= 0 {
				parts = append(parts, p[start:i])
			}
			start = i
		}
		j := bytes.IndexByte(p[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
	}
	if start >= 0 {
		parts = append(parts, p[start:])
	}
	return parts
}

// capPatches drops patches once their total passes MaxTotalPatch. It
// reports whether anything was dropped.
func capPatches(files []proto.DiffFile) bool {
	total, cut := 0, false
	for i := range files {
		total += len(files[i].Patch)
		if total > MaxTotalPatch {
			files[i].Patch, cut = "", true
		}
	}
	return cut
}

var generatedSuffixes = []string{
	".lock", "package-lock.json", "pnpm-lock.yaml", "go.sum", ".min.js", ".min.css", ".map",
	".pb.go", "_pb2.py", ".pb.ts", ".snap",
}

var generatedDirs = []string{"vendor/", "node_modules/", "dist/", "third_party/", "generated/", "__generated__/"}

// Generated is a heuristic for files nobody reviews line by line (lock files,
// vendored code, build output); the UI collapses them.
func Generated(path string) bool {
	lower := strings.ToLower(path)
	for _, s := range generatedSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	for _, d := range generatedDirs {
		if strings.HasPrefix(lower, d) || strings.Contains(lower, "/"+d) {
			return true
		}
	}
	return false
}
