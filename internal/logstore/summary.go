package logstore

import (
	"regexp"
	"strings"
)

// Summary is a quick read of a job log: where the first error is and how
// the log ends.
type Summary struct {
	// FirstError is the first line that looks like an error (1-based Line;
	// 0 when none was found).
	FirstError     string `json:"first_error,omitempty"`
	FirstErrorLine int    `json:"first_error_line"`
	// Context is a few lines around FirstError.
	Context []string `json:"context,omitempty"`
	// Tail is the last lines of the log (what was printed before the exit).
	Tail  []string `json:"tail"`
	Lines int      `json:"lines"`
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// errorRe matches the usual shapes of a failure line across toolchains.
	errorRe = regexp.MustCompile(`(?i)(^|[\s\[(:])(error|errors?:|fatal|failed|failure|panic:|exception|traceback|segmentation fault|cannot find|not found|denied|exit status [1-9]|exit code [1-9]|E[0-9]{3,}\b|npm ERR!|--- FAIL)`)
	// noiseRe filters lines that mention errors without being one.
	noiseRe = regexp.MustCompile(`(?i)(\b0 (errors?|failures?|failed)\b|no errors|errors?: 0\b|failed: 0\b|-Werror|error_reporting|ignore[-_ ]errors?|set -e\b|if .*error|onerror|\berror\.(go|ts|js|py|rb|java)\b)`)
)

const (
	contextBefore = 2
	contextAfter  = 4
	tailLines     = 30
)

// Summarize finds the first error-looking line and the tail of a log. It is
// a heuristic: good enough to jump straight to the failure, never authoritative.
func Summarize(log string) Summary {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	clean := make([]string, len(lines))
	for i, l := range lines {
		clean[i] = strings.TrimRight(ansiRe.ReplaceAllString(l, ""), "\r")
	}
	s := Summary{Lines: len(clean), Tail: []string{}}
	for i, l := range clean {
		// Skip the runner's own preamble lines and pure noise.
		if strings.HasPrefix(l, "Running job #") || noiseRe.MatchString(l) {
			continue
		}
		if errorRe.MatchString(l) {
			s.FirstError = strings.TrimSpace(l)
			s.FirstErrorLine = i + 1
			from := max(0, i-contextBefore)
			to := min(len(clean), i+contextAfter+1)
			s.Context = append([]string{}, clean[from:to]...)
			break
		}
	}
	from := max(0, len(clean)-tailLines)
	s.Tail = append(s.Tail, clean[from:]...)
	return s
}
