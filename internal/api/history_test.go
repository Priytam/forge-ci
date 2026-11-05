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

// fv reads an optional measure; -1 stands for "none" (null).
func fv(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestParseRange(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in, bucket string
		span       time.Duration
		want       time.Duration
	}{
		{"", "", 24 * time.Hour, time.Hour}, {"24h", "", 24 * time.Hour, time.Hour}, {"7d", "", 7 * 24 * time.Hour, 6 * time.Hour},
		{"30d", "", 30 * 24 * time.Hour, 24 * time.Hour}, {"7d", "1d", 7 * 24 * time.Hour, 24 * time.Hour},
	} {
		r, err := store.ParseRange(c.in, "", "", c.bucket, now)
		if err != nil || r.To.Sub(r.From) != c.span || r.Bucket != c.want {
			t.Errorf("%q/%q = %v %v %v", c.in, c.bucket, r.To.Sub(r.From), r.Bucket, err)
		}
	}
	if _, err := store.ParseRange("custom", "2026-10-01T00:00:00Z", "2026-10-03T00:00:00Z", "", now); err != nil {
		t.Error(err)
	}
	for _, bad := range [][4]string{{"1y", "", "", ""}, {"custom", "x", "y", ""}, {"24h", "", "", "2h"}} {
		if _, err := store.ParseRange(bad[0], bad[1], bad[2], bad[3], now); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// TestRunnerHistoryLiveAndRollups: wait and duration percentiles per bucket
// from jobs, then the same numbers from hourly rollups for a long range.
func TestRunnerHistoryLiveAndRollups(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    script: [x]\n  b:\n    stage: b\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	// Two jobs on runner r1 three hours ago: queued → started after 30s / 90s, ran 60s / 600s.
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='success', runner_id='r1', queued_at=$2::timestamptz, started_at=$2::timestamptz + interval '30 seconds', finished_at=$2::timestamptz + interval '90 seconds' WHERE pipeline_id=$1 AND name='a'`, p.ID, base)
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='failed', runner_id='r1', queued_at=$2::timestamptz, started_at=$2::timestamptz + interval '90 seconds', finished_at=$2::timestamptz + interval '690 seconds' WHERE pipeline_id=$1 AND name='b'`, p.ID, base)
	rg, _ := store.ParseRange("24h", "", "", "", time.Now())
	buckets, _, err := h.st.RunnerHistory(ctx, rg, store.HistoryFilter{Runner: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) < 23 {
		t.Fatalf("dense series expected, got %d buckets", len(buckets))
	}
	var hit *store.HistoryBucket
	for i := range buckets {
		if buckets[i].JobsStarted > 0 {
			hit = &buckets[i]
		}
	}
	if hit == nil || hit.JobsStarted != 2 || fv(hit.WaitP50) != 60 || fv(hit.WaitP95) < 85 || fv(hit.DurationP95) < 570 || hit.Failed != 1 || hit.BusySeconds != 660 {
		t.Errorf("bucket = %+v", hit)
	}
	// Rollups cover the same hour; a 30-day range reads them.
	if _, err := h.st.RollupHour(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	rg30, _ := store.ParseRange("30d", "", "", "1h", time.Now())
	rolled, _, err := h.st.RunnerHistory(ctx, rg30, store.HistoryFilter{Runner: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var again *store.HistoryBucket
	for i := range rolled {
		if rolled[i].JobsStarted > 0 {
			again = &rolled[i]
		}
	}
	if again == nil || again.JobsStarted != 2 || fv(again.WaitP50) != 60 || again.BusySeconds != 660 {
		t.Errorf("rollup bucket = %+v", again)
	}
	// Drill-down lists the jobs behind the point; the API serves both.
	rows, _ := h.st.JobsInBucket(ctx, hit.At, hit.At.Add(rg.Bucket), store.HistoryFilter{Runner: "r1"}, 10)
	if len(rows) != 2 {
		t.Errorf("jobs in bucket = %d", len(rows))
	}
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "GET", "/api/v1/runner-history?range=7d&runner=r1", nil), 200, "")
	rec := h.do(admin, "GET", "/api/v1/stats?range=7d", nil)
	h.expect(rec, 200, "")
	var st store.DashboardStats
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Range.Label != "7d" || st.Range.BucketSeconds != 6*3600 || st.Totals.Pipelines != 1 || st.Totals.SuccessRate != 0 || st.Previous == nil {
		t.Errorf("stats = %+v", st.Totals)
	}
	met := h.do(admin, "GET", "/api/v1/metrics", nil).Body.String()
	if !strings.Contains(met, "forge_job_queue_wait_seconds{quantile=\"0.95\"}") {
		t.Errorf("metrics missing latency: %s", met)
	}
}

func TestWebhookLogRecordsRejectedDeliveries(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	h := newEnforcedHarness(t)
	ctx := context.Background()
	body := []byte(`{"ref":"refs/heads/main","after":"abc1234","repository":{"full_name":"acme/shop"},"pusher":{"name":"ravi"}}`)
	post := func(sig, ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "d-"+sig[:3])
		req.Header.Set("X-Hub-Signature-256", sig)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("sha256=bad0000", "10.0.0.9"); code != 401 {
		t.Fatalf("bad signature = %d", code)
	}
	if code := post("sha256=bad1111", "10.0.0.9"); code != 401 {
		t.Fatalf("bad signature = %d", code)
	}
	health, entries, err := h.st.RepoWebhookHealth(ctx, "acme/shop", time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if health.Rejected != 2 || len(entries) != 2 || entries[0].Status != 401 || entries[0].Reason != "invalid webhook signature" || entries[0].Event != "push" || entries[0].SourceIP != "10.0.0.9" {
		t.Errorf("health = %+v entries = %+v", health, entries)
	}
	admin := h.login("admin@acme.io")
	rec := h.do(admin, "GET", "/api/v1/webhook-log/failing", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"repo":"acme/shop"`) || !strings.Contains(rec.Body.String(), `"rejected":2`) {
		t.Errorf("failing = %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(h.login("dev@acme.io"), "GET", "/api/v1/webhook-log?repo=acme/shop", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"failing_sources":["10.0.0.9"]`) {
		t.Errorf("repo log = %d %s", rec.Code, rec.Body.String())
	}
}
