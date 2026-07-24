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

// parseJUnit parses concatenated JUnit XML into a per-job summary. It returns
// ok=false when the input is empty, not valid XML, or contains no test cases —
// callers treat that as "no report" (never a job failure). On success, Failed
// counts both <failure> and <error> cases; Passed = Total - Failed - Skipped.
func parseJUnit(data []byte, jobID int64) (proto.JUnitReport, bool) {
	report := proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return report, false
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
				report.Failures = append(report.Failures, failure(c, "failure", c.Failures[0]))
			case len(c.Errors) > 0:
				report.Failed++
				report.Failures = append(report.Failures, failure(c, "error", c.Errors[0]))
			case c.Skipped != nil:
				report.Skipped++
			default:
				report.Passed++
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
			return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, false
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
				return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, false
			}
			for _, s := range col.Suites {
				walk(s)
			}
		case "testsuite":
			var s junitSuite
			if err := dec.DecodeElement(&s, &se); err != nil {
				return proto.JUnitReport{JobID: jobID, Failures: []proto.JUnitFailure{}}, false
			}
			walk(s)
		}
	}

	if report.Total == 0 {
		return report, false
	}
	return report, true
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
