package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// gitlabProvider: gitlab.com or a self-hosted GitLab, API v4. The API base is
// GITLAB_API_BASE when set, else derived from the clone URL's host, so a
// self-hosted instance needs no extra configuration.
type gitlabProvider struct{}

func init() { Register(gitlabProvider{}) }

func (gitlabProvider) Name() string { return "gitlab" }

func gitlabAPI(cloneURL string) string {
	if v := strings.TrimSpace(os.Getenv("GITLAB_API_BASE")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return hostBase(cloneURL, "https://gitlab.com") + "/api/v4"
}

// gitlabProject is the URL-encoded project path ("group/sub/proj" →
// "group%2Fsub%2Fproj"), which the API accepts in place of a numeric id.
func gitlabProject(repo string) string {
	return strings.ReplaceAll(url.PathEscape(projectPath(repo)), "/", "%2F")
}

func (gitlabProvider) FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	u := fmt.Sprintf("%s/projects/%s/repository/files/%s/raw?ref=%s", gitlabAPI(repo.CloneURL),
		gitlabProject(repo.Repo), strings.ReplaceAll(url.PathEscape(path), "/", "%2F"), url.QueryEscape(sha))
	return f.getRaw(ctx, u, "", bearer(repo.Token), path)
}

func (gitlabProvider) ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	var out struct {
		Diffs []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		} `json:"diffs"`
		CompareTimeout bool `json:"compare_timeout"`
	}
	u := fmt.Sprintf("%s/projects/%s/repository/compare?from=%s&to=%s&straight=false", gitlabAPI(repo.CloneURL),
		gitlabProject(repo.Repo), url.QueryEscape(base), url.QueryEscape(head))
	if err := f.getJSONHeaders(ctx, u, bearer(repo.Token), &out); err != nil {
		return nil, false, err
	}
	var files []string
	for _, d := range out.Diffs {
		files = append(files, d.NewPath)
		if d.OldPath != "" && d.OldPath != d.NewPath {
			files = append(files, d.OldPath)
		}
	}
	return files, !out.CompareTimeout && len(files) <= maxChangedFiles, nil
}

// MapGitLabState maps a Forge phase to a GitLab commit status.
func MapGitLabState(status string) (state, description string) {
	switch status {
	case "success":
		return "success", "Pipeline passed"
	case "failed":
		return "failed", "Pipeline failed"
	case "canceled":
		return "canceled", "Pipeline canceled"
	case "running":
		return "running", "Pipeline running"
	case "blocked":
		return "pending", "Waiting for approval"
	default:
		return "pending", "Pipeline queued"
	}
}

func (gitlabProvider) StatusRequest(_ *Poster, req PostRequest) (*StatusCall, error) {
	state, desc := MapGitLabState(req.Status)
	body, _ := json.Marshal(map[string]string{
		"state": state, "name": StatusContext, "target_url": req.TargetURL, "description": desc,
	})
	return &StatusCall{
		URL:     fmt.Sprintf("%s/projects/%s/statuses/%s", gitlabAPI(req.CloneURL), gitlabProject(req.Repo), url.PathEscape(req.SHA)),
		Body:    body,
		Headers: bearer(req.Token),
	}, nil
}

func (gitlabProvider) FileURL(repo RepoRef, sha, path string) (string, bool) {
	return fmt.Sprintf("%s/%s/-/blob/%s/%s", hostBase(repo.CloneURL, "https://gitlab.com"),
		projectPath(repo.Repo), url.PathEscape(sha), pathEscape(path)), true
}

// CloneUser: GitLab accepts any token type as the password for user "oauth2".
func (gitlabProvider) CloneUser(token string) *url.Userinfo {
	return url.UserPassword("oauth2", token)
}
