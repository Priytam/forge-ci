package api

import (
	"context"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const envYAML = `
stages: [build, dev, staging, prod]
jobs:
  build:
    stage: build
    script: [echo build]
  deploy_dev:
    stage: dev
    environment: dev
    script: [echo dev]
  deploy_staging:
    stage: staging
    environment: staging
    script: [echo staging]
  deploy_prod:
    stage: prod
    environment: production
    script: [echo prod]
`

func TestSortEnvironmentsPromotionOrder(t *testing.T) {
	got := []string{"production", "zeta", "staging", "dev", "alpha", "qa"}
	store.SortEnvironments(got)
	if strings.Join(got, ",") != "dev,qa,staging,production,alpha,zeta" {
		t.Errorf("order = %v", got)
	}
}

// TestEnvironmentBoard: one row per repo with deploy activity, one cell per
// environment holding the current deployment and the latest attempt.
func TestEnvironmentBoard(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	u := h.login("dev@acme.io")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.db.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// acme/api: dev and staging deployed; prod failed on the latest run.
	api := h.seedPipeline("acme/api", "main", envYAML)
	exec(`UPDATE jobs SET status='success' WHERE pipeline_id=$1 AND name IN ('build','deploy_dev','deploy_staging')`, api.ID)
	exec(`UPDATE jobs SET status='failed' WHERE pipeline_id=$1 AND name='deploy_prod'`, api.ID)
	for _, env := range []string{"dev", "staging"} {
		exec(`INSERT INTO deployments (repo, environment, sha, ref, pipeline_id, job_id, deployed_by)
		      SELECT 'acme/api', $2, 'abc123', 'main', $1, id, 'dev@acme.io'
		      FROM jobs WHERE pipeline_id=$1 AND environment=$2`, api.ID, env)
	}
	// acme/web: prod waiting for approval; never deployed.
	web := h.seedPipeline("acme/web", "main", envYAML)
	exec(`UPDATE jobs SET status='success' WHERE pipeline_id=$1 AND name IN ('build','deploy_dev','deploy_staging')`, web.ID)
	exec(`UPDATE jobs SET status='blocked' WHERE pipeline_id=$1 AND name='deploy_prod'`, web.ID)
	// acme/lib: no environments at all — not on the board.
	h.seedPipeline("acme/lib", "main", plainYAML)
	// A global prod freeze.
	exec(`INSERT INTO deploy_freezes (repo, environment, starts_at, ends_at, reason)
	      VALUES ('', 'production', now() - interval '1 hour', now() + interval '1 hour', 'release')`)

	type board struct {
		Environments []string             `json:"environments"`
		Rows         []store.EnvMatrixRow `json:"rows"`
	}
	get := func(query string) board {
		t.Helper()
		rec := h.do(u, "GET", "/api/v1/environment-board?"+query, nil)
		h.expect(rec, 200, "board "+query)
		return decodeInto[board](t, rec)
	}
	repos := func(b board) string {
		var out []string
		for _, r := range b.Rows {
			out = append(out, r.Repo)
		}
		return strings.Join(out, ",")
	}

	b := get("")
	if strings.Join(b.Environments, ",") != "dev,staging,production" {
		t.Errorf("environments = %v, want promotion order", b.Environments)
	}
	if repos(b) != "acme/api,acme/web" {
		t.Fatalf("rows = %s, want acme/api,acme/web", repos(b))
	}
	apiRow := b.Rows[0]
	if c := apiRow.Cells["staging"]; c == nil || c.Current == nil || c.Current.SHA != "abc123" || c.Latest.Status != "success" {
		t.Errorf("api staging cell = %+v", c)
	}
	if c := apiRow.Cells["production"]; c == nil || c.Current != nil || c.Latest == nil ||
		c.Latest.Status != "failed" || !c.Frozen {
		t.Errorf("api production cell = %+v, want failed attempt, no current, frozen", c)
	}
	if c := b.Rows[1].Cells["production"]; c == nil || c.Latest.Status != "blocked" {
		t.Errorf("web production cell = %+v, want blocked", c)
	}

	if got := repos(get("q=WEB")); got != "acme/web" {
		t.Errorf("q=WEB rows = %s", got)
	}
	if got := repos(get("q=%25")); got != "" {
		t.Errorf("q=%% should match literally, got %s", got)
	}
	if got := repos(get("env=dev")); got != "acme/api,acme/web" {
		t.Errorf("env=dev rows = %s", got)
	}
	if got := repos(get("env=qa")); got != "" {
		t.Errorf("env=qa rows = %s", got)
	}
	if got := repos(get("attention=1")); got != "acme/api,acme/web" {
		t.Errorf("attention rows = %s", got)
	}
	if got := repos(get("attention=1&env=staging")); got != "" {
		t.Errorf("attention in staging rows = %s, want none", got)
	}
	if got := repos(get("attention=1&env=production")); got != "acme/api,acme/web" {
		t.Errorf("attention in production rows = %s", got)
	}
	rec := h.do(u, "GET", "/api/v1/environment-board?limit=1", nil)
	if rec.Header().Get("X-Total-Count") != "2" || rec.Header().Get("X-Has-More") != "true" {
		t.Errorf("paging headers total=%s more=%s", rec.Header().Get("X-Total-Count"), rec.Header().Get("X-Has-More"))
	}
}

func TestJobListFilters(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	u := h.login("dev@acme.io")
	api := h.seedPipeline("acme/api", "main", envYAML)
	web := h.seedPipeline("acme/web", "feature", envYAML)
	if _, err := h.db.Exec(ctx,
		`UPDATE jobs SET status='running', runner_id='prod-runner' WHERE pipeline_id=$1 AND name='deploy_prod'`, api.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx,
		`UPDATE jobs SET status='failed', runner_id='ci-runner' WHERE pipeline_id=$1 AND name='deploy_dev'`, web.ID); err != nil {
		t.Fatal(err)
	}

	names := func(query string) string {
		t.Helper()
		rec := h.do(u, "GET", "/api/v1/jobs?"+query, nil)
		h.expect(rec, 200, "jobs "+query)
		var out []string
		for _, j := range decodeInto[[]store.JobRow](t, rec) {
			out = append(out, j.Repo+":"+j.Name)
		}
		return strings.Join(out, ",")
	}
	cases := map[string]string{
		"environment=production":                "acme/web:deploy_prod,acme/api:deploy_prod",
		"environment=production&status=running": "acme/api:deploy_prod",
		"runner=ci-runner":                      "acme/web:deploy_dev",
		"status=failed,running":                 "acme/web:deploy_dev,acme/api:deploy_prod",
		"repo=acme/api&name=DEPLOY_S":           "acme/api:deploy_staging",
		"ref=feature&name=build":                "acme/web:build",
		"name=%25":                              "",
	}
	for q, want := range cases {
		if got := names(q); got != want {
			t.Errorf("%s: got %s, want %s", q, got, want)
		}
	}
	h.expect(h.do(u, "GET", "/api/v1/jobs?status=nope", nil), 400, "bad job status")
	rec := h.do(u, "GET", "/api/v1/jobs?repo=acme/api&limit=2", nil)
	if rec.Header().Get("X-Total-Count") != "4" || rec.Header().Get("X-Has-More") != "true" {
		t.Errorf("paging headers total=%s more=%s", rec.Header().Get("X-Total-Count"), rec.Header().Get("X-Has-More"))
	}
}
