package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func fastReportRetries(t *testing.T) {
	t.Helper()
	oldT, oldFor, oldMin, oldMax := reportTimeout, reportRetryFor, reportBackoffMin, reportBackoffMax
	reportTimeout, reportRetryFor, reportBackoffMin, reportBackoffMax = 200*time.Millisecond, 10*time.Second, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		reportTimeout, reportRetryFor, reportBackoffMin, reportBackoffMax = oldT, oldFor, oldMin, oldMax
	})
}

// TestCompletionReportRetries: a completion POST that times out, then gets a
// 503, is retried until it lands, carrying the claim (attempt, runner id).
func TestCompletionReportRetries(t *testing.T) {
	fastReportRetries(t)
	var calls atomic.Int32
	var got proto.CompleteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			time.Sleep(400 * time.Millisecond) // past reportTimeout
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
	}))
	defer srv.Close()

	c := &client{base: srv.URL, http: srv.Client()}
	job := &proto.RunnerJob{ID: 5, Attempt: 2, RunnerID: "r-1"}
	reportCompletion(c, job, proto.CompleteRequest{Status: "success", Attempt: job.Attempt, RunnerID: job.RunnerID})
	if n := calls.Load(); n != 3 {
		t.Fatalf("calls=%d, want 3 (timeout, 503, success)", n)
	}
	if got.Status != "success" || got.Attempt != 2 || got.RunnerID != "r-1" {
		t.Fatalf("report = %+v", got)
	}
}

// TestCompletionReportStopsOnConflict: 409 means the report already landed
// (or the job is no longer this claim's); the runner stops retrying.
func TestCompletionReportStopsOnConflict(t *testing.T) {
	fastReportRetries(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	reportCompletion(&client{base: srv.URL, http: srv.Client()}, &proto.RunnerJob{ID: 5}, proto.CompleteRequest{Status: "failed"})
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls=%d, want 1", n)
	}
}

// TestLogFlushResendsWithSameSequence: chunks that fail are kept and resent in
// order with their original sequence number, so the server can drop a resend
// of a chunk it already stored.
func TestLogFlushResendsWithSameSequence(t *testing.T) {
	fastReportRetries(t)
	var mu sync.Mutex
	var seqs []int64
	stored := ""
	last := int64(0)
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq, _ := strconv.ParseInt(r.Header.Get(proto.LogSeqHeader), 10, 64)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		seqs = append(seqs, seq)
		if seq > last { // what the server's AcceptLogChunk does
			last = seq
			stored += string(body)
		}
		if fail.Load() {
			// Stored, but the runner never hears back: it must resend.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ls := &logStreamer{c: &client{base: srv.URL, http: srv.Client()}, jobID: 9, done: make(chan struct{})}
	ls.printf("one\n")
	if ls.flush() {
		t.Fatal("flush reported success on a 502")
	}
	ls.printf("two\n")
	fail.Store(false)
	if !ls.flush() {
		t.Fatal("flush failed after the server recovered")
	}
	mu.Lock()
	defer mu.Unlock()
	if stored != "one\ntwo\n" {
		t.Fatalf("stored %q, want each line once in order", stored)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 1 || seqs[2] != 2 {
		t.Fatalf("sequence numbers sent = %v, want [1 1 2]", seqs)
	}
}
