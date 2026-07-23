package compiler

import "testing"

const refYAML = `
stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    script: [make]
  unit-tests:
    stage: test
    script: [make test]
  deploy-dev:
    stage: deploy
    except: [main, release-*]
    script: [./deploy.sh dev]
  deploy-prod:
    stage: deploy
    only: [main, release-*]
    environment: production
    script: [./deploy.sh prod]
`

func jobNames(jobs []CompiledJob) map[string]CompiledJob {
	m := map[string]CompiledJob{}
	for _, j := range jobs {
		m[j.Name] = j
	}
	return m
}

func TestCompileRefAwareDAG(t *testing.T) {
	main, err := Compile(refYAML, "main")
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(main)
	if _, ok := m["deploy-prod"]; !ok {
		t.Error("main: expected deploy-prod")
	}
	if _, ok := m["deploy-dev"]; ok {
		t.Error("main: deploy-dev must be excluded")
	}

	feat, err := Compile(refYAML, "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	f := jobNames(feat)
	if _, ok := f["deploy-dev"]; !ok {
		t.Error("feature: expected deploy-dev")
	}
	if _, ok := f["deploy-prod"]; ok {
		t.Error("feature: deploy-prod must be excluded")
	}

	rel, err := Compile(refYAML, "release-1.2")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(rel)["deploy-prod"]; !ok {
		t.Error("release-1.2: glob only should include deploy-prod")
	}
}

func TestImplicitNeedsSkipEmptyStage(t *testing.T) {
	yml := `
stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    script: [make]
  only-main-tests:
    stage: test
    only: [main]
    script: [make test]
  deploy:
    stage: deploy
    script: [./deploy.sh]
`
	jobs, err := Compile(yml, "feature/x") // test stage empty for this ref
	if err != nil {
		t.Fatal(err)
	}
	deploy := jobNames(jobs)["deploy"]
	if len(deploy.Needs) != 1 || deploy.Needs[0] != "build-app" {
		t.Errorf("deploy should fall back to needing build-app, got %v", deploy.Needs)
	}
}

func TestExplicitNeedOnExcludedJobFails(t *testing.T) {
	yml := `
stages: [test, deploy]
jobs:
  only-main-tests:
    stage: test
    only: [main]
    script: [make test]
  deploy:
    stage: deploy
    needs: [only-main-tests]
    script: [./deploy.sh]
`
	if _, err := Compile(yml, "feature/x"); err == nil {
		t.Fatal("expected error: explicit need on ref-excluded job")
	}
}
