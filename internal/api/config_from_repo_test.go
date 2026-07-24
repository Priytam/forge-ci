package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// In-repo config (fetched from the stub) and the registered config carry
// DISTINCT job names, so the created pipeline's jobs prove which source won.
const inRepoYAML = `
stages: [test]
jobs:
  inrepo_job:
    stage: test
    script: [echo from-the-repo]
`

const registeredYAML = `
stages: [test]
jobs:
  registered_job:
    stage: test
    script: [echo from-the-registry]
`

func cfrBaseDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://forge:forge@localhost:5433/forge?sslmode=disable"
}

// newCFRStore creates a throwaway DB with GITHUB_API_BASE pointed at the given
// contents stub (set BEFORE store.New so the store's config fetcher picks it up).
// It also returns a direct connection to the throwaway DB so the test can read
// pipelines.config_yaml / config_source (not surfaced by the store API).
func newCFRStore(t *testing.T, stubURL string) (*store.Store, *pgx.Conn) {
	t.Helper()
	t.Setenv("GITHUB_API_BASE", stubURL)
	t.Setenv("FORGE_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, cfrBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed config-from-repo test", err)
	}
	dbName := fmt.Sprintf("forge_cfr_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}
	u, _ := url.Parse(cfrBaseDSN())
	u.Path = "/" + dbName
	st, err := store.New(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("store.New on throwaway db: %v", err)
	}
	db, err := pgx.Connect(ctx, u.String())
	if err != nil {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("connect to throwaway db: %v", err)
	}
	t.Cleanup(func() {
		db.Close(ctx)
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return st, db
}

// contentsStub emulates the GitHub contents API for .forge-ci.yml, tracking hits
// per repo so we can assert whether the file was fetched at all.
type contentsStub struct {
	mu   sync.Mutex
	hits map[string]int // repo (owner/name) -> contents-API hit count
	// serve maps repo -> (yaml, statusCode). statusCode 404 => not found.
	serve map[string]struct {
		yaml   string
		status int
	}
}

func newContentsStub() (*contentsStub, *httptest.Server) {
	cs := &contentsStub{
		hits: map[string]int{},
		serve: map[string]struct {
			yaml   string
			status int
		}{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/{owner}/{name}/contents/.forge-ci.yml
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[0] != "repos" || parts[3] != "contents" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		repo := parts[1] + "/" + parts[2]
		cs.mu.Lock()
		cs.hits[repo]++
		entry, ok := cs.serve[repo]
		cs.mu.Unlock()
		if !ok || entry.status == http.StatusNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.raw")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(entry.yaml))
	}))
	return cs, srv
}

func (cs *contentsStub) hitsFor(repo string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.hits[repo]
}

// postPush sends a GitHub push webhook through the real Server.ServeHTTP path.
func postPush(t *testing.T, srv *Server, repo, sha, delivery string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"repository":{"full_name":%q},"pusher":{"name":"octocat"}}`, sha, repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// latestPipeline returns the newest pipeline for a repo, its job names, its
// stored config_source, and its stored config_yaml.
func latestPipeline(t *testing.T, st *store.Store, db *pgx.Conn, repo string) (p *proto.Pipeline, jobNames []string, cfgSource, storedYAML string) {
	t.Helper()
	ctx := context.Background()
	ps, err := st.ListPipelines(ctx, repo)
	if err != nil {
		t.Fatalf("ListPipelines(%s): %v", repo, err)
	}
	if len(ps) == 0 {
		t.Fatalf("no pipeline created for %s", repo)
	}
	full, jobs, err := st.GetPipeline(ctx, ps[0].ID)
	if err != nil {
		t.Fatalf("GetPipeline(%d): %v", ps[0].ID, err)
	}
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}
	if err := db.QueryRow(ctx,
		`SELECT config_yaml, config_source FROM pipelines WHERE id=$1`, ps[0].ID).
		Scan(&storedYAML, &cfgSource); err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	return full, names, cfgSource, storedYAML
}

// TestConfigFromRepoFullFlow drives three repos through the real github push
// webhook handler and asserts the config-source precedence end to end.
func TestConfigFromRepoFullFlow(t *testing.T) {
	ctx := context.Background()
	stub, srv := newContentsStub()
	defer srv.Close()
	st, db := newCFRStore(t, srv.URL)
	server := New(st, nil, nil)

	const sha = "deadbeefcafe0001"

	// --- repo 1: config_source=repo, in-repo file present -> in-repo used ---
	const repo1 = "acme/inrepo"
	stub.mu.Lock()
	stub.serve[repo1] = struct {
		yaml   string
		status int
	}{inRepoYAML, http.StatusOK}
	stub.mu.Unlock()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo1, Provider: "github", CloneURL: "https://github.com/" + repo1 + ".git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "repo",
	}); err != nil {
		t.Fatalf("register repo1: %v", err)
	}
	// NOTE: no registered config for repo1 — proves the in-repo file is the source.

	if rec := postPush(t, server, repo1, sha, "d1"); rec.Code != http.StatusCreated {
		t.Fatalf("repo1 push: code=%d body=%s", rec.Code, rec.Body.String())
	}
	p1, names1, src1, stored1 := latestPipeline(t, st, db, repo1)
	if len(names1) != 1 || names1[0] != "inrepo_job" {
		t.Errorf("repo1 jobs = %v, want [inrepo_job] (the in-repo config)", names1)
	}
	if strings.TrimSpace(stored1) != strings.TrimSpace(inRepoYAML) {
		t.Errorf("repo1 stored config_yaml is not the fetched YAML:\n%s", stored1)
	}
	if src1 != "repo" {
		t.Errorf("repo1 config_source = %q, want repo", src1)
	}
	if p1.ConfigVersion != nil {
		t.Errorf("repo1 config_version = %v, want nil (in-repo config is not a registry version)", *p1.ConfigVersion)
	}
	if stub.hitsFor(repo1) != 1 {
		t.Errorf("repo1 contents-API hits = %d, want 1", stub.hitsFor(repo1))
	}

	// --- repo 2: config_source=repo, in-repo file 404 -> fall back to registered ---
	const repo2 = "acme/fallback"
	stub.mu.Lock()
	stub.serve[repo2] = struct {
		yaml   string
		status int
	}{"", http.StatusNotFound}
	stub.mu.Unlock()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo2, Provider: "github", CloneURL: "https://github.com/" + repo2 + ".git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "repo",
	}); err != nil {
		t.Fatalf("register repo2: %v", err)
	}
	if _, err := st.SetRepoConfig(ctx, repo2, registeredYAML, "admin", "initial"); err != nil {
		t.Fatalf("SetRepoConfig repo2: %v", err)
	}
	if rec := postPush(t, server, repo2, sha, "d2"); rec.Code != http.StatusCreated {
		t.Fatalf("repo2 push: code=%d body=%s", rec.Code, rec.Body.String())
	}
	p2, names2, src2, stored2 := latestPipeline(t, st, db, repo2)
	if len(names2) != 1 || names2[0] != "registered_job" {
		t.Errorf("repo2 jobs = %v, want [registered_job] (fell back to registered)", names2)
	}
	if strings.TrimSpace(stored2) != strings.TrimSpace(registeredYAML) {
		t.Errorf("repo2 stored config_yaml is not the registered YAML:\n%s", stored2)
	}
	if src2 != "registered" {
		t.Errorf("repo2 config_source = %q, want registered", src2)
	}
	if p2.ConfigVersion == nil || *p2.ConfigVersion != 1 {
		t.Errorf("repo2 config_version = %v, want 1 (registered version stamped)", p2.ConfigVersion)
	}
	if stub.hitsFor(repo2) != 1 {
		t.Errorf("repo2 contents-API hits = %d, want 1 (attempted, then 404 fallback)", stub.hitsFor(repo2))
	}

	// --- repo 3: config_source=registered, in-repo file present -> registered used, NO fetch ---
	const repo3 = "acme/registered"
	stub.mu.Lock()
	stub.serve[repo3] = struct {
		yaml   string
		status int
	}{inRepoYAML, http.StatusOK} // present, but must NOT be fetched
	stub.mu.Unlock()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo3, Provider: "github", CloneURL: "https://github.com/" + repo3 + ".git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "registered",
	}); err != nil {
		t.Fatalf("register repo3: %v", err)
	}
	if _, err := st.SetRepoConfig(ctx, repo3, registeredYAML, "admin", "initial"); err != nil {
		t.Fatalf("SetRepoConfig repo3: %v", err)
	}
	if rec := postPush(t, server, repo3, sha, "d3"); rec.Code != http.StatusCreated {
		t.Fatalf("repo3 push: code=%d body=%s", rec.Code, rec.Body.String())
	}
	_, names3, src3, stored3 := latestPipeline(t, st, db, repo3)
	if len(names3) != 1 || names3[0] != "registered_job" {
		t.Errorf("repo3 jobs = %v, want [registered_job] (registered mode)", names3)
	}
	if strings.TrimSpace(stored3) != strings.TrimSpace(registeredYAML) {
		t.Errorf("repo3 stored config_yaml is not the registered YAML:\n%s", stored3)
	}
	if src3 != "registered" {
		t.Errorf("repo3 config_source = %q, want registered", src3)
	}
	if got := stub.hitsFor(repo3); got != 0 {
		t.Errorf("repo3 contents-API hits = %d, want 0 (registered mode must never fetch)", got)
	}
}
