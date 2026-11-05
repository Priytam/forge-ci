package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// bitbucketProvider: Bitbucket Cloud (API base BITBUCKET_API_BASE).
type bitbucketProvider struct{}

func init() { Register(bitbucketProvider{}) }

func (bitbucketProvider) Name() string { return "bitbucket" }

func (bitbucketProvider) FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	owner, name, ok := splitRepo(repo.Repo)
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	u := fmt.Sprintf("%s/2.0/repositories/%s/%s/src/%s/%s", f.bitbucketBase, owner, name, url.PathEscape(sha), pathEscape(path))
	return f.getRaw(ctx, u, "*/*", bearer(repo.Token), path)
}

func (bitbucketProvider) ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	return f.bitbucketChanged(ctx, CompareRequest{Repo: repo.Repo, Base: base, Head: head, Token: repo.Token})
}

func (bitbucketProvider) StatusRequest(p *Poster, req PostRequest) (*StatusCall, error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return nil, fmt.Errorf("invalid repo %q (want owner/name)", req.Repo)
	}
	state, desc := MapBitbucketState(req.Status)
	body, _ := json.Marshal(map[string]string{
		"key": StatusContext, "state": state, "url": req.TargetURL, "name": "Forge CI", "description": desc,
	})
	return &StatusCall{
		URL:     fmt.Sprintf("%s/2.0/repositories/%s/%s/commit/%s/statuses/build", p.bitbucketBase, owner, name, req.SHA),
		Body:    body,
		Headers: bearer(req.Token),
	}, nil
}

func (bitbucketProvider) FileURL(repo RepoRef, sha, path string) (string, bool) {
	host := webHost(repo.CloneURL, "bitbucket.org")
	return fmt.Sprintf("https://%s/%s/src/%s/%s", host, projectPath(repo.Repo), url.PathEscape(sha), pathEscape(path)), true
}

func (bitbucketProvider) CloneUser(token string) *url.Userinfo {
	return url.UserPassword("x-token-auth", token)
}
