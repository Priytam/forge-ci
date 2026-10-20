package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func acquireRaw(t *testing.T, srv *Server, runner string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(proto.AcquireRequest{RunnerID: runner, Executor: "shell", Version: "1.0.0"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/acquire", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestDrainAndMaintenanceStopAcquisition: a draining runner, and every
// runner during maintenance, gets no job — with headers saying why — and
// resumes afterwards.
func TestDrainAndMaintenanceStopAcquisition(t *testing.T) {
	t.Setenv("ACQUIRE_LONG_POLL", "1s")
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	ctx := context.Background()
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n  b:\n    stage: b\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	_, _ = h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	_, _ = h.st.PromoteReadyJobs(ctx)

	if rec := acquireRaw(t, h.srv, "r1"); rec.Code != 200 {
		t.Fatalf("first acquire = %d", rec.Code)
	}
	h.expect(h.do(admin, "POST", "/api/v1/runners/r1/drain", nil), 200, "")
	rec := acquireRaw(t, h.srv, "r1")
	if rec.Code != 204 || rec.Header().Get("X-Forge-Runner-Action") != "drain" {
		t.Errorf("draining acquire = %d action %q", rec.Code, rec.Header().Get("X-Forge-Runner-Action"))
	}
	var list []proto.Runner
	_ = json.Unmarshal(h.do(admin, "GET", "/api/v1/runners", nil).Body.Bytes(), &list)
	if len(list) != 1 || !list[0].Draining || list[0].RunningJobs != 1 || list[0].Version != "1.0.0" {
		t.Errorf("runner row = %+v", list)
	}
	h.expect(h.do(admin, "POST", "/api/v1/runners/r1/resume", nil), 200, "")
	if rec := acquireRaw(t, h.srv, "r1"); rec.Code != 200 {
		t.Errorf("after resume = %d", rec.Code)
	}
	// Maintenance: everyone waits.
	h.expect(h.do(admin, "PUT", "/api/v1/maintenance", map[string]any{"on": true, "message": "back soon"}), 200, "")
	_, _ = h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s2", Config: yml}, jobs, nil, false, false)
	_, _ = h.st.PromoteReadyJobs(ctx)
	if rec := acquireRaw(t, h.srv, "r2"); rec.Code != 204 || rec.Header().Get("X-Forge-Maintenance") != "on" {
		t.Errorf("maintenance acquire = %d %q", rec.Code, rec.Header().Get("X-Forge-Maintenance"))
	}
	// Public status says so, without a session.
	st := httptest.NewRecorder()
	h.srv.ServeHTTP(st, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if st.Code != 200 || !strings.Contains(st.Body.String(), `"on":true`) || !strings.Contains(st.Body.String(), "back soon") {
		t.Errorf("status = %d %s", st.Code, st.Body.String())
	}
	h.expect(h.do(admin, "PUT", "/api/v1/maintenance", map[string]any{"on": false}), 200, "")
	if rec := acquireRaw(t, h.srv, "r2"); rec.Code != 200 {
		t.Errorf("after maintenance = %d", rec.Code)
	}
	h.expect(h.do(h.login("dev@acme.io"), "PUT", "/api/v1/maintenance", map[string]any{"on": true}), 403, "")
}

// TestLostRunnerFailsOrRetries: a job whose runner stops heart-beating is
// failed with a reason, or re-queued when retry.when covers it.
func TestLostRunnerFailsOrRetries(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	yml := `stages: [b]
jobs:
  plain:
    stage: b
    script: [x]
  infra:
    stage: b
    retry: {max: 1, when: [runner_system_failure]}
    script: [x]
  scripty:
    stage: b
    retry: {max: 1, when: [script_failure]}
    script: [x]
`
	jobs, err := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	_, _ = h.st.PromoteReadyJobs(ctx)
	for i := 0; i < 3; i++ {
		if rj, _ := h.st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "ghost"}); rj == nil {
			t.Fatal("expected a job")
		}
	}
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes' WHERE pipeline_id=$1`, p.ID)
	if n, err := h.st.FailStaleJobs(ctx, 90*time.Second); err != nil || n != 3 {
		t.Fatalf("stale = %d %v", n, err)
	}
	_, js, _ := h.st.GetPipeline(ctx, p.ID)
	by := map[string]proto.Job{}
	for _, j := range js {
		by[j.Name] = j
	}
	if by["plain"].Status != "failed" || !strings.Contains(by["plain"].StatusReason, "runner lost") || !strings.Contains(by["plain"].StatusReason, "ghost") {
		t.Errorf("plain = %s %q", by["plain"].Status, by["plain"].StatusReason)
	}
	if by["scripty"].Status != "failed" {
		t.Errorf("scripty (script_failure only) = %s, want failed", by["scripty"].Status)
	}
	if by["infra"].Status != "pending" || !strings.Contains(by["infra"].StatusReason, "retrying") {
		t.Errorf("infra = %s %q, want pending (retried)", by["infra"].Status, by["infra"].StatusReason)
	}
	// A script failure on the infra-only job is not retried.
	rj, _ := h.st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r"})
	if rj == nil || rj.Name != "infra" {
		t.Fatalf("acquire = %+v", rj)
	}
	if final, _, _ := h.st.CompleteJob(ctx, rj.ID, "failed", 1); final != "failed" {
		t.Errorf("infra script failure = %s, want failed (no script retry)", final)
	}
	// The failed-pipeline event names the reason.
	events, _ := h.st.ClaimPipelineEvents(ctx, 10)
	if len(events) != 1 || !strings.Contains(strings.Join(events[0].FailedJobs, ","), "plain (runner lost") {
		t.Errorf("event = %+v", events)
	}
}

func TestRolloutAdvancesOneRunnerAtATime(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	for _, r := range []struct{ id, v string }{{"a", "1.0.0"}, {"b", "1.0.0"}, {"c", "2.0.0"}} {
		if _, err := h.st.TouchRunnerV(ctx, r.id, "shell", nil, r.v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.st.StartRollout(ctx, "2.0.0", "admin"); err != nil {
		t.Fatal(err)
	}
	first, err := h.st.AdvanceRollout(ctx, time.Minute)
	if err != nil || first == "" || first == "c" {
		t.Fatalf("first = %q %v", first, err)
	}
	if st, _ := h.st.RunnerState(ctx, first); !st.Draining || !st.UpdateRequested {
		t.Errorf("first runner state = %+v", st)
	}
	// Still at the old version: nothing else is touched.
	if again, _ := h.st.AdvanceRollout(ctx, time.Minute); again != first {
		t.Errorf("second pass moved on to %q", again)
	}
	// It comes back upgraded: request cleared, drain lifted, next runner picked.
	if _, err := h.st.TouchRunnerV(ctx, first, "shell", nil, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.st.RunnerState(ctx, first); st.Draining || st.UpdateRequested {
		t.Errorf("after upgrade state = %+v", st)
	}
	second, _ := h.st.AdvanceRollout(ctx, time.Minute)
	if second == "" || second == first || second == "c" {
		t.Errorf("second = %q", second)
	}
	_, _ = h.st.TouchRunnerV(ctx, second, "shell", nil, "2.0.0")
	if last, _ := h.st.AdvanceRollout(ctx, time.Minute); last != "" {
		t.Errorf("rollout should be complete, got %q", last)
	}
	ro, _ := h.st.Rollout(ctx)
	if ro.Active || len(ro.Done) != 2 || ro.Note != "complete" {
		t.Errorf("rollout = %+v", ro)
	}
	_ = store.ErrNotFound
}
