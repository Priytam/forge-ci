package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Merges (batch 7, phase F): a Forge-native change is merged by a runner,
// in the mirror it already keeps, and so is a merge queue's test commit.
// `git merge-tree --write-tree` merges without a work tree, fetching only
// the blobs of files both sides touched; the result is pushed with a lease,
// so a branch that moved meanwhile is never overwritten.

// MergeSupported reports whether this machine's git can merge in a bare
// mirror (git 2.38+: merge-tree --write-tree).
func MergeSupported() bool {
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
	return major > 2 || (major == 2 && minor >= 38)
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func validBranch(b string) bool {
	return branchRe.MatchString(b) && !strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "/") &&
		!strings.Contains(b, "..") && !strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".lock")
}

// Merge does t: fetch the branches, merge t.Heads onto the target's tip, push.
func (m *Mirror) Merge(ctx context.Context, cloneURL string, redact []string, t proto.MergeTask) (*proto.MergeResult, error) {
	if !validBranch(t.Target) || (t.Source != "" && !validBranch(t.Source)) ||
		!strings.HasPrefix(t.PushRef, "refs/heads/") || !validBranch(strings.TrimPrefix(t.PushRef, "refs/heads/")) {
		return nil, errors.New("merge: invalid branch name")
	}
	if len(t.Heads) == 0 {
		return nil, errors.New("merge: nothing to merge")
	}
	for _, h := range append([]string{t.ExpectedTarget, t.ExpectedSource}, t.Heads...) {
		if h != "" && !shaRe.MatchString(h) {
			return nil, errors.New("merge: commits must be full shas")
		}
	}
	done, err := m.begin(ctx, cloneURL, redact)
	if err != nil {
		return nil, err
	}
	defer done()

	// Fetch the tips into private refs (never the host's names, which a
	// later fetch could move under us).
	args := []string{"fetch", "-q", "--filter=blob:none", "--no-tags", "origin", "+refs/heads/" + t.Target + ":refs/forge/target"}
	if t.Source != "" {
		args = append(args, "+refs/heads/"+t.Source+":refs/forge/source")
	}
	if _, err := m.git(ctx, args...); err != nil {
		return nil, err
	}
	base, err := m.revParse(ctx, "refs/forge/target")
	if err != nil {
		return nil, err
	}
	res := &proto.MergeResult{Base: base}
	if t.ExpectedTarget != "" && base != t.ExpectedTarget {
		res.Moved = t.Target + " has moved since the merge was tested"
		return res, nil
	}
	if t.Source != "" && t.ExpectedSource != "" {
		src, err := m.revParse(ctx, "refs/forge/source")
		if err != nil {
			return nil, err
		}
		if src != t.ExpectedSource {
			res.Moved = t.Source + " has new commits"
			return res, nil
		}
	}
	missing := false
	for _, h := range t.Heads {
		missing = missing || !m.has(ctx, h)
	}
	if missing && len(t.FetchRefs) > 0 {
		_, _ = m.git(ctx, append([]string{"fetch", "-q", "--filter=blob:none", "--no-tags", "origin"}, t.FetchRefs...)...)
	}
	for _, h := range t.Heads {
		if !m.has(ctx, h) {
			if _, err := m.git(ctx, "fetch", "-q", "--filter=blob:none", "--no-tags", "origin", h); err != nil || !m.has(ctx, h) {
				return nil, fmt.Errorf("commit %s is not on the host", h[:12])
			}
		}
	}

	cur := base
	for _, h := range t.Heads {
		next, conflicts, notFF, err := m.mergeOne(ctx, cur, h, t)
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 || notFF {
			res.Conflicts, res.NotFF = conflicts, notFF
			return res, nil
		}
		cur = next
	}
	push := []string{"push", "-q", "--no-verify", "origin"}
	if t.Force {
		push = append(push, "--force")
	} else {
		push = append(push, "--force-with-lease="+t.PushRef+":"+base)
	}
	if _, err := m.git(ctx, append(push, cur+":"+t.PushRef)...); err != nil {
		if strings.Contains(err.Error(), "stale info") || strings.Contains(err.Error(), "rejected") {
			res.Moved = strings.TrimPrefix(t.PushRef, "refs/heads/") + " moved while merging"
			return res, nil
		}
		return nil, err
	}
	res.SHA = cur
	return res, nil
}

// mergeOne merges head onto cur by t.Method and returns the new commit, or
// the conflicting paths, or notFF when a fast-forward is impossible.
func (m *Mirror) mergeOne(ctx context.Context, cur, head string, t proto.MergeTask) (string, []string, bool, error) {
	if t.Method == "fast_forward" {
		if _, err := m.git(ctx, "merge-base", "--is-ancestor", cur, head); err != nil {
			return "", nil, true, nil
		}
		return head, nil, false, nil
	}
	out, err := m.git(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", cur, head)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		// Exit 1 with a tree on the first line is a conflict; the rest are
		// the conflicted paths.
		if len(lines) > 1 && shaRe.MatchString(lines[0]) {
			return "", lines[1:], false, nil
		}
		return "", nil, false, err
	}
	if len(lines) == 0 || !shaRe.MatchString(lines[0]) {
		return "", nil, false, fmt.Errorf("merge-tree: unexpected output %q", string(out))
	}
	parents := []string{"-p", cur}
	if t.Method != "squash" {
		parents = append(parents, "-p", head)
	}
	return m.commitTree(ctx, lines[0], parents, t)
}

func (m *Mirror) commitTree(ctx context.Context, tree string, parents []string, t proto.MergeTask) (string, []string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	msg := t.Message
	if strings.TrimSpace(msg) == "" {
		msg = "Merge"
	}
	name, email := t.AuthorName, t.AuthorEmail
	if name == "" {
		name = "Forge"
	}
	if email == "" {
		email = "forge@localhost"
	}
	cmd := exec.CommandContext(ctx, "git", append(append([]string{"commit-tree", tree}, parents...), "-m", msg)...)
	cmd.Dir = m.Dir
	now := strconv.FormatInt(time.Now().Unix(), 10) + " +0000"
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME=Forge", "GIT_COMMITTER_EMAIL=forge@localhost", "GIT_AUTHOR_DATE="+now, "GIT_COMMITTER_DATE="+now)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, false, fmt.Errorf("git commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out)), nil, false, nil
}

func (m *Mirror) revParse(ctx context.Context, ref string) (string, error) {
	out, err := m.git(ctx, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
