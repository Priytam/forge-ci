package mirror

import (
	"context"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// branch commits content on a new branch off main and pushes it.
func (h *host) branch(name string, files map[string]string) string {
	h.t.Helper()
	h.git(h.work, "checkout", "-q", "-B", name, "origin/main")
	for p, c := range files {
		h.write(p, c)
	}
	sha := h.commit("work on " + name)
	h.git(h.work, "push", "-q", "-f", "origin", name)
	h.git(h.work, "checkout", "-q", "main")
	return sha
}

func (h *host) tip(branch string) string { return h.git(h.bare, "rev-parse", "refs/heads/"+branch) }

func mergeHost(t *testing.T) *host {
	t.Helper()
	if !MergeSupported() {
		t.Skip("git 2.38+ not installed")
	}
	h := newHost(t)
	h.write("app.go", "package app\n")
	h.write("README.md", "hello\n")
	h.commit("base")
	h.git(h.work, "push", "-q", "origin", "main")
	return h
}

func TestMergeMethods(t *testing.T) {
	ctx := context.Background()
	for _, method := range []string{"merge", "squash", "fast_forward"} {
		t.Run(method, func(t *testing.T) {
			h := mergeHost(t)
			head := h.branch("feature", map[string]string{"feature.go": "package app // new\n"})
			base := h.tip("main")
			m := Open(t.TempDir(), "acme/shop")
			res, err := m.Merge(ctx, h.url(), nil, proto.MergeTask{Target: "main", Source: "feature", ExpectedSource: head,
				Heads: []string{head}, Method: method, Message: "Merge change #1", AuthorName: "Ashwin",
				AuthorEmail: "ashwin@acme.io", PushRef: "refs/heads/main"})
			if err != nil || res.SHA == "" || res.Base != base || res.Moved != "" || len(res.Conflicts) > 0 {
				t.Fatalf("merge = %+v, %v", res, err)
			}
			if got := h.tip("main"); got != res.SHA {
				t.Fatalf("main = %s, want the merge %s", got, res.SHA)
			}
			parents := strings.Fields(h.git(h.bare, "log", "-1", "--format=%P", res.SHA))
			want := map[string][]string{"merge": {base, head}, "squash": {base}, "fast_forward": {base}}[method]
			if method == "fast_forward" {
				if res.SHA != head {
					t.Fatalf("fast-forward made %s, want the head %s", res.SHA, head)
				}
			} else if strings.Join(parents, " ") != strings.Join(want, " ") {
				t.Fatalf("parents = %v, want %v", parents, want)
			}
			if method != "fast_forward" {
				if a := h.git(h.bare, "log", "-1", "--format=%an <%ae>|%s", res.SHA); a != "Ashwin <ashwin@acme.io>|Merge change #1" {
					t.Errorf("commit = %q", a)
				}
			}
			if !strings.Contains(h.git(h.bare, "show", res.SHA+":feature.go"), "new") {
				t.Error("the merged tree lacks the change")
			}
		})
	}
}

func TestMergeRefusesWhatWouldBeWrong(t *testing.T) {
	ctx := context.Background()
	h := mergeHost(t)
	m := Open(t.TempDir(), "acme/shop")
	head := h.branch("feature", map[string]string{"app.go": "package app // feature\n"})
	task := proto.MergeTask{Target: "main", Source: "feature", ExpectedSource: head, Heads: []string{head},
		Method: "merge", PushRef: "refs/heads/main"}

	// The source has new commits: nothing is pushed.
	newer := h.branch("feature", map[string]string{"app.go": "package app // feature v2\n"})
	before := h.tip("main")
	if res, err := m.Merge(ctx, h.url(), nil, task); err != nil || res.Moved == "" || h.tip("main") != before {
		t.Fatalf("moved source: %+v, %v", res, err)
	}

	// The target moved since the queue tested it.
	task.ExpectedSource, task.Heads = newer, []string{newer}
	task.ExpectedTarget = strings.Repeat("a", 40)
	if res, err := m.Merge(ctx, h.url(), nil, task); err != nil || !strings.Contains(res.Moved, "main") {
		t.Fatalf("moved target: %+v, %v", res, err)
	}
	task.ExpectedTarget = ""

	// A conflict names the file and pushes nothing.
	h.write("app.go", "package app // main\n")
	h.commit("main edits app.go")
	h.git(h.work, "push", "-q", "origin", "main")
	before = h.tip("main")
	res, err := m.Merge(ctx, h.url(), nil, task)
	if err != nil || len(res.Conflicts) != 1 || res.Conflicts[0] != "app.go" || res.SHA != "" || h.tip("main") != before {
		t.Fatalf("conflict: %+v, %v", res, err)
	}

	// Not a fast-forward any more.
	task.Method = "fast_forward"
	if res, err := m.Merge(ctx, h.url(), nil, task); err != nil || !res.NotFF {
		t.Fatalf("fast-forward over a moved main: %+v, %v", res, err)
	}

	// Bad input never reaches git.
	for _, bad := range []proto.MergeTask{
		{Target: "--upload-pack=x", Heads: []string{newer}, PushRef: "refs/heads/main"},
		{Target: "main", Heads: []string{"HEAD"}, PushRef: "refs/heads/main"},
		{Target: "main", Heads: []string{newer}, PushRef: "refs/tags/v1"},
	} {
		if _, err := m.Merge(ctx, h.url(), nil, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// TestQueueBuild: a merge queue test commit stacks heads onto main and is
// force-pushed to the queue branch, leaving main alone.
func TestQueueBuild(t *testing.T) {
	ctx := context.Background()
	h := mergeHost(t)
	a := h.branch("a", map[string]string{"a.go": "package app // a\n"})
	b := h.branch("b", map[string]string{"b.go": "package app // b\n"})
	main := h.tip("main")
	m := Open(t.TempDir(), "acme/shop")
	for i := 0; i < 2; i++ { // the second run overwrites the first
		res, err := m.Merge(ctx, h.url(), nil, proto.MergeTask{Target: "main", Heads: []string{a, b}, Method: "merge",
			Message: "Merge queue", PushRef: "refs/heads/forge-queue/main", Force: true})
		if err != nil || res.SHA == "" || res.Base != main {
			t.Fatalf("queue build = %+v, %v", res, err)
		}
		if h.tip("forge-queue/main") != res.SHA || h.tip("main") != main {
			t.Fatal("the test commit must land on the queue branch only")
		}
		files := h.git(h.bare, "ls-tree", "--name-only", res.SHA)
		if !strings.Contains(files, "a.go") || !strings.Contains(files, "b.go") {
			t.Fatalf("test tree = %q", files)
		}
	}
}

func TestDiffReadsCodeOwnersFromTarget(t *testing.T) {
	ctx := context.Background()
	h := mergeHost(t)
	h.write(".github/CODEOWNERS", "*.go @acme/backend\n")
	h.commit("owners")
	h.git(h.work, "push", "-q", "origin", "main")
	head := h.branch("feature", map[string]string{"x.go": "package app\n", ".github/CODEOWNERS": "* @mallory\n"})
	m := Open(t.TempDir(), "acme/shop")
	res, err := m.Diff(ctx, h.url(), nil, proto.DiffTask{From: h.tip("main"), To: head, Mode: "change"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OwnersRead || res.CodeOwnersPath != ".github/CODEOWNERS" || res.CodeOwners != "*.go @acme/backend\n" {
		t.Fatalf("owners = %v %q %q (must come from the target, not the change)", res.OwnersRead, res.CodeOwnersPath, res.CodeOwners)
	}
	res, err = m.Diff(ctx, h.url(), nil, proto.DiffTask{From: h.tip("main"), To: head, Mode: "range"})
	if err != nil || res.OwnersRead {
		t.Fatalf("range diffs read no owners: %+v %v", res.OwnersRead, err)
	}
}
