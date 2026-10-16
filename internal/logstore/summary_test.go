package logstore

import "testing"

func TestSummarizeFindsFirstError(t *testing.T) {
	cases := []struct {
		name, log, want string
		line            int
	}{
		{"go test", "Running job #4 \"unit\"\n$ go test ./...\nok  pkg/a 0.1s\n--- FAIL: TestCheckout (0.01s)\n    checkout_test.go:42: got 3 want 4\nFAIL\n", "--- FAIL: TestCheckout (0.01s)", 4},
		{"npm", "> build\nadded 120 packages\nnpm ERR! code ELIFECYCLE\nnpm ERR! errno 1\n", "npm ERR! code ELIFECYCLE", 3},
		{"noise first", "compiling with -Werror\n0 errors, 0 warnings\nall good\nError: connect ECONNREFUSED 10.0.0.5:5432\n", "Error: connect ECONNREFUSED 10.0.0.5:5432", 4},
		{"ansi", "\x1b[31mERROR\x1b[0m deploy failed: timeout\n", "ERROR deploy failed: timeout", 1},
		{"python", "Traceback (most recent call last):\n  File \"x.py\"\nValueError: bad\n", "Traceback (most recent call last):", 1},
		{"clean", "building\ndone\n", "", 0},
	}
	for _, c := range cases {
		s := Summarize(c.log)
		if s.FirstError != c.want || s.FirstErrorLine != c.line {
			t.Errorf("%s: first error = %q (line %d), want %q (line %d)", c.name, s.FirstError, s.FirstErrorLine, c.want, c.line)
		}
	}
	s := Summarize("a\nb\nc\n")
	if s.Lines != 3 || len(s.Tail) != 3 || s.Tail[2] != "c" {
		t.Errorf("tail = %+v", s)
	}
	if s := Summarize(""); s.Lines != 0 || len(s.Tail) != 0 {
		t.Errorf("empty log summary = %+v", s)
	}
}
