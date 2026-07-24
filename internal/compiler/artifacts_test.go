package compiler

import (
	"reflect"
	"testing"
)

func TestParseExpireInUnits(t *testing.T) {
	cases := []struct {
		in      string
		want    int // seconds
		wantErr bool
	}{
		{"", 0, false},
		{"30m", 30 * 60, false},
		{"24h", 24 * 3600, false},
		{"1h30m", 3600 + 30*60, false},
		{"7d", 7 * 24 * 3600, false},
		{"2w", 2 * 7 * 24 * 3600, false},
		{"1s", 1, false},
		{"0", 0, false},
		{"banana", 0, true},
		{"5x", 0, true},
		{"-3d", 0, true},
		{"-5m", 0, true},
	}
	for _, c := range cases {
		got, err := parseExpireIn("test", c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseExpireIn(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseExpireIn(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseExpireIn(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCompileArtifactsExpireAndReports(t *testing.T) {
	yml := `
stages: [build, test]
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [dist/]
      expire_in: 7d
  test:
    stage: test
    script: [make test]
    artifacts:
      reports:
        junit: [report.xml, more/*.xml]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(jobs)

	b := m["build"]
	if b.ArtifactExpireSeconds != 7*24*3600 {
		t.Errorf("build ArtifactExpireSeconds = %d, want %d", b.ArtifactExpireSeconds, 7*24*3600)
	}
	if !reflect.DeepEqual(b.ArtifactPaths, []string{"dist/"}) {
		t.Errorf("build ArtifactPaths = %v", b.ArtifactPaths)
	}

	tj := m["test"]
	if !reflect.DeepEqual([]string(tj.ReportJUnit), []string{"report.xml", "more/*.xml"}) {
		t.Errorf("test ReportJUnit = %v", tj.ReportJUnit)
	}
	// A job with no expire_in stays at 0 (falls back to RETENTION_DAYS).
	if tj.ArtifactExpireSeconds != 0 {
		t.Errorf("test ArtifactExpireSeconds = %d, want 0", tj.ArtifactExpireSeconds)
	}
}

func TestCompileBadExpireInFails(t *testing.T) {
	yml := `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [dist/]
      expire_in: "not-a-duration"
`
	if _, err := Compile(yml, "main", SourceAPI, nil); err == nil {
		t.Fatal("expected compile error for bad expire_in")
	}
}

// Artifacts (paths, expire_in, reports.junit) survive extends: — a child that
// does not redeclare them inherits the base job's values.
func TestArtifactsSurviveExtends(t *testing.T) {
	yml := `
stages: [build]
jobs:
  .base:
    artifacts:
      paths: [dist/]
      expire_in: 2w
      reports:
        junit: [base.xml]
  build:
    stage: build
    extends: .base
    script: [make]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := jobNames(jobs)["build"]
	if b.ArtifactExpireSeconds != 2*7*24*3600 {
		t.Errorf("inherited expire = %d, want %d", b.ArtifactExpireSeconds, 2*7*24*3600)
	}
	if !reflect.DeepEqual(b.ArtifactPaths, []string{"dist/"}) {
		t.Errorf("inherited paths = %v", b.ArtifactPaths)
	}
	if !reflect.DeepEqual([]string(b.ReportJUnit), []string{"base.xml"}) {
		t.Errorf("inherited junit = %v", b.ReportJUnit)
	}
}
