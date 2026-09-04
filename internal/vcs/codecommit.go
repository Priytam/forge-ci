package vcs

// AWS CodeCommit differs from the HTTP-token providers in two ways that shape
// this file:
//
//  1. There is no bearer token. Every call is SigV4-signed with an AWS identity,
//     so the "auth material" is an AWS credential chain rather than a string
//     Forge stores. Nothing secret is persisted for a CodeCommit repo.
//  2. There is no owner/name split. A CodeCommit repository name is a single
//     segment, so the splitRepo() that gates the GitHub and Bitbucket paths must
//     be bypassed — the Forge repo key IS the CodeCommit repository name.
//
// The control plane's own AWS identity (default credential chain, optionally
// assuming a role) is what reads config here. That is deliberately separate from
// the RUNNER's identity, which is keyless per-job OIDC — see internal/vcs
// codecommit_sign.go and docs/codecommit.md.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	cctypes "github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// CodeCommitRepo identifies a CodeCommit repository and the AWS context needed
// to reach it. It is derived from a registered clone URL, so the region travels
// with the registration rather than being configured twice.
type CodeCommitRepo struct {
	Region  string // e.g. ap-south-1
	Name    string // CodeCommit repository name (single segment, no owner/)
	Profile string // optional named AWS profile from a codecommit::<r>://<p>@<n> URL
}

// ParseCodeCommitURL extracts the region and repository name from either clone
// URL form CodeCommit repos are registered with:
//
//	codecommit::ap-south-1://tablespace-api           (git-remote-codecommit)
//	codecommit::ap-south-1://myprofile@tablespace-api (with a named profile)
//	https://git-codecommit.ap-south-1.amazonaws.com/v1/repos/tablespace-api
//
// ok is false for anything that is not a CodeCommit URL, which is how callers
// tell a CodeCommit registration from a plain HTTPS one.
func ParseCodeCommitURL(raw string) (CodeCommitRepo, bool) {
	raw = strings.TrimSpace(raw)

	// git-remote-codecommit form: codecommit::<region>://[<profile>@]<repo>
	if rest, found := strings.CutPrefix(raw, "codecommit::"); found {
		region, name, ok := strings.Cut(rest, "://")
		if !ok || region == "" || name == "" {
			return CodeCommitRepo{}, false
		}
		var profile string
		if p, n, hasProfile := strings.Cut(name, "@"); hasProfile {
			profile, name = p, n
		}
		if name == "" {
			return CodeCommitRepo{}, false
		}
		return CodeCommitRepo{Region: region, Name: name, Profile: profile}, true
	}

	// GRC HTTPS form: https://git-codecommit.<region>.amazonaws.com/v1/repos/<repo>
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Scheme, "http") {
		return CodeCommitRepo{}, false
	}
	host := u.Hostname()
	if !strings.HasPrefix(host, "git-codecommit.") || !strings.Contains(host, ".amazonaws.com") {
		return CodeCommitRepo{}, false
	}
	region := strings.TrimPrefix(host, "git-codecommit.")
	region = strings.TrimSuffix(region, ".amazonaws.com")
	region = strings.TrimSuffix(region, ".cn") // cn-north-1 lives on .amazonaws.com.cn
	name := strings.TrimPrefix(u.Path, "/v1/repos/")
	name = strings.TrimSuffix(strings.Trim(name, "/"), ".git")
	if region == "" || name == "" || strings.Contains(name, "/") {
		return CodeCommitRepo{}, false
	}
	return CodeCommitRepo{Region: region, Name: name}, true
}

// CodeCommitCloneURL renders the git-remote-codecommit clone URL for a repo in a
// region — the form registerRepo derives when an operator does not supply one.
func CodeCommitCloneURL(region, repo string) string {
	return "codecommit::" + region + "://" + repo
}

// CodeCommitHTTPSCloneURL renders the GRC HTTPS clone URL, which is what the
// runner signs with SigV4 when cloning without the remote helper.
func CodeCommitHTTPSCloneURL(region, repo string) string {
	return fmt.Sprintf("https://git-codecommit.%s.amazonaws.com/v1/repos/%s", region, repo)
}

// ccClients lazily builds and caches one CodeCommit API client per
// (region, role, profile). Building a client resolves the AWS credential chain,
// which can touch IMDS or STS, so it is not something to redo on every pipeline
// event. Cached clients refresh their own credentials internally.
type ccClients struct {
	mu sync.Mutex
	m  map[string]*codecommit.Client
}

// client returns a CodeCommit client for the given AWS context.
//
// Credentials come from the standard AWS chain (env, shared config, IMDS/task
// role), so the control plane holds no CodeCommit secret of its own. When
// roleARN is set the chain is used only to assume that role, which is how one
// Forge deployment reads repositories across several accounts.
//
// CODECOMMIT_API_BASE overrides the service endpoint for tests, mirroring
// GITHUB_API_BASE / BITBUCKET_API_BASE. Because a test stub authenticates
// nothing, that mode also pins static placeholder credentials so the SDK does
// not go looking for a real identity that is not there.
func (c *ccClients) client(ctx context.Context, region, roleARN, profile string) (*codecommit.Client, error) {
	if region == "" {
		return nil, errors.New("vcs: codecommit requires a region")
	}
	key := region + "\x00" + roleARN + "\x00" + profile

	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.m[key]; ok {
		return cl, nil
	}

	base := strings.TrimSpace(os.Getenv("CODECOMMIT_API_BASE"))
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if base != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("vcs: load AWS config: %w", err)
	}
	if roleARN != "" && base == "" {
		cfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN))
	}

	cl := codecommit.NewFromConfig(cfg, func(o *codecommit.Options) {
		if base != "" {
			o.BaseEndpoint = aws.String(strings.TrimSuffix(base, "/"))
		}
	})
	if c.m == nil {
		c.m = map[string]*codecommit.Client{}
	}
	c.m[key] = cl
	return cl, nil
}

// getFile reads one file from CodeCommit at an exact commit.
//
// found is false with a nil error for every "the file isn't there" shape —
// absent file, absent path, unknown commit, unknown repository — so the caller
// falls back to the registered config exactly as a GitHub 404 does. Any other
// failure returns an error carrying only the AWS error CODE, never the response
// body, matching how the HTTP providers surface status without body.
func (c *ccClients) getFile(ctx context.Context, repo CodeCommitRepo, roleARN, sha, path string) ([]byte, bool, error) {
	cl, err := c.client(ctx, repo.Region, roleARN, repo.Profile)
	if err != nil {
		return nil, false, err
	}
	in := &codecommit.GetFileInput{
		RepositoryName: aws.String(repo.Name),
		FilePath:       aws.String(path),
	}
	if sha != "" {
		in.CommitSpecifier = aws.String(sha)
	}
	out, err := cl.GetFile(ctx, in)
	if err != nil {
		if codeCommitNotFound(err) {
			return nil, false, nil // fall back to the registered config
		}
		return nil, false, fmt.Errorf("vcs: codecommit GetFile %s: %s", path, awsErrCode(err))
	}
	return out.FileContent, true, nil
}

// codeCommitNotFound reports whether err is one of the "nothing there" faults
// that must degrade to a fall-back rather than fail the pipeline.
func codeCommitNotFound(err error) bool {
	var (
		noFile   *cctypes.FileDoesNotExistException
		noPath   *cctypes.PathDoesNotExistException
		noCommit *cctypes.CommitDoesNotExistException
		noRepo   *cctypes.RepositoryDoesNotExistException
	)
	return errors.As(err, &noFile) || errors.As(err, &noPath) ||
		errors.As(err, &noCommit) || errors.As(err, &noRepo)
}

// awsErrCode reduces an AWS failure to its error code so Forge can report what
// went wrong without echoing a response body into logs or API errors.
func awsErrCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return "request failed"
}

// checkAccess proves that a CodeCommit repository is reachable with the
// resolved AWS identity, using the cheapest call that exercises the same path
// config-from-repo will take. It is the CodeCommit stand-in for the git
// ls-remote the HTTPS providers validate with: git cannot reach CodeCommit
// without IAM-signed git auth on the control-plane host, which Forge does not
// require of an operator. The error carries the AWS error code only.
func (c *ccClients) checkAccess(ctx context.Context, repo CodeCommitRepo, roleARN string) error {
	cl, err := c.client(ctx, repo.Region, roleARN, repo.Profile)
	if err != nil {
		return err
	}
	if _, err := cl.GetRepository(ctx, &codecommit.GetRepositoryInput{
		RepositoryName: aws.String(repo.Name),
	}); err != nil {
		return errors.New(awsErrCode(err))
	}
	return nil
}

// CheckCodeCommitAccess verifies a CodeCommit registration before it is saved.
func (f *Fetcher) CheckCodeCommitAccess(ctx context.Context, repo CodeCommitRepo, roleARN string) error {
	return f.cc.checkAccess(ctx, repo, roleARN)
}
