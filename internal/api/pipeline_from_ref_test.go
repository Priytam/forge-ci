package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestCreatePipelineFromRepoConfig: POST /api/v1/pipelines without config
// runs the repo's .forge-ci.yml at the given commit (a missed webhook is
// replayed without pasting YAML); a repo with no config at all says so.
func TestCreatePipelineFromRepoConfig(t *testing.T) {
	ctx := context.Background()
	stub, srv := newContentsStub()
	defer srv.Close()
	st, db := newCFRStore(t, srv.URL)
	server := New(st, nil, nil)

	const repo = "acme/replay"
	stub.mu.Lock()
	stub.serve[repo] = struct {
		yaml   string
		status int
	}{inRepoYAML, http.StatusOK}
	stub.mu.Unlock()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo, Provider: "github", CloneURL: "https://github.com/" + repo + ".git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "repo",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	rec := post(`{"repo":"acme/replay","ref":"main","sha":"deadbeefcafe0002"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create from repo: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Pipeline proto.Pipeline `json:"pipeline"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	_, names, src, stored := latestPipeline(t, st, db, repo)
	if len(names) != 1 || names[0] != "inrepo_job" || src != "repo" || strings.TrimSpace(stored) != strings.TrimSpace(inRepoYAML) {
		t.Fatalf("pipeline ran jobs %v from %q, want the in-repo config", names, src)
	}
	if out.Pipeline.SHA != "deadbeefcafe0002" {
		t.Errorf("sha = %q", out.Pipeline.SHA)
	}

	// A repo with neither an in-repo file nor a registered config.
	const bare = "acme/noconfig"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: bare, Provider: "github", CloneURL: "https://github.com/" + bare + ".git",
		DefaultBranch: "main", Token: "ghp_t", ConfigSource: "repo",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rec = post(`{"repo":"acme/noconfig","ref":"main","sha":"deadbeefcafe0003"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no pipeline config") {
		t.Fatalf("no config anywhere: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"repo":"acme/replay"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing ref: %d", rec.Code)
	}
}
