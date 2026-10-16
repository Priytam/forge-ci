package compiler

import (
	"strings"
	"testing"
)

func TestEnvironmentLongFormAndReports(t *testing.T) {
	yml := `stages: [d]
jobs:
  .base:
    environment: {name: staging, url: "https://$HOST"}
  deploy:
    extends: .base
    stage: d
    coverage: '/Coverage: (\d+)%/'
    artifacts:
      reports:
        dotenv: out/deploy.env
        coverage_report: {coverage_format: cobertura, path: coverage.xml}
    script: [x]
  plain:
    stage: d
    environment: production
    script: [x]
`
	jobs, err := Compile(yml, "main", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]CompiledJob{}
	for _, j := range jobs {
		by[j.Name] = j
	}
	d := by["deploy"]
	if d.Environment != "staging" || d.EnvironmentURL != "https://$HOST" || d.ReportDotenv != "out/deploy.env" ||
		d.CoverageRegex != `Coverage: (\d+)%` || d.CoverageReportPath != "coverage.xml" {
		t.Errorf("deploy = %+v", d)
	}
	if by["plain"].Environment != "production" || by["plain"].EnvironmentURL != "" {
		t.Errorf("plain = %+v", by["plain"])
	}
	for bad, want := range map[string]string{
		"coverage: '/(unclosed/'":                  "invalid regex",
		"artifacts: {reports: {dotenv: ../x.env}}": "workspace-relative",
		"artifacts: {reports: {coverage_report: {coverage_format: jacoco, path: c.xml}}}": "not supported",
	} {
		y := "stages: [d]\njobs:\n  j:\n    stage: d\n    script: [x]\n    " + bad + "\n"
		if _, err := Compile(y, "main", "api", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", bad, err, want)
		}
	}
}

func TestExtractCoverage(t *testing.T) {
	log := "ok pkg/a coverage: 70.1% of statements\nok pkg/b coverage: 88.0% of statements\ntotal: (statements) 83.4%\n"
	for pat, want := range map[string]float64{
		`coverage: (\d+\.\d+)%`:             88.0, // last match wins
		`total: \(statements\)\s+([\d.]+)%`: 83.4,
		`TOTAL.*\s(\d+)%`:                   -1,
	} {
		got, ok := ExtractCoverage(pat, log)
		if want < 0 {
			if ok {
				t.Errorf("%s: matched %v, want no match", pat, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s = %v %v, want %v", pat, got, ok, want)
		}
	}
}
