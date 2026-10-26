package vcs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The plain-git change source. A git server has no pull requests, so changes
// are Forge-native: Forge stores them (source and target branch) and this
// source only re-reads the two branch tips with one `git ls-remote` — the
// cheapest call git has, and no API.

func (gitProvider) Capabilities() Capabilities {
	return Capabilities{NativeChanges: true, MergeMethods: []string{}}
}

// NormalizeEvent: a plain git server sends no change webhooks.
func (gitProvider) NormalizeEvent(http.Header, []byte) ([]ChangeEvent, error) { return nil, nil }

// ListOpenChanges: the list of changes lives in Forge, not on the host.
func (gitProvider) ListOpenChanges(context.Context, *ChangeClient, RepoRef, Validators) ([]ChangeRef, Validators, bool, error) {
	return nil, Validators{}, false, ErrNoChangeAPI
}

// GetChange re-reads the source and target tips of a native change. A source
// branch that no longer exists closes the change.
func (gitProvider) GetChange(ctx context.Context, c *ChangeClient, repo RepoRef, id string, prev *Change) (*Change, error) {
	if prev == nil || prev.SourceBranch == "" || prev.TargetBranch == "" {
		return nil, ErrChangeNotFound
	}
	if err := c.guard(ctx, "git"); err != nil {
		return nil, err
	}
	tips, err := lsRemote(ctx, repo, prev.SourceBranch, prev.TargetBranch)
	if err != nil {
		c.report(ctx, CallInfo{Provider: "git", Result: "error"})
		return nil, err
	}
	ch := *prev
	ch.ID = id
	ch.Labels = append([]string{}, prev.Labels...)
	ch.Reviews = nil // reviews of a native change live in Forge
	head, ok := tips[prev.SourceBranch]
	base := tips[prev.TargetBranch]
	switch {
	case !ok && ch.State == "open":
		ch.State = "closed"
	case ok:
		ch.HeadSHA = head
	}
	if base != "" {
		ch.BaseSHA = base
	}
	result := "ok"
	if ch.HeadSHA == prev.HeadSHA && ch.BaseSHA == prev.BaseSHA && ch.State == prev.State {
		result = "not_modified"
	} else {
		ch.UpdatedAt = time.Now().UTC()
	}
	c.report(ctx, CallInfo{Provider: "git", Result: result})
	return &ch, nil
}

// lsRemote returns branch → tip sha for the named branches that exist.
func lsRemote(ctx context.Context, repo RepoRef, branches ...string) (map[string]string, error) {
	if !gitReachable(repo.CloneURL) {
		return nil, fmt.Errorf("vcs: %s has no clone URL git can fetch", repo.Repo)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	args := []string{"ls-remote", authCloneURL(repo)}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if repo.Token != "" {
			msg = strings.ReplaceAll(msg, repo.Token, "***")
		}
		return nil, fmt.Errorf("git ls-remote: %s", msg)
	}
	tips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if ok {
			tips[strings.TrimPrefix(ref, "refs/heads/")] = sha
		}
	}
	return tips, nil
}

// MirrorRefs: a native change is its two branches.
func (gitProvider) MirrorRefs(_, source, target string) []string {
	return []string{headRef(source), headRef(target)}
}
