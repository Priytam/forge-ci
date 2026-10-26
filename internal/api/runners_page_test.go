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

// TestRunnerPools: a runner's pool is the one it reports (RUNNER_POOL, set by
// the docker provider), else the single pool whose tags equal its tags —
// the autoscaler's own rule; a tag subset or two matching pools is no pool.
func TestRunnerPools(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	for _, p := range []struct {
		name string
		tags []string
	}{{"k8s-pool", []string{"k8s", "linux"}}, {"gpu-a", []string{"gpu"}}, {"gpu-b", []string{"gpu"}}} {
		if _, err := h.db.Exec(ctx, `INSERT INTO runner_pools (name, provider, tags) VALUES ($1, 'exec', $2)`, p.name, p.tags); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []proto.AcquireRequest{
		{RunnerID: "forge-builders-1", Executor: "docker", Tags: []string{"docker"}, Pool: "builders"},
		{RunnerID: "gke-1", Executor: "kubernetes", Tags: []string{"linux", "k8s"}},
		{RunnerID: "k8s-only", Executor: "kubernetes", Tags: []string{"k8s"}},
		{RunnerID: "gpu-1", Executor: "shell", Tags: []string{"gpu"}},
		{RunnerID: "lonely", Executor: "shell"},
	} {
		_ = acquireWith(t, h.srv, r)
	}
	list := listRunnersAs(t, h, admin)
	for id, want := range map[string]struct {
		pool     string
		inferred bool
	}{
		"forge-builders-1": {"builders", false},
		"gke-1":            {"k8s-pool", true},
		"k8s-only":         {"", false},
		"gpu-1":            {"", false}, // two pools match: ambiguous
		"lonely":           {"", false},
	} {
		if r := list[id]; r.Pool != want.pool || r.PoolInferred != want.inferred {
			t.Errorf("%s: pool %q inferred %v, want %q %v", id, r.Pool, r.PoolInferred, want.pool, want.inferred)
		}
	}
	// A runner that stops reporting a pool falls back to inference.
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "forge-builders-1", Executor: "docker", Tags: []string{"k8s", "linux"}})
	if r := listRunnersAs(t, h, admin)["forge-builders-1"]; r.Pool != "k8s-pool" || !r.PoolInferred {
		t.Errorf("after dropping RUNNER_POOL: %+v", r)
	}
}

// TestCapacitySnapshots: the per-minute snapshot counts slots by state like
// the page does, is written once a minute, and feeds the history buckets and
// summary (null where no snapshot fell).
func TestCapacitySnapshots(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	for _, r := range []proto.AcquireRequest{
		{RunnerID: "busy", Executor: "kubernetes", Tags: []string{"docker"}, Concurrency: 4},
		{RunnerID: "paused", Executor: "docker", Tags: []string{"docker"}},
		{RunnerID: "draining", Executor: "docker", Tags: []string{"docker", "staging"}},
		{RunnerID: "gone", Executor: "shell", Tags: []string{"gpu"}},
		{RunnerID: "dead", Executor: "shell", Tags: []string{"old"}},
	} {
		_ = acquireWith(t, h.srv, r)
	}
	_, _ = h.db.Exec(ctx, `UPDATE runners SET paused = true WHERE id = 'paused'`)
	_, _ = h.db.Exec(ctx, `UPDATE runners SET draining = true WHERE id = 'draining'`)
	_, _ = h.db.Exec(ctx, `UPDATE runners SET last_contact_at = now() - interval '1 hour' WHERE id = 'gone'`)
	_, _ = h.db.Exec(ctx, `UPDATE runners SET last_contact_at = now() - interval '3 days' WHERE id = 'dead'`)
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n  w:\n    stage: b\n    tags: [gpu]\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='running', runner_id='busy', started_at=now() WHERE pipeline_id=$1 AND name='a'`, p.ID)
	// A trigger stamps queued_at on entering pending; backdate it after.
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='pending' WHERE pipeline_id=$1 AND name='w'`, p.ID)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET queued_at=now() - interval '3 minutes' WHERE pipeline_id=$1 AND name='w'`, p.ID)

	snap, err := h.st.CurrentCapacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := store.CapacitySnapshot{TotalSlots: 7, AvailableSlots: 4, BusySlots: 1, PausedSlots: 1, DrainingSlots: 1, OfflineSlots: 1, Pending: 1}
	if snap.TotalSlots != want.TotalSlots || snap.AvailableSlots != want.AvailableSlots || snap.BusySlots != want.BusySlots ||
		snap.PausedSlots != want.PausedSlots || snap.DrainingSlots != want.DrainingSlots || snap.OfflineSlots != want.OfflineSlots ||
		snap.Pending != 1 || snap.OldestWait < 170 || strings.Join(snap.Starved, ",") != "gpu,staging" {
		t.Errorf("snapshot = %+v", snap)
	}
	if ok, err := h.st.RecordCapacitySnapshot(ctx); !ok || err != nil {
		t.Fatalf("first record = %v %v", ok, err)
	}
	if ok, _ := h.st.RecordCapacitySnapshot(ctx); ok {
		t.Error("second record in the same minute wrote a row")
	}

	// Two hours of history four and five hours ago: 6 then 2 available slots.
	base := time.Now().UTC().Add(-5 * time.Hour).Truncate(time.Hour)
	_, _ = h.db.Exec(ctx, `INSERT INTO runner_capacity_snapshots (at, total_slots, available_slots, pending, oldest_wait_s)
		SELECT $1::timestamptz + make_interval(mins => i), 8, CASE WHEN i < 60 THEN 6 ELSE 2 END, CASE WHEN i < 60 THEN 0 ELSE 5 END, CASE WHEN i < 60 THEN 0 ELSE 400 END
		FROM generate_series(0, 119) i`, base)
	rg, _ := store.ParseRange("24h", "", "", "", time.Now())
	buckets, sum, err := h.st.RunnerHistory(ctx, rg, store.HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byAt := map[time.Time]store.HistoryBucket{}
	for _, b := range buckets {
		byAt[b.At] = b
	}
	first, second, empty := byAt[base], byAt[base.Add(time.Hour)], byAt[base.Add(2*time.Hour)]
	if fv(first.CapacityAvailable) != 6 || fv(first.CapacityTotal) != 8 || *first.QueuePendingMax != 0 {
		t.Errorf("first hour = %+v", first)
	}
	if fv(second.CapacityAvailable) != 2 || *second.QueuePendingMax != 5 || *second.QueueOldestMax != 400 {
		t.Errorf("second hour = %+v", second)
	}
	if empty.CapacityAvailable != nil || empty.QueuePendingMax != nil {
		t.Errorf("hour without snapshots = %+v (want null)", empty)
	}
	// 121 snapshots: 60 × 6 + 60 × 2 + 1 × 4 available.
	if sum.SnapshotSeconds != 121*60 || sum.AvailableSlotSeconds != (60*6+60*2+4)*60 || *sum.QueuePendingMax != 5 || *sum.QueueOldestMax != 400 {
		t.Errorf("summary = %+v", sum)
	}
	if got := fv(sum.CapacityAvailable); got < 3.99 || got > 4.01 {
		t.Errorf("avg available = %v, want ~4", got)
	}
	rec := h.do(h.login("dev@acme.io"), "GET", "/api/v1/runner-history?range=24h", nil)
	if !strings.Contains(rec.Body.String(), `"capacity_available":6`) || !strings.Contains(rec.Body.String(), `"capacity_available":null`) {
		t.Errorf("history JSON lacks capacity: %.400s", rec.Body.String())
	}
}

// TestRunnerStaleAfterIsConfigurable: RUNNER_STALE_AFTER moves the line.
func TestRunnerStaleAfterIsConfigurable(t *testing.T) {
	t.Setenv("RUNNER_STALE_AFTER", "1h")
	h := newEnforcedHarness(t)
	_ = acquireWith(t, h.srv, proto.AcquireRequest{RunnerID: "r", Executor: "shell"})
	_, _ = h.db.Exec(context.Background(), `UPDATE runners SET last_contact_at = now() - interval '2 hours' WHERE id = 'r'`)
	if r := listRunnersAs(t, h, h.login("admin@acme.io"))["r"]; !r.Stale {
		t.Errorf("2h-old runner with RUNNER_STALE_AFTER=1h: %+v", r)
	}
}
