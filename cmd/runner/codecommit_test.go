package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// captureStreamer builds a logStreamer whose flushes land in a buffer, without
// starting the background ticker (the test drives flush directly).
func captureStreamer(t *testing.T, seed []string) (*logStreamer, func() string) {
	t.Helper()
	var mu sync.Mutex
	var got strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got.Write(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ls := &logStreamer{
		c:      &client{base: srv.URL, http: srv.Client()},
		jobID:  1,
		masked: seed,
		done:   make(chan struct{}),
	}
	return ls, func() string {
		mu.Lock()
		defer mu.Unlock()
		return got.String()
	}
}

// A credential that only exists once the job is running — a CodeCommit clone
// signature — must be masked from the moment it is registered.
func TestLogStreamerRedactMasksRuntimeSecrets(t *testing.T) {
	const sig = "20260904T103000Zdeadbeefcafef00d"
	ls, captured := captureStreamer(t, nil)

	ls.redact(sig)
	ls.printf("remote: https://AKIA:%s@git-codecommit.ap-south-1.amazonaws.com/v1/repos/x\n", sig)
	ls.flush()

	out := captured()
	if strings.Contains(out, sig) {
		t.Errorf("signature reached the log: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected a redaction marker, got %q", out)
	}
}

// The seeded RedactValues must keep working alongside values added later.
func TestLogStreamerRedactKeepsSeededValues(t *testing.T) {
	ls, captured := captureStreamer(t, []string{"seeded-token"})
	ls.redact("runtime-secret")
	ls.printf("a seeded-token and a runtime-secret\n")
	ls.flush()

	out := captured()
	if strings.Contains(out, "seeded-token") || strings.Contains(out, "runtime-secret") {
		t.Errorf("a secret reached the log: %q", out)
	}
}

// Empty values must not be registered: masking "" would replace every empty
// string in the log and destroy the output.
func TestLogStreamerRedactIgnoresEmpty(t *testing.T) {
	ls, captured := captureStreamer(t, nil)
	ls.redact("", "real")
	ls.printf("keep this\n")
	ls.flush()

	if out := captured(); out != "keep this\n" {
		t.Errorf("log = %q, want it untouched", out)
	}
}

// redact() appends to the same slice flush() reads. Run under -race, this
// asserts the two are actually synchronised.
func TestLogStreamerRedactIsRaceFree(t *testing.T) {
	ls, _ := captureStreamer(t, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); ls.redact("secret") }()
		go func() {
			defer wg.Done()
			ls.printf("line %d\n", i)
			ls.flush()
		}()
	}
	wg.Wait()
}

// A job whose clone URL is not a CodeCommit URL must fail loudly rather than
// attempt to sign something meaningless.
func TestSignedCodeCommitRemoteRejectsNonCodeCommitURL(t *testing.T) {
	ls, _ := captureStreamer(t, nil)
	job := &proto.RunnerJob{
		CloneAuth: proto.CloneAuthAWSSigV4,
		CloneURL:  "https://github.com/acme/widgets.git",
	}
	_, err := signedCodeCommitRemote(context.Background(), ls, job)
	if err == nil {
		t.Fatal("expected an error for a non-CodeCommit clone URL")
	}
	if !strings.Contains(err.Error(), "not a CodeCommit URL") {
		t.Errorf("err = %v", err)
	}
}
