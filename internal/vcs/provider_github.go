package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// githubProvider: github.com and GitHub Enterprise (API base GITHUB_API_BASE).
type githubProvider struct{}

func init() { Register(githubProvider{}) }

func (githubProvider) Name() string { return "github" }

func (githubProvider) FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", f.githubBase, owner, name, pathEscape(path), url.QueryEscape(sha))
	body, found, ctype, err := f.getRawCT(ctx, u, "application/vnd.github.raw", bearer(repo.Token), path)
	if err != nil || !found {
		return nil, found, err
	}
	// A host that ignores the raw media type answers with the JSON envelope.
	if strings.Contains(ctype, "application/json") {
		decoded, derr := decodeGitHubContents(body)
		if derr != nil {
			return nil, false, derr
		}
		return decoded, true, nil
	}
	return body, true, nil
}

func (githubProvider) ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	return f.githubChanged(ctx, CompareRequest{Repo: repo.Repo, Base: base, Head: head, Token: repo.Token})
}

func (githubProvider) StatusRequest(p *Poster, req PostRequest) (*StatusCall, error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return nil, fmt.Errorf("invalid repo %q (want owner/name)", req.Repo)
	}
	state, desc := MapGitHubState(req.Status)
	body, _ := json.Marshal(map[string]string{
		"state": state, "target_url": req.TargetURL, "context": StatusContext, "description": desc,
	})
	return &StatusCall{
		URL:     fmt.Sprintf("%s/repos/%s/%s/statuses/%s", p.githubBase, owner, name, req.SHA),
		Body:    body,
		Headers: bearer(req.Token),
	}, nil
}

func (githubProvider) FileURL(repo RepoRef, sha, path string) (string, bool) {
	host := webHost(repo.CloneURL, "github.com")
	return fmt.Sprintf("https://%s/%s/blob/%s/%s", host, projectPath(repo.Repo), url.PathEscape(sha), pathEscape(path)), true
}

func (githubProvider) CloneUser(token string) *url.Userinfo {
	return url.UserPassword("x-access-token", token)
}
