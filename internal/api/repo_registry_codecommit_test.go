package api

import (
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// A region alone is enough: the clone URL is derived from it, so an operator
// registering a CodeCommit repo need not know the URL form at all.
func TestNormalizeCodeCommitDerivesCloneURL(t *testing.T) {
	req := proto.RepoRegistration{
		Repo: "tablespace-api", Provider: "codecommit", AWSRegion: "ap-south-1",
	}
	if err := normalizeCodeCommitRegistration(&req); err != nil {
		t.Fatal(err)
	}
	if req.CloneURL != "codecommit::ap-south-1://tablespace-api" {
		t.Errorf("CloneURL = %q", req.CloneURL)
	}
	if req.AWSRegion != "ap-south-1" {
		t.Errorf("AWSRegion = %q", req.AWSRegion)
	}
}

// A clone URL alone is equally enough: the region is read back out of it, so the
// stored record and the URL cannot drift apart.
func TestNormalizeCodeCommitDerivesRegionFromURL(t *testing.T) {
	for _, tt := range []struct {
		name, url, wantRegion, wantProfile string
	}{
		{"grc form", "codecommit::eu-west-2://widgets", "eu-west-2", ""},
		{"grc form with profile", "codecommit::us-east-1://prod@widgets", "us-east-1", "prod"},
		{
			"https form",
			"https://git-codecommit.ap-south-1.amazonaws.com/v1/repos/widgets",
			"ap-south-1", "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := proto.RepoRegistration{Repo: "widgets", Provider: "codecommit", CloneURL: tt.url}
			if err := normalizeCodeCommitRegistration(&req); err != nil {
				t.Fatal(err)
			}
			if req.AWSRegion != tt.wantRegion {
				t.Errorf("AWSRegion = %q, want %q", req.AWSRegion, tt.wantRegion)
			}
			if req.AWSProfile != tt.wantProfile {
				t.Errorf("AWSProfile = %q, want %q", req.AWSProfile, tt.wantProfile)
			}
			if req.CloneURL != tt.url {
				t.Errorf("CloneURL = %q, want it left as supplied", req.CloneURL)
			}
		})
	}
}

// An explicit region wins over the URL, so an operator correcting a record is
// not silently overridden by a stale URL.
func TestNormalizeCodeCommitExplicitRegionWins(t *testing.T) {
	req := proto.RepoRegistration{
		Repo: "widgets", Provider: "codecommit", AWSRegion: "ap-south-1",
		CloneURL: "codecommit::us-east-1://widgets",
	}
	if err := normalizeCodeCommitRegistration(&req); err != nil {
		t.Fatal(err)
	}
	if req.AWSRegion != "ap-south-1" {
		t.Errorf("AWSRegion = %q, want the explicit ap-south-1", req.AWSRegion)
	}
}

func TestNormalizeCodeCommitRejections(t *testing.T) {
	tests := []struct {
		name    string
		req     proto.RepoRegistration
		wantMsg string
	}{
		{
			name:    "no region and no clone url",
			req:     proto.RepoRegistration{Repo: "widgets", Provider: "codecommit"},
			wantMsg: "aws_region",
		},
		{
			name: "a token is never stored for codecommit",
			req: proto.RepoRegistration{
				Repo: "widgets", Provider: "codecommit",
				AWSRegion: "ap-south-1", Token: "ghp_nope",
			},
			wantMsg: "takes no token",
		},
		{
			name: "owner/name repo key",
			req: proto.RepoRegistration{
				Repo: "acme/widgets", Provider: "codecommit", AWSRegion: "ap-south-1",
			},
			wantMsg: "no owner/ prefix",
		},
		{
			name: "clone url that is not codecommit",
			req: proto.RepoRegistration{
				Repo: "widgets", Provider: "codecommit",
				CloneURL: "https://github.com/acme/widgets.git",
			},
			wantMsg: "codecommit::<region>://<repo>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalizeCodeCommitRegistration(&tt.req)
			if err == nil {
				t.Fatalf("expected an error, got nil (req = %+v)", tt.req)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// The AWS fields are non-secret and SHOULD appear in the audit detail — but a
// CodeCommit registration must still record no credential, because it has none.
func TestRepoRegistryAuditDetailCodeCommit(t *testing.T) {
	d := repoRegistryAuditDetail(proto.RepoRegistration{
		Repo:       "tablespace-api",
		Provider:   "codecommit",
		CloneURL:   "codecommit::ap-south-1://tablespace-api",
		AWSRegion:  "ap-south-1",
		AWSRoleARN: "arn:aws:iam::123456789012:role/forge-codecommit-read",
	})
	if d["aws_region"] != "ap-south-1" {
		t.Errorf("aws_region = %v", d["aws_region"])
	}
	if d["aws_role_arn"] != "arn:aws:iam::123456789012:role/forge-codecommit-read" {
		t.Errorf("aws_role_arn = %v", d["aws_role_arn"])
	}
	if d["has_token"] != false {
		t.Errorf("has_token = %v, want false — CodeCommit stores no token", d["has_token"])
	}
	if d["github_app"] != false {
		t.Errorf("github_app = %v, want false", d["github_app"])
	}
}

// Regression: the AWS keys must appear ONLY for CodeCommit, so a GitHub or
// Bitbucket audit record is byte-for-byte what it was before this provider
// existed.
func TestRepoRegistryAuditDetailNoAWSKeysForOtherProviders(t *testing.T) {
	for _, provider := range []string{"github", "bitbucket", "other"} {
		t.Run(provider, func(t *testing.T) {
			d := repoRegistryAuditDetail(proto.RepoRegistration{
				Repo: "acme/app", Provider: provider,
				CloneURL: "https://example.com/acme/app.git", Token: "secret",
			})
			for _, k := range []string{"aws_region", "aws_profile", "aws_role_arn"} {
				if _, present := d[k]; present {
					t.Errorf("%s present for provider %q", k, provider)
				}
			}
		})
	}
}
