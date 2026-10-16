package compiler

import (
	"strings"
	"testing"
)

const deployTemplate = `spec:
  inputs:
    environment: {default: staging, options: [staging, production]}
    cluster: {}
---
jobs:
  deploy:
    stage: deploy
    script: ["./deploy.sh $[[ inputs.environment ]] $[[ inputs.cluster ]]"]
`

func TestTemplateInputs(t *testing.T) {
	ext := func(ref IncludeRef) (string, bool, error) {
		if ref.Kind == "catalog" && ref.Name == "deploy/k8s" && ref.Version == "1.0.0" {
			return deployTemplate, true, nil
		}
		return "", false, nil
	}
	compile := func(inputs string) ([]CompiledJob, error) {
		yml := "stages: [deploy]\ninclude:\n  - template: deploy/k8s@1.0.0\n    inputs: " + inputs + "\n"
		return CompileWith(yml, "main", "api", CompileOpts{Includes: ext})
	}
	jobs, err := compile("{cluster: east}")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !strings.Contains(jobs[0].Script, "./deploy.sh staging east") {
		t.Fatalf("jobs = %+v", jobs)
	}
	for in, want := range map[string]string{
		"{}":                            `input "cluster" is required`,
		"{cluster: e, environment: qa}": `is not one of`,
		"{cluster: e, region: x}":       `unknown input "region"`,
	} {
		if _, err := compile(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("inputs %s: err = %v, want %q", in, err, want)
		}
	}
}

func TestExternalIncludes(t *testing.T) {
	var seen []IncludeRef
	ext := func(ref IncludeRef) (string, bool, error) {
		seen = append(seen, ref)
		switch ref.Kind {
		case "project":
			return "jobs:\n  lint:\n    stage: test\n    script: [make lint]\n", true, nil
		case "remote":
			return "jobs:\n  audit:\n    stage: test\n    script: [make audit]\n", true, nil
		}
		return "", false, nil
	}
	yml := `stages: [test]
include:
  - project: platform/ci
    ref: v2
    file: lint.yml
  - remote: https://ci.example.com/audit.yml
    integrity: sha256-abc
jobs:
  build:
    stage: test
    script: [make]
`
	jobs, err := CompileWith(yml, "main", "api", CompileOpts{Includes: ext})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	if seen[0] != (IncludeRef{Kind: "project", Project: "platform/ci", Ref: "v2", File: "lint.yml"}) ||
		seen[1] != (IncludeRef{Kind: "remote", URL: "https://ci.example.com/audit.yml", Integrity: "sha256-abc"}) {
		t.Errorf("refs = %+v", seen)
	}
	// Without a resolver (e.g. offline lint) the error says why.
	if _, err := Compile(yml, "main", "api", nil); err == nil || !strings.Contains(err.Error(), "not available here") {
		t.Errorf("no resolver: %v", err)
	}
	bad := "include:\n  - template: a\n    remote: https://x\n"
	if _, err := Compile(bad, "main", "api", nil); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("two kinds: %v", err)
	}
}
