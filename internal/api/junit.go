package api

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// JUnit XML has no single canonical schema, so the parser is intentionally
// lenient. A document may be rooted at <testsuites> (a collection) or a single
// <testsuite>; suites may nest other suites; each <testcase> is a pass unless it
// carries a <failure>, <error> or <skipped> child. The runner concatenates all
// matched report files into one upload, so the parser loops over every
// top-level element in the stream (ignoring <?xml?> declarations and comments),
// which lets several documents — each with its own prolog — parse in one pass.

type junitResult struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failures  []junitResult `xml:"failure"`
	Errors    []junitResult `xml:"error"`
	Skipped   *struct{}     `xml:"skipped"`
}

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	Time   float64      `xml:"time,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"` // nested suites (some tools emit these)
}

// parseJUnit parses concatenated JUnit XML into a per-job summary plus one
// TestCaseResult per case (passed, failed and skipped alike — the aggregate
// report only counts them). It returns ok=false when the input is empty, not
// valid XML, or contains no test cases — callers treat that as "no report"
// (never a job failure). On success, Failed counts both <failure> and <error>
// cases; Passed = Total - Failed - Skipped.
func parseJUnit(data []byte, jobID int64) (proto.JUnitReport, []proto.TestCaseResult, bool) {
	report := proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}
	var cases []proto.TestCaseResult
	if len(bytes.TrimSpace(data)) == 0 {
		return report, nil, false
	}

	dec := xml.NewDecoder(bytes.NewReader(data))
	// Be tolerant of non-UTF-8 declarations and stray entities in messages.
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			report.Total++
			report.DurationSeconds += c.Time
			switch {
			case len(c.Failures) > 0:
				report.Failed++
				f := failure(c, "failure", c.Failures[0])
				report.Failures = append(report.Failures, f)
				cases = append(cases, caseResult(c, jobID, "failed", f.Message))
			case len(c.Errors) > 0:
				report.Failed++
				f := failure(c, "error", c.Errors[0])
				report.Failures = append(report.Failures, f)
				cases = append(cases, caseResult(c, jobID, "failed", f.Message))
			case c.Skipped != nil:
				report.Skipped++
				cases = append(cases, caseResult(c, jobID, "skipped", ""))
			default:
				report.Passed++
				cases = append(cases, caseResult(c, jobID, "passed", ""))
			}
		}
		for _, child := range s.Suites {
			walk(child)
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, nil, false
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "testsuites":
			var col struct {
				Suites []junitSuite `xml:"testsuite"`
			}
			if err := dec.DecodeElement(&col, &se); err != nil {
				return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, nil, false
			}
			for _, s := range col.Suites {
				walk(s)
			}
		case "testsuite":
			var s junitSuite
			if err := dec.DecodeElement(&s, &se); err != nil {
				return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, nil, false
			}
			walk(s)
		}
	}

	if report.Total == 0 {
		return report, nil, false
	}
	return report, cases, true
}

// caseResult builds one TestCaseResult row for a parsed <testcase>. message is
// only meaningful for a failed case (the failure/error text); passed and
// skipped cases carry none.
func caseResult(c junitCase, jobID int64, status, message string) proto.TestCaseResult {
	return proto.TestCaseResult{
		JobID:           jobID,
		Name:            c.Name,
		Classname:       c.Classname,
		Status:          status,
		DurationSeconds: c.Time,
		Message:         message,
	}
}

// failure builds a JUnitFailure, preferring the message attribute and falling
// back to the trimmed element body. Long messages are capped for storage.
func failure(c junitCase, kind string, r junitResult) proto.JUnitFailure {
	msg := strings.TrimSpace(r.Message)
	if msg == "" {
		msg = strings.TrimSpace(r.Body)
	}
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return proto.JUnitFailure{
		Name:      c.Name,
		Classname: c.Classname,
		Type:      kind,
		Message:   msg,
	}
}
