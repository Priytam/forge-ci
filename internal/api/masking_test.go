package api

import (
	"strings"
	"testing"
)

// feed streams chunks through maskChunkTail, carrying the shared tail between
// chunks (as the log tier does across replicas), then flushes the final tail
// masked — reproducing what the store would persist across a job's lifetime.
func feed(masked []string, chunks ...string) string {
	var out strings.Builder
	tail := ""
	for _, c := range chunks {
		emit, keep := maskChunkTail(tail, c, masked)
		out.WriteString(emit)
		tail = keep
	}
	// Flush the remaining tail as the complete path (flushMaskTail) would.
	out.WriteString(maskAll(tail, masked))
	return out.String()
}

func TestMaskChunkAcrossBoundaries(t *testing.T) {
	const secret = "SUPERSECRETVALUE123"
	masked := []string{secret}

	// Split at every interior position; the reassembled, masked output must
	// never contain the raw secret and must contain the mask token.
	for split := 1; split < len(secret); split++ {
		got := feed(masked,
			"prefix ", secret[:split], secret[split:], " suffix")
		if strings.Contains(got, secret) {
			t.Fatalf("split=%d: raw secret leaked in %q", split, got)
		}
		if !strings.Contains(got, "[MASKED]") {
			t.Fatalf("split=%d: expected [MASKED] in %q", split, got)
		}
		if !strings.HasPrefix(got, "prefix ") || !strings.HasSuffix(got, " suffix") {
			t.Fatalf("split=%d: surrounding text lost: %q", split, got)
		}
	}
}

func TestMaskChunkNoSecretsPassThrough(t *testing.T) {
	got := feed(nil, "hello ", "world")
	if got != "hello world" {
		t.Fatalf("no-secret text should pass through unchanged, got %q", got)
	}
}

func TestMaskChunkOneChunk(t *testing.T) {
	const secret = "abcd1234efgh"
	got := feed([]string{secret}, "x "+secret+" y")
	if strings.Contains(got, secret) || !strings.Contains(got, "[MASKED]") {
		t.Fatalf("single-chunk masking failed: %q", got)
	}
}

func TestMaskChunkAdjacentSecretsAcrossBoundary(t *testing.T) {
	// Two different secrets, each split, appearing back to back.
	a, b := "AAAAAAAAAA", "BBBBBBBBBB"
	got := feed([]string{a, b}, "start "+a[:4], a[4:]+b[:6], b[6:]+" end")
	if strings.Contains(got, a) || strings.Contains(got, b) {
		t.Fatalf("a secret leaked: %q", got)
	}
	if strings.Count(got, "[MASKED]") != 2 {
		t.Fatalf("expected 2 masked tokens, got %q", got)
	}
}
