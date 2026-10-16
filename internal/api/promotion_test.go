package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const promoteYAML = `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [make image]
    coverage: '/total coverage: (\d+\.\d+)%/'
    artifacts:
      reports:
        dotenv: build.env
  deploy-staging:
    stage: deploy
    needs: [build]
    variables: {APP_HOST: example.com}
    environment: {name: staging, url: "https://staging.$APP_HOST"}
    script: [./deploy staging]
  deploy-production:
    stage: deploy
    needs: [build]
    when: manual
    environment: production
    script: [./deploy production]
`

func runnerPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// runNext promotes ready jobs, acquires the next one (which must be want),
// lets it log/report, then completes it successfully.
func runNext(t *testing.T, st *store.Store, db *pgx.Conn, srv *Server, want string, log, dotenv string) *proto.RunnerJob {
	t.Helper()
	ctx := context.Background()
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil || rj.Name != want {
		t.Fatalf("acquire: want %s, got %+v %v", want, rj, err)
	}
	base := "/api/v1/runner/jobs/" + strconv.FormatInt(rj.ID, 10)
	if log != "" {
		if rec := runnerPost(t, srv, base+"/logs", log); rec.Code >= 300 {
			t.Fatalf("logs = %d %s", rec.Code, rec.Body.String())
		}
	}
	if dotenv != "" {
		if rec := runnerPost(t, srv, base+"/dotenv", dotenv); rec.Code != 200 {
			t.Fatalf("dotenv = %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := runnerPost(t, srv, base+"/complete", `{"status":"success","exit_code":0}`); rec.Code != 200 {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body.String())
	}
	return rj
}

func TestDeployRecordsAndPromotion(t *testing.T) {
	st, db := newCFRStore(t, "")
	ctx := context.Background()
	srv := New(st, nil, logstore.Resolve(ctx, st, nil))
	const repo = "acme/shop"
	jobs, err := compiler.Compile(promoteYAML, "main", compiler.SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: repo, Ref: "main", SHA: "c0ffee1234567890", Config: promoteYAML, Source: "push"}, jobs, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if rj := runNext(t, st, db, srv, "build", "running tests\ntotal coverage: 81.5%\n", "DEPLOY_VERSION=1.4.2\nDEPLOY_DIGEST=sha256:abc123\nexport IMAGE='registry.example.com/shop@sha256:abc123'\n"); rj.ReportDotenvPath != "build.env" {
		t.Errorf("runner was not told about the dotenv report: %+v", rj)
	}
	staging := runNext(t, st, db, srv, "deploy-staging", "", "")
	if staging.Env["DEPLOY_VERSION"] != "1.4.2" || staging.Env["IMAGE"] != "registry.example.com/shop@sha256:abc123" {
		t.Errorf("deploy job did not inherit the build's dotenv: %v", staging.Env)
	}

	deps, _, err := st.ListDeployments(ctx, repo, "staging", 10, 0)
	if err != nil || len(deps) != 1 {
		t.Fatalf("staging deployments = %v %v", deps, err)
	}
	d := deps[0]
	if d.URL != "https://staging.example.com" || d.Version != "1.4.2" || d.Digest != "sha256:abc123" {
		t.Errorf("deployment record = %+v", d)
	}
	_, pjobs, _ := st.GetPipeline(ctx, p.ID)
	for _, j := range pjobs {
		if j.Name == "build" && (j.Coverage == nil || *j.Coverage != 81.5) {
			t.Errorf("build coverage = %v, want 81.5", j.Coverage)
		}
	}

	// Promote staging → production: only the production job, reusing the build.
	rec := runnerPost(t, srv, "/api/v1/environments/"+repo+"/production/promote", `{"from_environment":"staging"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("promote = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Pipeline proto.Pipeline `json:"pipeline"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	pp, pjobs, _ := st.GetPipeline(ctx, out.Pipeline.ID)
	if pp.Source != "promotion" || pp.SHA != "c0ffee1234567890" || pp.PromotedFrom == nil || *pp.PromotedFrom != d.ID {
		t.Errorf("promotion pipeline = %+v", pp)
	}
	if len(pjobs) != 1 || pjobs[0].Name != "deploy-production" {
		t.Fatalf("promotion jobs = %+v, want only deploy-production", pjobs)
	}
	// It is still a manual job: the promotion doesn't bypass gates.
	if pjobs[0].Status != "blocked" || !pjobs[0].Manual {
		t.Errorf("promoted job = %s manual=%v, want blocked manual", pjobs[0].Status, pjobs[0].Manual)
	}
	if _, err := st.StartManualJob(ctx, pjobs[0].ID); err != nil {
		t.Fatal(err)
	}
	// production is protected: once played, the promoted deploy still waits
	// for an approval like any other production deploy.
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.GetJob(ctx, pjobs[0].ID); j.Status != "blocked" {
		t.Fatalf("promoted production deploy = %s, want blocked on approval", j.Status)
	}
	if _, err := st.Approve(ctx, pjobs[0].ID, proto.ApprovalRequest{Approver: "release@acme.io", Verdict: "approved"}); err != nil {
		t.Fatal(err)
	}
	prod := runNext(t, st, db, srv, "deploy-production", "", "")
	if prod.Env["DEPLOY_DIGEST"] != "sha256:abc123" || prod.Env["CI_PROMOTED_FROM_ENVIRONMENT"] != "staging" || prod.Env["CI_PIPELINE_SOURCE"] != "promotion" {
		t.Errorf("promoted job env = %v", prod.Env)
	}
	pdeps, _, _ := st.ListDeployments(ctx, repo, "production", 10, 0)
	if len(pdeps) != 1 || pdeps[0].Digest != "sha256:abc123" || pdeps[0].Version != "1.4.2" ||
		pdeps[0].PromotedFrom == nil || *pdeps[0].PromotedFrom != d.ID {
		t.Errorf("production deployment = %+v", pdeps)
	}

	// Errors: same environment, nothing to promote, no job for the target.
	for path, want := range map[string]int{
		"/api/v1/environments/" + repo + "/staging/promote": 400,
		"/api/v1/environments/" + repo + "/qa/promote":      400,
	} {
		if rec := runnerPost(t, srv, path, `{"from_environment":"staging"}`); rec.Code != want {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := runnerPost(t, srv, "/api/v1/environments/"+repo+"/production/promote", `{"from_environment":"nowhere"}`); rec.Code != 404 {
		t.Errorf("unknown source = %d", rec.Code)
	}

	// Coverage trend.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/coverage?repo="+repo, nil)
	crec := httptest.NewRecorder()
	srv.ServeHTTP(crec, req)
	if !bytes.Contains(crec.Body.Bytes(), []byte(`"coverage":81.5`)) {
		t.Errorf("coverage trend = %s", crec.Body.String())
	}
}

func TestCancelSupersededDeploys(t *testing.T) {
	st, _ := newCFRStore(t, "")
	ctx := context.Background()
	if err := st.SaveEnvPolicy(ctx, store.EnvPolicy{Environments: []store.RegisteredEnv{
		{Name: "live", Tier: "prod", CancelSuperseded: true},
		{Name: "staging", Tier: "staging"},
	}}); err != nil {
		t.Fatal(err)
	}
	yml := "stages: [d]\njobs:\n  prod:\n    stage: d\n    environment: live\n    when: manual\n    script: [x]\n  stg:\n    stage: d\n    environment: staging\n    when: manual\n    script: [x]\n"
	mk := func(sha string) []proto.Job {
		jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
		p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "release/" + sha, SHA: sha, Config: yml}, jobs, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		_, js, _ := st.GetPipeline(ctx, p.ID)
		return js
	}
	old := mk("old")
	newer := mk("new")
	byName := func(js []proto.Job, n string) proto.Job {
		for _, j := range js {
			if j.Name == n {
				return j
			}
		}
		return proto.Job{}
	}
	if n, _ := st.CancelSupersededDeploys(ctx); n != 0 {
		t.Fatalf("nothing has deployed yet, canceled %d", n)
	}
	// The newer pipeline deploys to both environments.
	for _, n := range []string{"prod", "stg"} {
		_, _ = st.StartManualJob(ctx, byName(newer, n).ID)
	}
	_, _ = st.PromoteReadyJobs(ctx)
	for i := 0; i < 2; i++ {
		rj, _ := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r"})
		if rj == nil {
			t.Fatal("expected a job")
		}
		if _, _, err := st.CompleteJob(ctx, rj.ID, "success", 0); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.CancelSupersededDeploys(ctx); err != nil || n != 1 {
		t.Fatalf("canceled = %d %v, want 1 (only live opts in)", n, err)
	}
	_, js, _ := st.GetPipeline(ctx, byName(old, "prod").PipelineID)
	if p := byName(js, "prod"); p.Status != "canceled" || p.StatusReason == "" {
		t.Errorf("old prod deploy = %s %q, want canceled with a reason", p.Status, p.StatusReason)
	}
	if s := byName(js, "stg"); s.Status != "blocked" {
		t.Errorf("old staging deploy = %s, want untouched (staging doesn't opt in)", s.Status)
	}
}
