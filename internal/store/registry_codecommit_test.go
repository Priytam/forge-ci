package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// newRegistryTestStore spins up a throwaway database (dropped on cleanup) for
// the repo-registry tests. Skips when Postgres is unreachable.
func newRegistryTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, appBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed registry test", err)
	}
	dbName := fmt.Sprintf("forge_registry_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}
	u, _ := url.Parse(appBaseDSN())
	u.Path = "/" + dbName
	st, err := New(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("store.New on throwaway db: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return st
}

// The provider CHECK constraint rejected 'codecommit' before this change, so a
// round-trip through the real schema is what proves the migration landed.
func TestRegisterCodeCommitRepoRoundTrip(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	reg := proto.RepoRegistration{
		Repo:          "tablespace-api",
		Provider:      "codecommit",
		CloneURL:      "codecommit::ap-south-1://tablespace-api",
		DefaultBranch: "main",
		AWSRegion:     "ap-south-1",
		AWSRoleARN:    "arn:aws:iam::123456789012:role/forge-codecommit-read",
		ConfigSource:  "repo",
	}
	if err := st.RegisterRepo(ctx, reg); err != nil {
		t.Fatalf("RegisterRepo(codecommit): %v", err)
	}

	repos, err := st.ListRegisteredRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	got := repos[0]
	if got.Provider != "codecommit" {
		t.Errorf("Provider = %q", got.Provider)
	}
	if got.AWSRegion != "ap-south-1" {
		t.Errorf("AWSRegion = %q", got.AWSRegion)
	}
	if got.AWSRoleARN != reg.AWSRoleARN {
		t.Errorf("AWSRoleARN = %q", got.AWSRoleARN)
	}
	if got.HasToken {
		t.Error("HasToken = true, want false — CodeCommit stores no token")
	}
}

// The runner must receive the CodeCommit URL exactly as registered. Before this
// change the default branch of cloneAuth spliced an x-access-token into any URL
// with a token, and a stray token on the row must not resurrect that.
func TestCloneAuthCodeCommitIsNeverTokenized(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	const cloneURL = "codecommit::ap-south-1://tablespace-api"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "tablespace-api", Provider: "codecommit", CloneURL: cloneURL,
		DefaultBranch: "main", AWSRegion: "ap-south-1",
	}); err != nil {
		t.Fatal(err)
	}
	// Force a token onto the row, bypassing the API guard, to prove cloneAuth
	// refuses to use one for CodeCommit rather than merely never being given one.
	enc, err := st.cipher.Encrypt("ghp_should_never_be_used")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE repo_registry SET token=$1 WHERE repo='tablespace-api'`, enc); err != nil {
		t.Fatal(err)
	}

	gotURL, gotToken, err := st.cloneAuth(ctx, "tablespace-api")
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != cloneURL {
		t.Errorf("cloneAuth URL = %q, want it unchanged (%q)", gotURL, cloneURL)
	}
	if gotToken != "" {
		t.Errorf("cloneAuth token = %q, want empty for CodeCommit", gotToken)
	}
	if strings.Contains(gotURL, "x-access-token") || strings.Contains(gotURL, "ghp_") {
		t.Errorf("cloneAuth spliced credentials into a CodeCommit URL: %q", gotURL)
	}
}

// Regression: the GitHub and Bitbucket clone-URL tokenization must be exactly
// what it was before the repoConn refactor.
func TestCloneAuthTokenizationUnchangedForHTTPSProviders(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/gh", Provider: "github",
		CloneURL: "https://github.com/acme/gh.git", Token: "ghp_tok", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "team/bb", Provider: "bitbucket",
		CloneURL: "https://bitbucket.org/team/bb.git", Token: "bb_tok", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	ghURL, ghTok, err := st.cloneAuth(ctx, "acme/gh")
	if err != nil {
		t.Fatal(err)
	}
	if ghURL != "https://x-access-token:ghp_tok@github.com/acme/gh.git" {
		t.Errorf("github cloneAuth URL = %q", ghURL)
	}
	if ghTok != "ghp_tok" {
		t.Errorf("github cloneAuth token = %q", ghTok)
	}

	bbURL, bbTok, err := st.cloneAuth(ctx, "team/bb")
	if err != nil {
		t.Fatal(err)
	}
	if bbURL != "https://x-token-auth:bb_tok@bitbucket.org/team/bb.git" {
		t.Errorf("bitbucket cloneAuth URL = %q", bbURL)
	}
	if bbTok != "bb_tok" {
		t.Errorf("bitbucket cloneAuth token = %q", bbTok)
	}
}

// The registered region/profile are authoritative, with the clone URL as the
// fall-back for connections registered by URL alone.
func TestRepoConnCodeCommitRepoPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		conn        repoConn
		wantRegion  string
		wantName    string
		wantProfile string
		wantOK      bool
	}{
		{
			name: "registered columns win over the URL",
			conn: repoConn{
				CloneURL: "codecommit::us-east-1://widgets",
				AWS:      awsConn{Region: "ap-south-1", Profile: "prod"},
			},
			wantRegion: "ap-south-1", wantName: "widgets", wantProfile: "prod", wantOK: true,
		},
		{
			name:       "URL-only registration still resolves",
			conn:       repoConn{CloneURL: "codecommit::eu-west-2://widgets"},
			wantRegion: "eu-west-2", wantName: "widgets", wantOK: true,
		},
		{
			name: "region column rescues a URL with no region",
			conn: repoConn{
				CloneURL: "https://git-codecommit.ap-south-1.amazonaws.com/v1/repos/widgets",
				AWS:      awsConn{Region: "ap-south-1"},
			},
			wantRegion: "ap-south-1", wantName: "widgets", wantOK: true,
		},
		{
			name: "no repository name anywhere",
			conn: repoConn{CloneURL: "https://github.com/acme/widgets.git",
				AWS: awsConn{Region: "ap-south-1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.conn.codeCommitRepo()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if !ok {
				return
			}
			if got.Region != tt.wantRegion || got.Name != tt.wantName || got.Profile != tt.wantProfile {
				t.Errorf("codeCommitRepo = %+v, want region=%q name=%q profile=%q",
					got, tt.wantRegion, tt.wantName, tt.wantProfile)
			}
		})
	}
}
