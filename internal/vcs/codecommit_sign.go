package vcs

// IAM-signed git access to CodeCommit.
//
// Forge clones every other provider with an HTTPS token embedded in the remote
// URL (https://<token>@host/...). CodeCommit issues no such token: git access is
// authenticated with SigV4 over the caller's AWS identity. There are two ways to
// do that, and Forge implements the one with no external dependency:
//
//   - `git-remote-codecommit`, a Python helper that must be installed on every
//     runner and put a `codecommit::` remote helper on PATH; or
//   - signing the GRC HTTPS URL directly, which is exactly what that helper does
//     internally — it computes a SigV4 signature over a synthetic "GIT" request
//     and hands git a URL whose password is that signature.
//
// SignCloneURL is the second. It reproduces git-remote-codecommit's signing
// scheme byte for byte, so the resulting URL is one CodeCommit accepts from any
// stock git, with nothing installed and no static credential anywhere. The
// signature is derived from short-lived credentials and is good for a few
// minutes — ample for a clone, and worthless if it leaks afterwards.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AWSCredentials is one set of resolved AWS credentials. SessionToken is set for
// temporary credentials (the OIDC and assume-role paths); it is empty for
// long-lived ones. Both the secret and the token are secret-equivalent and must
// reach the job's redaction set before any git command runs.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// codeCommitSigningService is the SigV4 service name CodeCommit signs git
// requests under.
const codeCommitSigningService = "codecommit"

// SignCloneURL returns an HTTPS clone URL for repo whose credentials are a SigV4
// signature over the request, valid for a few minutes from at.
//
// The scheme is git-remote-codecommit's:
//
//	canonical request = "GIT\n/v1/repos/<name>\n\nhost:<host>\n\nhost\n"
//	string to sign    = "AWS4-HMAC-SHA256\n<ts>\n<date>/<region>/codecommit/aws4_request\n<sha256(canonical)>"
//	password          = "<ts>Z" + hex(hmac(signing key, string to sign))
//	username          = access key id, with "%<session token>" appended when the
//	                    credentials are temporary
//
// The username is percent-encoded whole (a session token contains characters
// that are not URL-safe), and the password is hex, so the result is always a
// well-formed URL.
func SignCloneURL(repo CodeCommitRepo, creds AWSCredentials, at time.Time) (string, error) {
	if repo.Region == "" || repo.Name == "" {
		return "", errors.New("vcs: codecommit sign: region and repository name are required")
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return "", errors.New("vcs: codecommit sign: no AWS credentials resolved")
	}

	at = at.UTC()
	timestamp := at.Format("20060102T150405")
	date := timestamp[:8]

	host := fmt.Sprintf("git-codecommit.%s.amazonaws.com", repo.Region)
	path := "/v1/repos/" + repo.Name

	// The empty lines are the (absent) canonical query string and the end of the
	// canonical headers block; "host" is the only signed header.
	canonicalRequest := "GIT\n" + path + "\n\nhost:" + host + "\n\nhost\n"

	scope := strings.Join([]string{date, repo.Region, codeCommitSigningService, "aws4_request"}, "/")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	signature := hex.EncodeToString(
		hmacSHA256(sigV4SigningKey(creds.SecretAccessKey, date, repo.Region), stringToSign))

	username := creds.AccessKeyID
	if creds.SessionToken != "" {
		username += "%" + creds.SessionToken
	}
	return fmt.Sprintf("https://%s:%s@%s%s",
		url.QueryEscape(username), timestamp+"Z"+signature, host, path), nil
}

// sigV4SigningKey derives the date/region/service-scoped signing key from the
// secret access key (the standard AWS4 HMAC chain).
func sigV4SigningKey(secretKey, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, codeCommitSigningService)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// AWSAuthOIDC and AWSAuthInstance name where a runner's AWS identity came from.
// They are reported back so a job log can say which path authenticated the
// clone without ever showing the credentials themselves.
const (
	AWSAuthOIDC     = "oidc"
	AWSAuthInstance = "instance"
)

// ResolveRunnerAWSCredentials resolves the AWS identity a runner clones with.
//
// The default is Forge's keyless OIDC path: the per-job ID token Forge already
// mints (FORGE_OIDC_TOKEN) is exchanged via STS AssumeRoleWithWebIdentity for
// short-lived credentials on a role granting codecommit:GitPull. Nothing static
// is stored on the runner, and the role's trust policy can condition on the
// token's repo/ref claims — so a runner can only clone what the pipeline it is
// running is entitled to.
//
// The fall-back is the ambient credential chain (an EC2 instance role, an ECS
// task role, or env credentials), used when there is no token or no role to
// assume, or when the operator pins FORGE_AWS_AUTH=instance. Callers pass
// forceInstance for that pin.
//
// The returned source is AWSAuthOIDC or AWSAuthInstance, for logging only.
func ResolveRunnerAWSCredentials(
	ctx context.Context, region, roleARN, oidcToken, sessionName string, forceInstance bool,
) (AWSCredentials, string, error) {
	if region == "" {
		return AWSCredentials{}, "", errors.New("vcs: codecommit auth: region is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return AWSCredentials{}, "", fmt.Errorf("vcs: load AWS config: %w", err)
	}

	source := AWSAuthInstance
	if !forceInstance && oidcToken != "" && roleARN != "" {
		if sessionName == "" {
			sessionName = "forge-ci"
		}
		cfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewWebIdentityRoleProvider(
				sts.NewFromConfig(cfg), roleARN, staticIdentityToken(oidcToken),
				func(o *stscreds.WebIdentityRoleOptions) { o.RoleSessionName = sessionName },
			))
		source = AWSAuthOIDC
	} else if !forceInstance && roleARN != "" {
		// A role with no token: assume it from the ambient identity instead.
		cfg.Credentials = aws.NewCredentialsCache(
			stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN,
				func(o *stscreds.AssumeRoleOptions) {
					if sessionName != "" {
						o.RoleSessionName = sessionName
					}
				}))
	}

	got, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		// The AWS error code is enough to act on; the body may echo the request.
		return AWSCredentials{}, source, fmt.Errorf(
			"vcs: resolve AWS credentials (%s): %s", source, awsErrCode(err))
	}
	return AWSCredentials{
		AccessKeyID:     got.AccessKeyID,
		SecretAccessKey: got.SecretAccessKey,
		SessionToken:    got.SessionToken,
	}, source, nil
}

// staticIdentityToken adapts an already-minted OIDC token to the retriever the
// STS provider expects. Forge injects the token into the job environment, so
// there is no file to read and no fetch to make.
type staticIdentityToken string

func (t staticIdentityToken) GetIdentityToken() ([]byte, error) { return []byte(t), nil }
