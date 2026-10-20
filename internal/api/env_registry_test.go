package api

import (
	"context"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const policyYAML = `
stages: [dev, staging, prod, sandbox]
jobs:
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
    environment: prd
    script: [echo prod]
  deploy_sandbox:
    stage: sandbox
    environment: sandbox
    script: [echo sandbox]
`

var testPolicy = map[string]any{
	"unknown": "deny",
	"environments": []map[string]any{
		{"name": "dev", "tier": "dev"},
		{"name": "staging", "tier": "staging", "allowed_refs": []string{"main", "release/*"}},
		{"name": "production", "tier": "prod", "aliases": []string{"prd", "prod"},
			"allowed_refs": []string{"main", "release/*"}},
	},
}

func TestEnvPolicyUnit(t *testing.T) {
	e := store.RegisteredEnv{Name: "production", AllowedRefs: []string{"main", "release/*"}}
	for ref, want := range map[string]bool{
		"main": true, "refs/heads/main": true, "release/1.2": true, "release/2024/q1": true,
		"feature/x": false, "mainline": false, "": false,
	} {
		if got := e.AllowsRef(ref); got != want {
			t.Errorf("AllowsRef(%q) = %v, want %v", ref, got, want)
		}
	}
	if (&store.RegisteredEnv{Name: "dev"}).AllowsRef("anything/at/all") != true {
		t.Error("no allowed_refs should allow any branch")
	}
	bad := store.EnvPolicy{Environments: []store.RegisteredEnv{
		{Name: "production", Aliases: []string{"prod"}}, {Name: "Prod"},
	}}
	if err := bad.Validate(); err == nil {
		t.Error("an alias reused as another environment's name should be rejected")
	}
	if err := (&store.EnvPolicy{Unknown: "maybe"}).Validate(); err == nil {
		t.Error("bad unknown mode should be rejected")
	}
	empty := store.EnvPolicy{}
	if c, d := empty.CheckJob("prd", "feature/x"); c != "prd" || d != "" {
		t.Errorf("empty policy changed behaviour: %q %q", c, d)
	}
}

// TestEnvRegistryEnforcesBranchesAndAliases is the end-to-end path: before a
// registry exists nothing changes; saving one renames stored aliases; new
// pipelines then get canonical names and branch rules.
func TestEnvRegistryEnforcesBranchesAndAliases(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	dev := h.login("dev@acme.io")
	const repo = "acme/app"

	jobs := func(pipelineID int64) map[string]proto.Job {
		t.Helper()
		_, js, err := h.st.GetPipeline(ctx, pipelineID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]proto.Job{}
		for _, j := range js {
			out[j.Name] = j
		}
		return out
	}
	run := func(ref string) int64 {
		t.Helper()
		rec := h.do(dev, "POST", "/api/v1/pipelines", map[string]any{
			"repo": repo, "ref": ref, "sha": "abc123", "config": policyYAML,
		})
		h.expect(rec, 201, "create pipeline on "+ref)
		return decodeInto[struct {
			Pipeline proto.Pipeline `json:"pipeline"`
		}](t, rec).Pipeline.ID
	}

	// No registry: a feature branch's prod job is created like any other.
	before := run("feature/x")
	if j := jobs(before)["deploy_prod"]; j.Status != "created" || *j.Environment != "prd" {
		t.Fatalf("without a registry: deploy_prod = %s/%v", j.Status, *j.Environment)
	}
	// Admin config typed with aliases before the registry existed.
	if _, err := h.db.Exec(ctx, `INSERT INTO protected_environments (repo, name) VALUES ($1, 'prod')`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `INSERT INTO deploy_freezes (repo, environment, starts_at, ends_at)
		VALUES ($1, 'PRD', now(), now() + interval '1 hour')`, repo); err != nil {
		t.Fatal(err)
	}

	h.expect(h.do(dev, "PUT", "/api/v1/environment-registry", testPolicy), 403, "developer edits registry")
	h.expect(h.do(admin, "PUT", "/api/v1/environment-registry", testPolicy), 200, "admin saves registry")

	// Stored aliases were renamed.
	if j := jobs(before)["deploy_prod"]; *j.Environment != "production" {
		t.Errorf("old job environment = %q, want renamed to production", *j.Environment)
	}
	var n int
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM protected_environments WHERE repo=$1 AND name='production'`, repo).Scan(&n)
	if n != 1 {
		t.Errorf("approval rule not renamed to production")
	}
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM deploy_freezes WHERE environment='production'`).Scan(&n)
	if n != 1 {
		t.Errorf("freeze not renamed to production")
	}

	// Feature branch: dev runs; staging and prod are refused; unknown denied.
	feat := jobs(run("feature/y"))
	if feat["deploy_dev"].Status != "created" {
		t.Errorf("deploy_dev on feature = %s", feat["deploy_dev"].Status)
	}
	for _, name := range []string{"deploy_staging", "deploy_prod"} {
		j := feat[name]
		if j.Status != "failed" || !strings.Contains(j.StatusReason, `branch "feature/y" may not deploy`) {
			t.Errorf("%s on feature = %s %q", name, j.Status, j.StatusReason)
		}
	}
	if j := feat["deploy_prod"]; *j.Environment != "production" {
		t.Errorf("deploy_prod environment = %q, want canonical production", *j.Environment)
	}
	if j := feat["deploy_sandbox"]; j.Status != "failed" || !strings.Contains(j.StatusReason, "not registered") {
		t.Errorf("deploy_sandbox = %s %q", j.Status, j.StatusReason)
	}

	// Release branch: allowed everywhere registered.
	rel := jobs(run("release/2.0"))
	for _, name := range []string{"deploy_dev", "deploy_staging", "deploy_prod"} {
		if rel[name].Status == "failed" {
			t.Errorf("%s on release/2.0 refused: %q", name, rel[name].StatusReason)
		}
	}

	// The job list carries the reason too.
	rec := h.do(dev, "GET", "/api/v1/jobs?environment=production&status=failed", nil)
	h.expect(rec, 200, "jobs")
	if rows := decodeInto[[]store.JobRow](t, rec); len(rows) != 1 || rows[0].StatusReason == "" {
		t.Errorf("failed production jobs = %+v", rows)
	}

	// Anyone signed in can read the registry; order is the admin's.
	rec = h.do(dev, "GET", "/api/v1/environment-registry", nil)
	h.expect(rec, 200, "read registry")
	if p := decodeInto[store.EnvPolicy](t, rec); len(p.Environments) != 3 || p.Unknown != "deny" ||
		p.Environments[2].Name != "production" || p.Environments[2].AllowedRefs[1] != "release/*" {
		t.Errorf("registry = %+v", p)
	}
	// New admin input typed with an alias lands on the canonical name.
	h.expect(h.do(admin, "POST", "/api/v1/protected-environments",
		map[string]any{"repo": "acme/other", "name": "prd", "required_approvals": 1}), 200, "rule via alias")
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM protected_environments WHERE repo='acme/other' AND name='production'`).Scan(&n)
	if n != 1 {
		t.Errorf("rule created via alias not stored as production")
	}
}

// TestEnvRegistryFoldsConflictingRules: when a repo has approval rules for
// both an alias and the canonical name, saving the registry folds them into
// one canonical rule with the stricter settings (it used to refuse the save
// and leave the weaker rule in place).
func TestEnvRegistryFoldsConflictingRules(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	if _, err := h.db.Exec(ctx, `INSERT INTO protected_environments (repo, name, required_approvals, allow_self_approval) VALUES
		('acme/app', 'prod', 3, FALSE), ('acme/app', 'production', 1, TRUE)`); err != nil {
		t.Fatal(err)
	}
	rec := h.do(admin, "PUT", "/api/v1/environment-registry", testPolicy)
	h.expect(rec, 200, "fold instead of conflict")
	var n, required int
	var selfOK bool
	_ = h.db.QueryRow(ctx, `SELECT count(*) FROM protected_environments WHERE repo='acme/app'`).Scan(&n)
	_ = h.db.QueryRow(ctx, `SELECT required_approvals, allow_self_approval FROM protected_environments WHERE repo='acme/app' AND name='production'`).Scan(&required, &selfOK)
	if n != 1 || required != 3 || selfOK {
		t.Errorf("after fold: %d rules, required %d, self-approval %v; want 1 / 3 / false", n, required, selfOK)
	}
}

func TestEnvRegistryOrdersBoard(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "PUT", "/api/v1/environment-registry", map[string]any{
		"environments": []map[string]any{{"name": "qa"}, {"name": "dev"}, {"name": "live", "tier": "prod"}},
	}), 200, "save registry")
	rec := h.do(admin, "GET", "/api/v1/environment-board", nil)
	h.expect(rec, 200, "board")
	got := decodeInto[struct {
		Environments []string `json:"environments"`
	}](t, rec).Environments
	// Registered names first in the admin's order; then "production", which
	// comes from the default global approval rule every server ships with.
	if strings.Join(got, ",") != "qa,dev,live,production" {
		t.Errorf("board environments = %v, want qa,dev,live,production", got)
	}
}
