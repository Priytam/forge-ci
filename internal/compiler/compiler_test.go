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

func TestRetryPrecedenceAndBounds(t *testing.T) {
	yml := `
stages: [build, test]
default:
  retry: 1
jobs:
  build-app:
    stage: build
    retry: 3
    script: [make]
  unit-tests:
    stage: test
    script: [make test]
`
	jobs, err := Compile(yml, "main")
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(jobs)
	if got := m["build-app"].Retry; got != 3 {
		t.Errorf("build-app retry: want job-level 3, got %d", got)
	}
	if got := m["unit-tests"].Retry; got != 1 {
		t.Errorf("unit-tests retry: want default 1, got %d", got)
	}

	// Out-of-bounds retry is rejected.
	bad := `
stages: [build]
jobs:
  b:
    stage: build
    retry: 11
    script: [make]
`
	if _, err := Compile(bad, "main"); err == nil {
		t.Fatal("expected error for retry > 10")
	}
	neg := `
stages: [build]
jobs:
  b:
    stage: build
    retry: -1
    script: [make]
`
	if _, err := Compile(neg, "main"); err == nil {
		t.Fatal("expected error for negative retry")
	}
}

func TestAutoCancelOption(t *testing.T) {
	base := "stages: [b]\njobs:\n  j:\n    stage: b\n    script: [echo hi]\n"
	// Default (absent) => true.
	opts, err := Options(base)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.AutoCancel {
		t.Error("auto_cancel should default to true when absent")
	}
	// Explicit false.
	opts, err = Options("auto_cancel: false\n" + base)
	if err != nil {
		t.Fatal(err)
	}
	if opts.AutoCancel {
		t.Error("auto_cancel: false should be honored")
	}
	// Explicit true.
	opts, err = Options("auto_cancel: true\n" + base)
	if err != nil || !opts.AutoCancel {
		t.Errorf("auto_cancel: true should parse, got %v err=%v", opts.AutoCancel, err)
	}
}
