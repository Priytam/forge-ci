//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- black-box JSON shapes (defined locally so the suite stays HTTP-only) ----

type pipeline struct {
	ID            int64         `json:"id"`
	Repo          string        `json:"repo"`
	Ref           string        `json:"ref"`
	SHA           string        `json:"sha"`
	Status        string        `json:"status"`
	Stages        []stageStatus `json:"stages"`
	ConfigVersion *int          `json:"config_version"`
	CreatedAt     time.Time     `json:"created_at"`
}

type stageStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type job struct {
	ID          int64   `json:"id"`
	PipelineID  int64   `json:"pipeline_id"`
	Name        string  `json:"name"`
	Stage       string  `json:"stage"`
	Image       *string `json:"image"`
	Environment *string `json:"environment"`
	Status      string  `json:"status"`
	ExitCode    *int    `json:"exit_code"`
	Needs       []int64 `json:"needs"`
}

type logDelta struct {
	Bytes      string `json:"bytes"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
}

// ---- low-level HTTP ----

// doJSON performs a request with an optional JSON body and returns the status
// code and raw body. It never fails the test itself — callers assert on status.
func doJSON(t *testing.T, method, urlStr string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, urlStr, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, urlStr, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, urlStr, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// mustJSON is doJSON asserting an expected status code, decoding into out if set.
func mustJSON(t *testing.T, method, urlStr string, body, out any, want int) {
	t.Helper()
	code, raw := doJSON(t, method, urlStr, body)
	if code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, urlStr, code, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode response: %v body=%s", method, urlStr, err, raw)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// ---- high-level API operations (bound to a baseURL) ----

func createPipeline(t *testing.T, baseURL, repo, ref, sha, config string) pipeline {
	t.Helper()
	var resp struct {
		Pipeline pipeline `json:"pipeline"`
	}
	mustJSON(t, http.MethodPost, baseURL+"/api/v1/pipelines", map[string]string{
		"repo":   repo,
		"ref":    ref,
		"sha":    sha,
		"config": config,
	}, &resp, http.StatusCreated)
	if resp.Pipeline.ID == 0 {
		t.Fatalf("createPipeline: got zero pipeline id")
	}
	return resp.Pipeline
}

func getPipeline(t *testing.T, baseURL string, id int64) (pipeline, []job) {
	t.Helper()
	var resp struct {
		Pipeline pipeline `json:"pipeline"`
		Jobs     []job    `json:"jobs"`
	}
	mustJSON(t, http.MethodGet, fmt.Sprintf("%s/api/v1/pipelines/%d", baseURL, id), nil, &resp, http.StatusOK)
	return resp.Pipeline, resp.Jobs
}

func jobByName(jobs []job, name string) (job, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return job{}, false
}

func jobNames(jobs []job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// getJobLogsFull returns the full masked log body (text/plain path).
func getJobLogsFull(t *testing.T, baseURL string, jobID int64) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/jobs/%d/logs", baseURL, jobID), nil)
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("get logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get logs: status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// getJobLogsDelta returns the incremental delta from offset.
func getJobLogsDelta(t *testing.T, baseURL string, jobID, offset int64) logDelta {
	t.Helper()
	var d logDelta
	mustJSON(t, http.MethodGet,
		fmt.Sprintf("%s/api/v1/jobs/%d/logs?offset=%d", baseURL, jobID, offset), nil, &d, http.StatusOK)
	return d
}

// ---- polling ----

// pollPipeline polls the pipeline until pred(pipeline, jobs) is true or the
// deadline passes. On timeout it fails with the last observed state.
func pollPipeline(t *testing.T, baseURL string, id int64, timeout time.Duration, pred func(pipeline, []job) bool) (pipeline, []job) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var p pipeline
	var jobs []job
	for time.Now().Before(deadline) {
		p, jobs = getPipeline(t, baseURL, id)
		if pred(p, jobs) {
			return p, jobs
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("pollPipeline %d: condition not met within %s; last status=%q jobs=%s",
		id, timeout, p.Status, describeJobs(jobs))
	return p, jobs
}

func describeJobs(jobs []job) string {
	var sb strings.Builder
	for i, j := range jobs {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s=%s", j.Name, j.Status)
	}
	return "[" + sb.String() + "]"
}

// pipelineStatusIs returns a predicate matching one of the given pipeline statuses.
func pipelineStatusIn(statuses ...string) func(pipeline, []job) bool {
	set := map[string]bool{}
	for _, s := range statuses {
		set[s] = true
	}
	return func(p pipeline, _ []job) bool { return set[p.Status] }
}

// jobStatusIs returns a predicate that a named job has one of the given statuses.
func jobStatusIn(name string, statuses ...string) func(pipeline, []job) bool {
	set := map[string]bool{}
	for _, s := range statuses {
		set[s] = true
	}
	return func(_ pipeline, jobs []job) bool {
		j, ok := jobByName(jobs, name)
		return ok && set[j.Status]
	}
}

// waitJob polls the pipeline until the named job reaches one of statuses.
func waitJob(t *testing.T, baseURL string, pid int64, name string, timeout time.Duration, statuses ...string) job {
	t.Helper()
	_, jobs := pollPipeline(t, baseURL, pid, timeout, jobStatusIn(name, statuses...))
	j, _ := jobByName(jobs, name)
	return j
}

// ---- SSE ----

// sseCollect connects to the SSE stream for a job and collects "log" event
// payloads until an "eof" event, the deadline, or a read error. It returns the
// concatenated bytes and whether an eof event was seen.
func sseCollect(t *testing.T, baseURL string, jobID int64, timeout time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/jobs/%d/logs/stream", baseURL, jobID), nil)
	// A dedicated client with no overall timeout — the context bounds it.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var out strings.Builder
	sawEOF := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var event string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if event == "log" {
				var d logDelta
				if json.Unmarshal([]byte(data), &d) == nil {
					out.WriteString(d.Bytes)
				}
			} else if event == "eof" {
				sawEOF = true
				return out.String(), sawEOF
			}
		}
	}
	return out.String(), sawEOF
}
