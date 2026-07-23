package logstore

import "testing"

func TestSliceDelta(t *testing.T) {
	full := []byte("0123456789") // 10 bytes
	cases := []struct {
		offset   int64
		terminal bool
		want     string
		next     int64
		eof      bool
	}{
		{0, false, "0123456789", 10, false}, // running: full delta, not eof
		{0, true, "0123456789", 10, true},   // terminal + drained: eof
		{4, true, "456789", 10, true},       // mid-offset to end
		{10, true, "", 10, true},            // at end, terminal: empty + eof
		{15, true, "", 10, true},            // past end clamps
		{10, false, "", 10, false},          // at end, running: empty, not eof
	}
	for i, c := range cases {
		d := sliceDelta(full, c.offset, c.terminal)
		if string(d.Bytes) != c.want || d.NextOffset != c.next || d.EOF != c.eof {
			t.Errorf("case %d (offset=%d terminal=%v): got {%q, next=%d, eof=%v}, want {%q, next=%d, eof=%v}",
				i, c.offset, c.terminal, d.Bytes, d.NextOffset, d.EOF, c.want, c.next, c.eof)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"":                                  "(empty)",
		"redis://localhost:6379/0":          "redis://localhost:6379/0",
		"redis://:secret@host:6379/0":       "redis://***@host:6379/0",
		"rediss://user:pw@cache.aws:6379/1": "rediss://***@cache.aws:6379/1",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []string{"success", "failed", "canceled"} {
		if !isTerminal(s) {
			t.Errorf("isTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"running", "pending", "blocked", "created", ""} {
		if isTerminal(s) {
			t.Errorf("isTerminal(%q) = true, want false", s)
		}
	}
}
