package api

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestParseJUnitTestsuitesNested(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="outer" time="1.5">
    <testcase name="passes_a" classname="pkg.A" time="0.5"/>
    <testcase name="fails_b" classname="pkg.B" time="1.0">
      <failure message="expected 1 got 2">assertion failed</failure>
    </testcase>
  </testsuite>
</testsuites>`
	r, _, ok := parseJUnit([]byte(xml), 42)
	if !ok {
		t.Fatal("expected parsed=true")
	}
	if r.Total != 2 || r.Failed != 1 || r.Passed != 1 || r.Skipped != 0 {
		t.Fatalf("counts: total=%d failed=%d passed=%d skipped=%d", r.Total, r.Failed, r.Passed, r.Skipped)
	}
	if len(r.Failures) != 1 || r.Failures[0].Name != "fails_b" {
		t.Fatalf("failures = %+v", r.Failures)
	}
	if r.Failures[0].Type != "failure" || r.Failures[0].Message != "expected 1 got 2" {
		t.Fatalf("failure detail = %+v", r.Failures[0])
	}
	if r.DurationSeconds != 1.5 {
		t.Errorf("duration = %v, want 1.5", r.DurationSeconds)
	}
}

func TestParseJUnitBareTestsuiteWithErrorAndSkip(t *testing.T) {
	xml := `<testsuite name="s">
  <testcase name="ok"/>
  <testcase name="broke"><error message="boom">stack</error></testcase>
  <testcase name="later"><skipped/></testcase>
</testsuite>`
	r, cases, ok := parseJUnit([]byte(xml), 7)
	if !ok {
		t.Fatal("expected parsed=true")
	}
	if r.Total != 3 || r.Failed != 1 || r.Skipped != 1 || r.Passed != 1 {
		t.Fatalf("counts: total=%d failed=%d skipped=%d passed=%d", r.Total, r.Failed, r.Skipped, r.Passed)
	}
	if r.Failures[0].Type != "error" || r.Failures[0].Name != "broke" {
		t.Fatalf("error case = %+v", r.Failures[0])
	}

	// Every case gets a row — including the passed and skipped ones, which the
	// aggregate report only counts (this is the point of returning cases at all).
	if len(cases) != 3 {
		t.Fatalf("expected 3 case rows, got %d: %+v", len(cases), cases)
	}
	byName := map[string]proto.TestCaseResult{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	if byName["ok"].Status != "passed" {
		t.Errorf(`"ok" status = %q, want "passed"`, byName["ok"].Status)
	}
	if byName["broke"].Status != "failed" || byName["broke"].Message != "boom" {
		t.Errorf(`"broke" = %+v, want status=failed message="boom"`, byName["broke"])
	}
	if byName["later"].Status != "skipped" {
		t.Errorf(`"later" status = %q, want "skipped"`, byName["later"].Status)
	}
	for _, c := range cases {
		if c.JobID != 7 {
			t.Errorf("case %q: JobID = %d, want 7", c.Name, c.JobID)
		}
	}
}

func TestParseJUnitNestedSuites(t *testing.T) {
	xml := `<testsuites>
  <testsuite name="parent">
    <testsuite name="child">
      <testcase name="deep_pass"/>
      <testcase name="deep_fail"><failure/></testcase>
    </testsuite>
    <testcase name="top_pass"/>
  </testsuite>
</testsuites>`
	r, _, ok := parseJUnit([]byte(xml), 1)
	if !ok {
		t.Fatal("expected parsed=true")
	}
	if r.Total != 3 || r.Failed != 1 || r.Passed != 2 {
		t.Fatalf("counts: total=%d failed=%d passed=%d", r.Total, r.Failed, r.Passed)
	}
}

func TestParseJUnitMultipleConcatenatedDocuments(t *testing.T) {
	// The runner concatenates several files (each with its own prolog) into one
	// upload; the parser must sum across all top-level roots.
	xml := `<?xml version="1.0"?><testsuite name="a"><testcase name="a1"/></testsuite>
<?xml version="1.0"?><testsuite name="b"><testcase name="b1"/><testcase name="b2"><failure/></testcase></testsuite>`
	r, _, ok := parseJUnit([]byte(xml), 1)
	if !ok {
		t.Fatal("expected parsed=true")
	}
	if r.Total != 3 || r.Failed != 1 || r.Passed != 2 {
		t.Fatalf("counts: total=%d failed=%d passed=%d", r.Total, r.Failed, r.Passed)
	}
}

func TestParseJUnitMalformedOrEmpty(t *testing.T) {
	for _, in := range []string{
		"",
		"   \n  ",
		"not xml at all",
		"<testsuite><testcase name=\"x\">", // truncated / unclosed
		"<other><thing/></other>",          // valid XML but no test cases
	} {
		if r, _, ok := parseJUnit([]byte(in), 1); ok {
			t.Errorf("parseJUnit(%q): expected parsed=false, got %+v", in, r)
		}
	}
}
