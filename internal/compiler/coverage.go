package compiler

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// validateCoverageRegex accepts GitLab's /regex/ form (or a bare pattern) and
// returns the Go pattern, checking that it compiles.
func validateCoverageRegex(job, raw string) (string, error) {
	pat := strings.TrimSpace(raw)
	if pat == "" {
		return "", nil
	}
	if len(pat) >= 2 && strings.HasPrefix(pat, "/") && strings.HasSuffix(pat, "/") {
		pat = pat[1 : len(pat)-1]
	}
	if _, err := regexp.Compile(pat); err != nil {
		return "", fmt.Errorf("job %q: coverage: invalid regex: %w", job, err)
	}
	return pat, nil
}

var percentRe = regexp.MustCompile(`\d+(?:\.\d+)?`)

// ExtractCoverage applies a coverage regex to a job log and returns the
// percentage from the LAST matching line (tools print a running total last).
// The first capture group is used when there is one, else the whole match;
// the first number inside it is the value.
func ExtractCoverage(pattern, log string) (float64, bool) {
	if pattern == "" {
		return 0, false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, false
	}
	matches := re.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return 0, false
	}
	m := matches[len(matches)-1]
	text := m[0]
	if len(m) > 1 && m[1] != "" {
		text = m[1]
	}
	num := percentRe.FindString(text)
	if num == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 || v > 100 {
		return 0, false
	}
	return v, true
}
