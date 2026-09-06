package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
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

// The candidate scan is where acceptance criterion 5 is actually enforced: a
// CodeCommit PR run must be offered exactly once, at its terminal phase, and a
// CodeCommit PUSH run must never be offered at all (nothing to comment on).
func TestPipelinesPendingStatusPostCodeCommit(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "tablespace-api", Provider: "codecommit", AWSRegion: "ap-south-1",
		CloneURL: "codecommit::ap-south-1://tablespace-api", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	newPipeline := func(t *testing.T, mrIID, baseSHA, jobStatus string) int64 {
		t.Helper()
		var id int64
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO pipelines (repo, ref, sha, config_yaml, source, mr_iid, mr_base_sha)
			 VALUES ('tablespace-api','feature/x','1111','{}','merge_request',$1,$2)
			 RETURNING id`, mrIID, baseSHA).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, script, status)
			 VALUES ($1,'build','test',0,'echo hi',$2)`,
			id, jobStatus); err != nil {
			t.Fatal(err)
		}
		return id
	}

	find := func(t *testing.T, id int64) *StatusCandidate {
		t.Helper()
		got, err := st.PipelinesPendingStatusPost(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			if got[i].PipelineID == id {
				return &got[i]
			}
		}
		return nil
	}

	t.Run("push run is never a candidate", func(t *testing.T) {
		id := newPipeline(t, "", "", "success")
		if c := find(t, id); c != nil {
			t.Errorf("a CodeCommit run with no pull request was offered: %+v", c)
		}
	})

	t.Run("PR run mid-flight is not a candidate", func(t *testing.T) {
		id := newPipeline(t, "42", "2222", "running")
		if c := find(t, id); c != nil {
			t.Errorf("a non-terminal phase was offered: %+v — CodeCommit comments only on the final result", c)
		}
	})

	t.Run("PR run at a terminal phase is offered with its PR identity", func(t *testing.T) {
		id := newPipeline(t, "43", "3333", "failed")
		c := find(t, id)
		if c == nil {
			t.Fatal("a finished CodeCommit PR run was not offered")
		}
		if c.Status != "failed" {
			t.Errorf("Status = %q, want failed", c.Status)
		}
		if c.MRIID != "43" || c.MRBaseSHA != "3333" {
			t.Errorf("PR identity = (%q, %q), want (43, 3333)", c.MRIID, c.MRBaseSHA)
		}
		if c.Provider != "codecommit" {
			t.Errorf("Provider = %q", c.Provider)
		}
	})

	t.Run("claiming the post removes it from the scan", func(t *testing.T) {
		id := newPipeline(t, "44", "4444", "success")
		if find(t, id) == nil {
			t.Fatal("expected the run to be offered before the claim")
		}
		claimed, err := st.ClaimStatusPost(ctx, id, "success")
		if err != nil || !claimed {
			t.Fatalf("ClaimStatusPost: claimed=%v err=%v", claimed, err)
		}
		if c := find(t, id); c != nil {
			t.Errorf("run offered again after a terminal post was claimed: %+v", c)
		}
	})
}

// Regression: a GitHub repo with no credential is still excluded, and one with a
// token is still offered at every phase — CodeCommit's terminal-only rule must
// not have leaked across.
func TestPipelinesPendingStatusPostGitHubUnchanged(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/tokenless", Provider: "github",
		CloneURL: "https://github.com/acme/tokenless.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/tokened", Provider: "github", Token: "ghp_x",
		CloneURL: "https://github.com/acme/tokened.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/tokenless", "acme/tokened"} {
		var id int64
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO pipelines (repo, ref, sha, config_yaml, source)
			 VALUES ($1,'main','abc','{}','webhook') RETURNING id`, repo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, script, status)
			 VALUES ($1,'b','test',0,'echo hi','running')`,
			id); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.PipelinesPendingStatusPost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawTokened, sawTokenless bool
	for _, c := range got {
		switch c.Repo {
		case "acme/tokened":
			sawTokened = true
			if c.Status != "running" {
				t.Errorf("github status = %q, want running — mid-flight phases still post", c.Status)
			}
		case "acme/tokenless":
			sawTokenless = true
		}
	}
	if !sawTokened {
		t.Error("a tokened GitHub repo was not offered")
	}
	if sawTokenless {
		t.Error("a GitHub repo with no credential was offered")
	}
}

// The config link is emitted only for a run that actually used the in-repo
// file. A registered-config run did NOT run whatever is in git at that commit,
// so linking there would show the wrong pipeline definition.
func TestConfigFileURLOnlyForRepoSourcedRuns(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/app", Provider: "github", Token: "ghp_x",
		CloneURL: "https://github.com/acme/app.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	link, err := st.ConfigFileURL(ctx, "acme/app", "deadbeef", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://github.com/acme/app/blob/deadbeef/.forge-ci.yml" {
		t.Errorf("repo-sourced link = %q", link)
	}

	for _, source := range []string{"registered", ""} {
		link, err := st.ConfigFileURL(ctx, "acme/app", "deadbeef", source)
		if err != nil {
			t.Fatal(err)
		}
		if link != "" {
			t.Errorf("config_source=%q produced a link (%q); the git file is not what ran",
				source, link)
		}
	}
}

// A repo's config_path override must be reflected in the link.
func TestConfigFileURLHonoursConfigPath(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/app", Provider: "github", Token: "ghp_x",
		CloneURL: "https://github.com/acme/app.git", DefaultBranch: "main",
		ConfigSource: "repo", ConfigPath: "ci/forge.yml",
	}); err != nil {
		t.Fatal(err)
	}
	link, err := st.ConfigFileURL(ctx, "acme/app", "abc123", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://github.com/acme/app/blob/abc123/ci/forge.yml" {
		t.Errorf("link = %q, want the repo's config_path", link)
	}
}

// An unregistered repo, and one on provider "other", must both yield no link
// and no error — the page renders normally with nothing to click.
func TestConfigFileURLNoLinkCases(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	link, err := st.ConfigFileURL(ctx, "never/registered", "abc", "repo")
	if err != nil {
		t.Fatalf("unregistered repo should not error: %v", err)
	}
	if link != "" {
		t.Errorf("unregistered repo produced a link: %q", link)
	}

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/selfhosted", Provider: "other",
		CloneURL: "https://git.example.com/acme/selfhosted.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	link, err = st.ConfigFileURL(ctx, "acme/selfhosted", "abc", "repo")
	if err != nil {
		t.Fatalf(`provider "other" should not error: %v`, err)
	}
	if link != "" {
		t.Errorf(`provider "other" produced a link: %q`, link)
	}
}

// A CodeCommit run links into the region-scoped console browser.
func TestConfigFileURLCodeCommit(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "tablespace-api", Provider: "codecommit", AWSRegion: "ap-south-1",
		CloneURL: "codecommit::ap-south-1://tablespace-api", DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	link, err := st.ConfigFileURL(ctx, "tablespace-api", "9fceb02d", "repo")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://ap-south-1.console.aws.amazon.com/codesuite/codecommit/" +
		"repositories/tablespace-api/browse/9fceb02d/--/.forge-ci.yml?region=ap-south-1"
	if link != want {
		t.Errorf("link =\n  %q\nwant\n  %q", link, want)
	}
}

// The approval gate's state is not derivable from a job's status: a 'blocked'
// job looks identical whether nobody has voted or one of two approvers already
// has. GetPipeline must therefore carry the vote record and the requirement.
func TestGetPipelineCarriesApprovalGateState(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO protected_environments (repo, name, required_approvals, allow_self_approval)
		 VALUES ('acme/app', 'production', 2, true)`); err != nil {
		t.Fatal(err)
	}

	var pid int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO pipelines (repo, ref, sha, config_yaml, source, triggered_by)
		 VALUES ('acme/app','main','abc','{}','webhook','alice@acme.test') RETURNING id`).
		Scan(&pid); err != nil {
		t.Fatal(err)
	}
	newJob := func(t *testing.T, name, env, status string) int64 {
		t.Helper()
		var id int64
		var envArg any
		if env != "" {
			envArg = env
		}
		if err := st.pool.QueryRow(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, script, status, environment)
			 VALUES ($1,$2,'deploy',0,'echo hi',$3,$4) RETURNING id`,
			pid, name, status, envArg).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	gated := newJob(t, "deploy-prod", "production", "blocked")
	newJob(t, "build", "", "success")

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO job_approvals (job_id, approver, verdict, comment)
		 VALUES ($1,'bob@acme.test','approved','checked the migration plan')`, gated); err != nil {
		t.Fatal(err)
	}

	p, jobs, err := st.GetPipeline(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	// Who started the run — previously not exposed at all.
	if p.TriggeredBy != "alice@acme.test" {
		t.Errorf("TriggeredBy = %q, want alice@acme.test", p.TriggeredBy)
	}

	byName := map[string]proto.Job{}
	for _, j := range jobs {
		byName[j.Name] = j
	}

	got := byName["deploy-prod"]
	if got.RequiredApprovals != 2 {
		t.Errorf("RequiredApprovals = %d, want 2", got.RequiredApprovals)
	}
	if len(got.Approvals) != 1 {
		t.Fatalf("got %d approvals, want 1", len(got.Approvals))
	}
	if got.Approvals[0].Approver != "bob@acme.test" ||
		got.Approvals[0].Verdict != "approved" ||
		got.Approvals[0].Comment != "checked the migration plan" {
		t.Errorf("approval = %+v", got.Approvals[0])
	}

	// A job with no environment has no gate and must stay untouched, so a
	// pipeline without gates costs no rule lookups.
	if ungated := byName["build"]; ungated.RequiredApprovals != 0 || len(ungated.Approvals) != 0 {
		t.Errorf("ungated job carries gate state: required=%d approvals=%d",
			ungated.RequiredApprovals, len(ungated.Approvals))
	}
}

// The job page renders the same gate as the DAG, so GetJob must carry it too.
func TestGetJobCarriesApprovalGateState(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO protected_environments (repo, name, required_approvals)
		 VALUES ('', 'staging', 1)`); err != nil {
		t.Fatal(err)
	}
	var pid int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO pipelines (repo, ref, sha, config_yaml, source)
		 VALUES ('acme/app','main','abc','{}','api') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var jid int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, script, status, environment)
		 VALUES ($1,'deploy','deploy',0,'echo hi','failed','staging') RETURNING id`, pid).
		Scan(&jid); err != nil {
		t.Fatal(err)
	}
	// A rejection ends the gate and flips the job to 'failed'. The record has to
	// survive that, or the UI loses who rejected the deploy and why.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO job_approvals (job_id, approver, verdict, comment)
		 VALUES ($1,'carol@acme.test','rejected','hold until the index migration lands')`,
		jid); err != nil {
		t.Fatal(err)
	}

	j, err := st.GetJob(ctx, jid)
	if err != nil {
		t.Fatal(err)
	}
	if j.RequiredApprovals != 1 {
		t.Errorf("RequiredApprovals = %d, want 1 (global rule)", j.RequiredApprovals)
	}
	if len(j.Approvals) != 1 || j.Approvals[0].Verdict != "rejected" {
		t.Fatalf("approvals = %+v, want the rejection preserved on a finished job", j.Approvals)
	}
	if j.Approvals[0].Comment != "hold until the index migration lands" {
		t.Errorf("comment = %q", j.Approvals[0].Comment)
	}
}

// The commit facts describe the CODE; triggered_by describes the person. A
// merge is the case that separates them, and both must survive the round trip.
func TestPipelineCarriesCommitIdentity(t *testing.T) {
	st := newRegistryTestStore(t)
	ctx := context.Background()

	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "acme/app", Ref: "main", SHA: "abc123",
		Config: "jobs:\n  build:\n    script: echo hi",
		// alice merged dana's work: the actor is not the author.
		TriggeredBy:   "alice@acme.test",
		CommitAuthor:  "dana",
		CommitMessage: "Fix booking race condition\n\nbody nobody reads",
		Source:        "webhook",
	}, []compiler.CompiledJob{{
		Name: "build", Stage: "test", StageIdx: 0, Script: "echo hi",
	}}, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := st.GetPipeline(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TriggeredBy != "alice@acme.test" {
		t.Errorf("TriggeredBy = %q", got.TriggeredBy)
	}
	if got.CommitAuthor != "dana" {
		t.Errorf("CommitAuthor = %q, want dana — the author is not the actor", got.CommitAuthor)
	}
	if got.CommitMessage != "Fix booking race condition\n\nbody nobody reads" {
		t.Errorf("CommitMessage = %q", got.CommitMessage)
	}

	// The list views feed the pipeline table and repo cards, so they carry it too.
	list, err := st.ListPipelines(ctx, "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d pipelines, want 1", len(list))
	}
	if list[0].CommitAuthor != "dana" || list[0].TriggeredBy != "alice@acme.test" {
		t.Errorf("list row lost the identities: %+v", list[0])
	}
}

// A commit message is an unbounded field written from an external event, so it
// is capped — and cut on a rune boundary, never mid-character.
func TestCommitMessageIsCapped(t *testing.T) {
	long := strings.Repeat("a", maxCommitMessageBytes+500)
	if got := capCommitMessage(long); len(got) != maxCommitMessageBytes {
		t.Errorf("len = %d, want %d", len(got), maxCommitMessageBytes)
	}
	if got := capCommitMessage("short"); got != "short" {
		t.Errorf("a short message must pass through unchanged, got %q", got)
	}
	// A multi-byte rune straddling the cap must not be sliced in half.
	multi := strings.Repeat("a", maxCommitMessageBytes-1) + "é" + "tail"
	got := capCommitMessage(multi)
	if !utf8.ValidString(got) {
		t.Errorf("cap produced invalid UTF-8: %q", got[len(got)-4:])
	}
}
