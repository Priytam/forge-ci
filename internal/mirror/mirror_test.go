package mirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// host is a git server on disk that serves partial clones (as GitHub does).
type host struct {
	t          *testing.T
	bare, work string
}

func newHost(t *testing.T) *host {
	t.Helper()
	if !Supported() {
		t.Skip("git 2.27+ not installed")
	}
	dir := t.TempDir()
	h := &host{t: t, bare: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work")}
	h.git(dir, "init", "-q", "--bare", "-b", "main", h.bare)
	h.git(h.bare, "config", "uploadpack.allowFilter", "true")
	h.git(h.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	h.git(dir, "clone", "-q", h.bare, h.work)
	return h
}

func (h *host) git(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *host) write(path, content string) {
	h.t.Helper()
	full := filepath.Join(h.work, path)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) commit(msg string) string {
	h.git(h.work, "add", "-A")
	h.git(h.work, "commit", "-q", "-m", msg)
	return h.git(h.work, "rev-parse", "HEAD")
}

func (h *host) url() string { return "file://" + h.bare }

func byPath(files []proto.DiffFile) map[string]proto.DiffFile {
	out := map[string]proto.DiffFile{}
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

func TestMirrorDiffFromMergeBase(t *testing.T) {
	h := newHost(t)
	h.write("README.md", "hello\n")
	h.write("old name.txt", strings.Repeat("same line\n", 20))
	h.write("gone.txt", "bye\n")
	h.write("big/untouched.bin", strings.Repeat("x", 200000))
	h.write("logo.png", "\x89PNG\x00\x01\x02")
	h.commit("base")
	h.git(h.work, "push", "-q", "origin", "main")
	h.git(h.work, "checkout", "-q", "-b", "feature")
	h.write("README.md", "hello\nworld\n")
	h.git(h.work, "mv", "old name.txt", "new name.txt")
	h.git(h.work, "rm", "-q", "gone.txt")
	h.write("logo.png", "\x89PNG\x00\x09\x09\x09")
	h.write("package-lock.json", "{}\n")
	head := h.commit("feature work")
	h.git(h.work, "push", "-q", "origin", "feature")
	h.git(h.work, "checkout", "-q", "main")
	h.write("main-only.txt", "moved on\n")
	target := h.commit("main moves")
	h.git(h.work, "push", "-q", "origin", "main")

	root := t.TempDir()
	m := Open(root, "acme/shop")
	task := proto.DiffTask{From: target, To: head, Mode: "change",
		FetchRefs: []string{"+refs/heads/main:refs/heads/main", "+refs/heads/feature:refs/heads/feature"}}
	res, err := m.Diff(context.Background(), h.url(), nil, task)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Fetched || res.MergeBase == target || res.MergeBase == "" {
		t.Fatalf("fetched=%v merge base=%s target=%s", res.Fetched, res.MergeBase, target)
	}
	files := byPath(res.Files)
	if _, ok := files["main-only.txt"]; ok {
		t.Error("a change on the target branch leaked into the pull request's diff")
	}
	if f := files["README.md"]; f.Status != "modified" || f.Additions != 1 || f.Deletions != 0 ||
		!strings.Contains(f.Patch, "+world") || !strings.HasPrefix(f.Patch, "diff --git ") {
		t.Errorf("README = %+v", f)
	}
	if f := files["new name.txt"]; f.Status != "renamed" || f.OldPath != "old name.txt" {
		t.Errorf("rename = %+v", f)
	}
	if f := files["gone.txt"]; f.Status != "deleted" || f.Deletions != 1 {
		t.Errorf("delete = %+v", f)
	}
	if f := files["logo.png"]; !f.Binary || f.Patch != "" {
		t.Errorf("binary = %+v", f)
	}
	if f := files["package-lock.json"]; f.Status != "added" || !f.Generated {
		t.Errorf("generated = %+v", f)
	}
	if len(res.Files) != 5 {
		t.Errorf("files = %d, want 5", len(res.Files))
	}

	// Blobless: a file the diff never touched was never downloaded.
	blob := h.git(h.work, "rev-parse", target+":big/untouched.bin")
	local := exec.Command("git", "-c", "remote.origin.promisor=false", "cat-file", "-e", blob)
	local.Dir, local.Env = m.Dir, append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if local.Run() == nil {
		t.Error("the mirror holds a blob no diff needed: it is not a blobless mirror")
	}
	// The credential-bearing URL was put back to the bare one.
	if got, _ := m.git(context.Background(), "config", "remote.origin.url"); strings.TrimSpace(string(got)) != h.url() {
		t.Errorf("remote url left as %q", got)
	}

	// The same diff again: everything is local, no fetch.
	again, err := m.Diff(context.Background(), h.url(), nil, task)
	if err != nil || again.Fetched || len(again.Files) != 5 {
		t.Errorf("second diff = %+v %v", again, err)
	}

	// Revision to revision: exactly the commits' difference.
	h.git(h.work, "checkout", "-q", "feature")
	h.write("README.md", "hello\nworld\nagain\n")
	head2 := h.commit("second push")
	h.git(h.work, "push", "-q", "origin", "feature")
	rng, err := m.Diff(context.Background(), h.url(), nil, proto.DiffTask{From: head, To: head2, Mode: "range",
		FetchRefs: []string{"+refs/heads/feature:refs/heads/feature"}})
	if err != nil || !rng.Fetched || rng.MergeBase != head || len(rng.Files) != 1 || rng.Files[0].Additions != 1 {
		t.Errorf("range = %+v %v", rng, err)
	}
}

func TestMirrorMissingCommitAndBadInput(t *testing.T) {
	h := newHost(t)
	h.write("a", "1\n")
	sha := h.commit("one")
	h.git(h.work, "push", "-q", "origin", "main")
	m := Open(t.TempDir(), "acme/x")
	missing := strings.Repeat("ab", 20)
	_, err := m.Diff(context.Background(), h.url(), []string{"s3cret"}, proto.DiffTask{From: sha, To: missing, Mode: "change",
		FetchRefs: []string{"+refs/heads/main:refs/heads/main"}})
	if err == nil || !strings.Contains(err.Error(), "not on the host") {
		t.Errorf("missing commit err = %v", err)
	}
	if _, err := m.Diff(context.Background(), h.url(), nil, proto.DiffTask{From: "main", To: sha}); err == nil {
		t.Error("a branch name was accepted as a diff endpoint")
	}
}

func TestParsers(t *testing.T) {
	raw := ":100644 100644 aaa bbb M\x00a b.go\x00:100644 100644 ccc ddd R087\x00old.go\x00new.go\x00:000000 100644 0 eee A\x00vendor/x.go\x00"
	files, err := parseRaw([]byte(raw))
	if err != nil || len(files) != 3 || files[0].Path != "a b.go" || files[1].OldPath != "old.go" ||
		files[1].Status != "renamed" || !files[2].Generated {
		t.Fatalf("parseRaw = %+v %v", files, err)
	}
	if err := applyNumstat(files, []byte("3\t1\ta b.go\x002\t2\t\x00old.go\x00new.go\x00-\t-\tvendor/x.go\x00")); err != nil {
		t.Fatal(err)
	}
	if files[0].Additions != 3 || files[1].Deletions != 2 || !files[2].Binary {
		t.Errorf("numstat = %+v", files)
	}
	parts := splitPatch([]byte("diff --git a/x b/x\n+1\ndiff --git a/y b/y\n-2\n"))
	if len(parts) != 2 || string(parts[1]) != "diff --git a/y b/y\n-2\n" {
		t.Errorf("split = %q", parts)
	}
	big := []proto.DiffFile{{Patch: strings.Repeat("x", MaxTotalPatch)}, {Patch: "y"}}
	if !capPatches(big) || big[1].Patch != "" || big[0].Patch == "" {
		t.Error("total cap not applied")
	}
	if stripUser("https://x-access-token:ghs_1@github.com/a/b.git") != "https://github.com/a/b.git" {
		t.Error("stripUser kept the credential")
	}
}
