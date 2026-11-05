package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// giteaProvider: Gitea and its forks (Forgejo, Codeberg), API v1. The API
// base is GITEA_API_BASE when set, else derived from the clone URL's host.
type giteaProvider struct{}

func init() { Register(giteaProvider{}) }

func (giteaProvider) Name() string { return "gitea" }

func giteaAPI(cloneURL string) string {
	if v := strings.TrimSpace(os.Getenv("GITEA_API_BASE")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return hostBase(cloneURL, "https://gitea.com") + "/api/v1"
}

func giteaAuth(token string) map[string]string {
	if token == "" {
		return nil
	}
	return map[string]string{"Authorization": "token " + token}
}

func (giteaProvider) FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	owner, name, ok := splitRepo(projectPath(repo.Repo))
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q (want owner/name)", repo.Repo)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/raw/%s?ref=%s", giteaAPI(repo.CloneURL), owner, name, pathEscape(path), url.QueryEscape(sha))
	return f.getRaw(ctx, u, "", giteaAuth(repo.Token), path)
}

// ChangedFiles uses the compare API, which lists files on Gitea ≥ 1.22. An
// older server answers without "files"; that is reported incomplete so the
// caller falls back to a git diff.
func (giteaProvider) ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	owner, name, ok := splitRepo(projectPath(repo.Repo))
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q", repo.Repo)
	}
	var out struct {
		Files *[]struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", giteaAPI(repo.CloneURL), owner, name,
		url.PathEscape(base), url.PathEscape(head))
	if err := f.getJSONHeaders(ctx, u, giteaAuth(repo.Token), &out); err != nil {
		return nil, false, err
	}
	if out.Files == nil {
		return nil, false, nil
	}
	var files []string
	for _, fl := range *out.Files {
		files = append(files, fl.Filename)
		if fl.PreviousFilename != "" {
			files = append(files, fl.PreviousFilename)
		}
	}
	return files, len(files) <= maxChangedFiles, nil
}

// MapGiteaState maps a Forge phase to a Gitea commit status.
func MapGiteaState(status string) (state, description string) {
	switch status {
	case "success":
		return "success", "Pipeline passed"
	case "failed":
		return "failure", "Pipeline failed"
	case "canceled":
		return "error", "Pipeline canceled"
	case "blocked":
		return "pending", "Waiting for approval"
	case "running":
		return "pending", "Pipeline running"
	default:
		return "pending", "Pipeline queued"
	}
}

func (giteaProvider) StatusRequest(_ *Poster, req PostRequest) (*StatusCall, error) {
	owner, name, ok := splitRepo(projectPath(req.Repo))
	if !ok {
		return nil, fmt.Errorf("invalid repo %q (want owner/name)", req.Repo)
	}
	state, desc := MapGiteaState(req.Status)
	body, _ := json.Marshal(map[string]string{
		"state": state, "context": StatusContext, "target_url": req.TargetURL, "description": desc,
	})
	return &StatusCall{
		URL:     fmt.Sprintf("%s/repos/%s/%s/statuses/%s", giteaAPI(req.CloneURL), owner, name, url.PathEscape(req.SHA)),
		Body:    body,
		Headers: giteaAuth(req.Token),
	}, nil
}

func (giteaProvider) FileURL(repo RepoRef, sha, path string) (string, bool) {
	base := hostBase(repo.CloneURL, "")
	if base == "" {
		return "", false
	}
	return fmt.Sprintf("%s/%s/src/commit/%s/%s", base, projectPath(repo.Repo), url.PathEscape(sha), pathEscape(path)), true
}

// CloneUser: Gitea accepts the token as the username.
func (giteaProvider) CloneUser(token string) *url.Userinfo {
	return url.UserPassword(token, "x-oauth-basic")
}
