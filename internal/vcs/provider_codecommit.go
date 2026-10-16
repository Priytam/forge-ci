package vcs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
)

// codecommitProvider: AWS CodeCommit, through the AWS SDK with the control
// plane's own IAM identity (no token). It has no commit-status API; Forge
// reports merge-request results as PR comments instead (see Poster).
type codecommitProvider struct{}

func init() { Register(codecommitProvider{}) }

func (codecommitProvider) Name() string { return "codecommit" }

// repoFor resolves the CodeCommit coordinates for a registration.
func codeCommitRepoFor(repo RepoRef) (CodeCommitRepo, error) {
	cc, ok := ParseCodeCommitURL(repo.CloneURL)
	if repo.Region != "" {
		cc.Region = repo.Region
		ok = ok || cc.Name != ""
	}
	if repo.Profile != "" {
		cc.Profile = repo.Profile
	}
	if !ok || cc.Region == "" || cc.Name == "" {
		return cc, fmt.Errorf("invalid codecommit clone URL %q (want codecommit::<region>://<repo>)", repo.CloneURL)
	}
	return cc, nil
}

func (codecommitProvider) FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	cc, err := codeCommitRepoFor(repo)
	if err != nil {
		return nil, false, err
	}
	return f.cc.getFile(ctx, cc, repo.RoleARN, sha, path)
}

func (codecommitProvider) ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	return f.codecommitChanged(ctx, CompareRequest{
		Base: base, Head: head, CloneURL: repo.CloneURL, Region: repo.Region, Profile: repo.Profile, RoleARN: repo.RoleARN,
	})
}

func (codecommitProvider) StatusRequest(*Poster, PostRequest) (*StatusCall, error) {
	return nil, ErrNoStatusAPI
}

func (codecommitProvider) FileURL(repo RepoRef, sha, path string) (string, bool) {
	region := strings.TrimSpace(repo.Region)
	if region == "" {
		if cc, ok := ParseCodeCommitURL(repo.CloneURL); ok {
			region = cc.Region
		}
	}
	if region == "" {
		return "", false
	}
	// The console's browse path separates the ref from the file path with a
	// literal "--" segment.
	return fmt.Sprintf(
		"https://%s.console.aws.amazon.com/codesuite/codecommit/repositories/%s/browse/%s/--/%s?region=%s",
		url.PathEscape(region), url.PathEscape(projectPath(repo.Repo)), url.PathEscape(sha),
		pathEscape(path), url.QueryEscape(region)), true
}

// CloneUser: CodeCommit clones are SigV4-signed by the runner, never a token.
func (codecommitProvider) CloneUser(string) *url.Userinfo { return nil }

// ResolveCodeCommitRef resolves a branch to its tip commit through the
// CodeCommit API, so the server needs no git-remote-codecommit helper. ok is
// false when ref is not a branch (e.g. a tag), letting callers fall back.
func (f *Fetcher) ResolveCodeCommitRef(ctx context.Context, repo RepoRef, ref string) (sha string, ok bool, err error) {
	cc, err := codeCommitRepoFor(repo)
	if err != nil {
		return "", false, err
	}
	cl, err := f.cc.client(ctx, cc.Region, repo.RoleARN, cc.Profile)
	if err != nil {
		return "", false, err
	}
	out, err := cl.GetBranch(ctx, &codecommit.GetBranchInput{
		RepositoryName: aws.String(cc.Name),
		BranchName:     aws.String(strings.TrimPrefix(ref, "refs/heads/")),
	})
	if err != nil {
		if codeCommitNotFound(err) || awsErrCode(err) == "BranchDoesNotExistException" {
			return "", false, nil
		}
		return "", false, fmt.Errorf("vcs: codecommit GetBranch: %s", awsErrCode(err))
	}
	if out.Branch == nil || out.Branch.CommitId == nil {
		return "", false, nil
	}
	return *out.Branch.CommitId, true, nil
}
