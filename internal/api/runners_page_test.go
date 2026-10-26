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

// Runners page (batch 8): the bugs behind the redesign.

// acquireWith registers (or polls as) a runner; an empty queue answers 204
// after a short long-poll.
func acquireWith(t *testing.T, srv *Server, req proto.AcquireRequest) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("ACQUIRE_LONG_POLL", "10ms")
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/runner/acquire", bytes.NewReader(body)))
	return rec
}

func listRunnersAs(t *testing.T, h *apiHarness, c testCaller) map[string]proto.Runner {
	t.Helper()
	rec := h.do(c, "GET", "/api/v1/runners", nil)
	h.expect(rec, 200, "list runners")
	var list []proto.Runner
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]proto.Runner{}
	for _, r := range list {
		out[r.ID] = r
	}
	return out
}

// TestAuditLogRunnerActionsHaveTimestamps: bug 3 — "who paused the runner
// and when" must be answerable: runner actions carry created_at (and ts),
// and the log filters by runner and action prefix.
func TestAuditLogRunnerActionsHaveTimestamps(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	if rec := acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "builder-2", Executor: "docker"}); rec.Code != 204 {
		t.Fatalf("register runner = %d", rec.Code)
	}
	before := time.Now().Add(-time.Minute)
	h.expect(h.do(admin, "POST", "/api/v1/runners/builder-2/pause", map[string]any{"paused": true}), 200, "pause")
	h.expect(h.do(admin, "POST", "/api/v1/runners/builder-2/drain", nil), 200, "drain")
	h.expect(h.do(admin, "PUT", "/api/v1/maintenance", map[string]any{"on": false}), 200, "maintenance")

	rec := h.do(admin, "GET", "/api/v1/audit-log?target=builder-2&action=runner.", nil)
	h.expect(rec, 200, "audit log")
	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("runner entries = %d, want 2 (pause, drain): %s", len(raw), rec.Body.String())
	}
	for _, e := range raw {
		created, ok := e["created_at"].(string)
		if !ok || created == "" {
			t.Fatalf("created_at missing or null: %v", e)
		}
		at, err := time.Parse(time.RFC3339Nano, created)
		if err != nil || at.Before(before) || at.After(time.Now().Add(time.Minute)) {
			t.Errorf("created_at = %q (%v)", created, err)
		}
		if e["ts"] != created {
			t.Errorf("ts %v != created_at %v", e["ts"], created)
		}
		if e["actor"] != "admin@acme.io" || e["target"] != "builder-2" {
			t.Errorf("entry = %v", e)
		}
	}
	if raw[0]["action"] != "runner.drain" || raw[1]["action"] != "runner.pause" {
		t.Errorf("actions = %v, %v (newest first)", raw[0]["action"], raw[1]["action"])
	}
}

// TestRunnerLoadFollowsRange: bug 4 — the table's per-runner numbers cover
// the range the page picked, not a fixed 24h.
func TestRunnerLoadFollowsRange(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n  b:\n    stage: b\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "r1", Executor: "shell"})
	// a: three days ago, ran 10 minutes. b: two hours ago, ran 1 minute.
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='success', runner_id='r1', queued_at=now()-interval '3 days', started_at=now()-interval '3 days', finished_at=now()-interval '3 days'+interval '600 seconds' WHERE pipeline_id=$1 AND name='a'`, p.ID)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='success', runner_id='r1', queued_at=now()-interval '2 hours', started_at=now()-interval '2 hours', finished_at=now()-interval '2 hours'+interval '60 seconds' WHERE pipeline_id=$1 AND name='b'`, p.ID)

	load := func(q string) store.RunnerLoadRow {
		t.Helper()
		rec := h.do(admin, "GET", "/api/v1/runner-load"+q, nil)
		h.expect(rec, 200, "runner load "+q)
		var l store.RunnerLoad
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		for _, r := range l.Runners {
			if r.ID == "r1" {
				return r
			}
		}
		t.Fatalf("r1 missing from %s", rec.Body.String())
		return store.RunnerLoadRow{}
	}
	if r := load("?range=24h"); r.Jobs != 1 || r.BusySeconds != 60 {
		t.Errorf("24h: jobs %d busy %d, want 1 / 60", r.Jobs, r.BusySeconds)
	}
	if r := load("?range=7d"); r.Jobs != 2 || r.BusySeconds != 660 {
		t.Errorf("7d: jobs %d busy %d, want 2 / 660", r.Jobs, r.BusySeconds)
	}
	if r := load(""); r.Jobs != 1 {
		t.Errorf("default range: jobs %d, want 1 (24h)", r.Jobs)
	}
	from := time.Now().Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(-2 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if r := load("?range=custom&from=" + from + "&to=" + to); r.Jobs != 1 || r.BusySeconds != 600 {
		t.Errorf("custom: jobs %d busy %d, want 1 / 600", r.Jobs, r.BusySeconds)
	}
	h.expect(h.do(admin, "GET", "/api/v1/runner-load?range=1y", nil), 400, "bad range")
}

// TestRunnerHistoryGapsAreNotZeros: bug 5 — an hour with no jobs has no
// wait or duration (null), a running-only hour has a wait but no duration,
// and hours the rollups don't cover are marked no_data.
func TestRunnerHistoryGapsAreNotZeros(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n  b:\n    stage: b\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	done := time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Hour)
	running := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='success', runner_id='r1', queued_at=$2::timestamptz, started_at=$2::timestamptz + interval '20 seconds', finished_at=$2::timestamptz + interval '80 seconds' WHERE pipeline_id=$1 AND name='a'`, p.ID, done)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='running', runner_id='r1', queued_at=$2::timestamptz, started_at=$2::timestamptz + interval '5 seconds' WHERE pipeline_id=$1 AND name='b'`, p.ID, running)

	rg, _ := store.ParseRange("24h", "", "", "", time.Now())
	buckets, sum, err := h.st.RunnerHistory(ctx, rg, store.HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byAt := map[time.Time]store.HistoryBucket{}
	for _, b := range buckets {
		byAt[b.At] = b
		if b.NoData {
			t.Errorf("live range bucket %v marked no_data", b.At)
		}
	}
	if b := byAt[done]; fv(b.WaitP50) != 20 || fv(b.DurationP50) != 60 {
		t.Errorf("finished bucket = %+v", b)
	}
	if b := byAt[running]; fv(b.WaitP50) != 5 || b.DurationP50 != nil || b.JobsStarted != 1 {
		t.Errorf("running-only bucket: wait %v duration %v (want 5, null)", fv(b.WaitP50), fv(b.DurationP50))
	}
	if b := byAt[done.Add(time.Hour)]; b.JobsStarted != 0 || b.WaitP50 != nil || b.WaitP95 != nil || b.DurationP95 != nil {
		t.Errorf("empty bucket = %+v (want zero jobs, null percentiles)", b)
	}
	if sum.JobsStarted != 2 || sum.Finished != 1 || fv(sum.DurationP50) != 60 {
		t.Errorf("summary = %+v", sum)
	}

	// Rollups: only the last day is rolled up, so the rest of a 30-day
	// range is no_data, while an empty rolled-up hour is a real zero.
	if _, err := h.st.RollupHour(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	rg30, _ := store.ParseRange("30d", "", "", "1h", time.Now())
	rolled, rsum, err := h.st.RunnerHistory(ctx, rg30, store.HistoryFilter{Runner: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var noData, covered int
	for _, b := range rolled {
		switch {
		case b.NoData:
			noData++
			if b.JobsStarted != 0 || b.WaitP50 != nil {
				t.Errorf("no_data bucket carries values: %+v", b)
			}
		case b.At.Equal(done):
			if fv(b.WaitP50) != 20 || fv(b.DurationP50) != 60 {
				t.Errorf("rolled-up bucket = %+v", b)
			}
			covered++
		default:
			if b.WaitP50 != nil && b.JobsStarted == 0 {
				t.Errorf("empty rolled-up hour has a wait: %+v", b)
			}
			covered++
		}
	}
	if noData < 28*24 || covered < 20 {
		t.Errorf("no_data %d, covered %d (want most of 30 days missing, the last day covered)", noData, covered)
	}
	if fv(rsum.WaitP50) < 0 || rsum.JobsStarted < 1 {
		t.Errorf("rollup summary = %+v", rsum)
	}

	// The API spells a gap as null, not 0.
	rec := h.do(h.login("dev@acme.io"), "GET", "/api/v1/runner-history?range=24h", nil)
	h.expect(rec, 200, "history")
	if !strings.Contains(rec.Body.String(), `"wait_p50":null`) || !strings.Contains(rec.Body.String(), `"summary":{`) {
		t.Errorf("history JSON lacks null gaps or summary: %.300s", rec.Body.String())
	}
}

// TestStaleRunnersAndRemoval: bug 6 — a runner unseen for a day is stale
// (left out of tag totals by the page) and an admin can remove it; an online
// runner can't be removed.
func TestStaleRunnersAndRemoval(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	for _, id := range []string{"gce-runner-dev", "gce-builder-1"} {
		_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: id, Executor: "docker", Tags: []string{"gcp"}})
	}
	h.expect(h.do(admin, "DELETE", "/api/v1/runners/gce-runner-dev", nil), 409, "remove online runner")

	_, _ = h.db.Exec(ctx, `UPDATE runners SET last_contact_at = now() - interval '12 days' WHERE id = 'gce-runner-dev'`)
	list := listRunnersAs(t, h, admin)
	if r := list["gce-runner-dev"]; !r.Stale || r.Online {
		t.Errorf("12-day-old runner = %+v, want stale", r)
	}
	if r := list["gce-builder-1"]; r.Stale || !r.Online {
		t.Errorf("live runner = %+v", r)
	}

	h.expect(h.do(h.login("dev@acme.io"), "DELETE", "/api/v1/runners/gce-runner-dev", nil), 403, "non-admin remove")
	h.expect(h.do(admin, "DELETE", "/api/v1/runners/gce-runner-dev", nil), 204, "remove stale runner")
	h.expect(h.do(admin, "DELETE", "/api/v1/runners/gce-runner-dev", nil), 404, "remove again")
	h.expect(h.do(admin, "DELETE", "/api/v1/runners/rollout", nil), 204, "rollout route still wins")
	if _, ok := listRunnersAs(t, h, admin)["gce-runner-dev"]; ok {
		t.Error("removed runner still listed")
	}
	entries, _ := h.st.ListAudit(ctx, store.AuditFilter{Target: "gce-runner-dev", Limit: 5})
	if len(entries) != 1 || entries[0].Action != "runner.remove" {
		t.Errorf("audit = %+v", entries)
	}

	// An offline runner that still has a running job (lost, not yet failed)
	// is kept.
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='running', runner_id='gce-builder-1', started_at=now() WHERE pipeline_id=$1`, p.ID)
	_, _ = h.db.Exec(ctx, `UPDATE runners SET last_contact_at = now() - interval '2 days' WHERE id = 'gce-builder-1'`)
	h.expect(h.do(admin, "DELETE", "/api/v1/runners/gce-builder-1", nil), 409, "remove busy runner")
}

// TestRunnerReportsConcurrency: capacity is slots, not runners — a runner
// reports its concurrency; older runners that don't count as one slot.
func TestRunnerReportsConcurrency(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "k8s-manager", Executor: "kubernetes", Concurrency: 4})
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "old-shell", Executor: "shell"})
	list := listRunnersAs(t, h, admin)
	if list["k8s-manager"].Concurrency != 4 || list["old-shell"].Concurrency != 1 {
		t.Errorf("concurrency = %d / %d, want 4 / 1", list["k8s-manager"].Concurrency, list["old-shell"].Concurrency)
	}
	rec := h.do(admin, "GET", "/api/v1/runner-load", nil)
	if !strings.Contains(rec.Body.String(), `"concurrency":4`) {
		t.Errorf("runner load lacks concurrency: %s", rec.Body.String())
	}
}

// TestRunnerEventsForAnnotations: runner operations and maintenance show on
// the time axis; who did it is admin-only.
func TestRunnerEventsForAnnotations(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "r1", Executor: "shell"})
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "r2", Executor: "shell"})
	h.expect(h.do(admin, "POST", "/api/v1/runners/r1/pause", map[string]any{"paused": true}), 200, "pause")
	h.expect(h.do(admin, "POST", "/api/v1/runners/r2/upgrade", nil), 200, "upgrade")
	h.expect(h.do(admin, "PUT", "/api/v1/maintenance", map[string]any{"on": true, "message": "db"}), 200, "maintenance")
	// Not a runner event.
	_ = h.st.InsertAudit(context.Background(), store.AuditEntry{Actor: "admin@acme.io", Action: "variable.set", Target: "A"})

	events := func(c testCaller, q string) []RunnerEvent {
		t.Helper()
		rec := h.do(c, "GET", "/api/v1/runner-events"+q, nil)
		h.expect(rec, 200, "events "+q)
		var body struct{ Events []RunnerEvent }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Events
	}
	all := events(admin, "?range=24h")
	var actions []string
	for _, e := range all {
		actions = append(actions, e.Action+":"+e.Target)
		if e.Actor != "admin@acme.io" || e.At.IsZero() {
			t.Errorf("admin event = %+v", e)
		}
	}
	if got := strings.Join(actions, ","); got != "runner.pause:r1,runner.upgrade:r2,maintenance.set:" {
		t.Errorf("events = %s (oldest first, runner/maintenance only)", got)
	}
	if one := events(admin, "?range=24h&runner=r1"); len(one) != 1 || one[0].Action != "runner.pause" {
		t.Errorf("r1 events = %+v", one)
	}
	for _, e := range events(h.login("dev@acme.io"), "?range=7d") {
		if e.Actor != "" {
			t.Errorf("non-admin sees actor: %+v", e)
		}
	}
	h.expect(h.do(admin, "GET", "/api/v1/runner-events?range=nope", nil), 400, "bad range")
}

// TestRunnerStreamSendsSnapshots: the page's live feed sends runners and
// range-scoped load on connect.
func TestRunnerStreamSendsSnapshots(t *testing.T) {
	h := newEnforcedHarness(t)
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "live-1", Executor: "docker", Concurrency: 2})
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/runners/stream?range=7d", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.login("dev@acme.io").cookie})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	buf := make([]byte, 64<<10)
	var got strings.Builder
	for !strings.Contains(got.String(), "\n\n") {
		n, err := res.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	frame := got.String()
	if !strings.HasPrefix(frame, "event: snapshot\ndata: ") {
		t.Fatalf("first frame = %.200q", frame)
	}
	var snap struct {
		Runners []proto.Runner
		Load    store.RunnerLoad
	}
	data := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(frame, "event: snapshot\ndata: "), "\n\n", 2)[0])
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		t.Fatalf("snapshot JSON: %v", err)
	}
	if len(snap.Runners) != 1 || snap.Runners[0].ID != "live-1" || snap.Runners[0].Concurrency != 2 {
		t.Errorf("runners = %+v", snap.Runners)
	}
	if span := snap.Load.To.Sub(snap.Load.From); span < 7*24*time.Hour-time.Minute || span > 7*24*time.Hour+time.Minute {
		t.Errorf("load range = %v, want 7d", span)
	}
}
