package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestLogSummaryAndSearch(t *testing.T) {
	t.Setenv("LOG_BACKEND", "postgres")
	st, db := newCFRStore(t, "")
	ctx := context.Background()
	srv := New(st, nil, logstore.Resolve(ctx, st, nil))
	h := &apiHarness{t: t, st: st, db: db, srv: srv}
	p := h.seedPipeline("acme/cos", "main", plainYAML)
	var jobID int64
	if err := db.QueryRow(ctx, `SELECT id FROM jobs WHERE pipeline_id=$1`, p.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	log := "$ make test\nok  pkg/a\n--- FAIL: TestPay (0.01s)\n    pay_test.go:9: boom\nFAIL\nmake: *** [test] Error 1\n"
	if err := st.AppendLog(ctx, jobID, log); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+pid(jobID)+"/logs/summary", nil))
	h.expect(rec, 200, "summary")
	sum := decodeInto[logstore.Summary](t, rec)
	if sum.FirstErrorLine != 3 || sum.FirstError != "--- FAIL: TestPay (0.01s)" || len(sum.Tail) != 6 {
		t.Errorf("summary = %+v", sum)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+pid(jobID)+"/logs/search?q=FAIL", nil))
	h.expect(rec, 200, "search")
	got := decodeInto[struct {
		Matches []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	}](t, rec)
	if len(got.Matches) != 2 || got.Matches[0].Line != 3 || got.Matches[1].Line != 5 {
		t.Errorf("search = %+v", got.Matches)
	}
}

func TestQueueMetricsRender(t *testing.T) {
	out := renderQueueMetrics(&store.RunnerLoad{
		Queues:  []store.QueueStat{{Tags: []string{"gpu", "linux"}, Pending: 4, OldestWaitSeconds: 90}},
		Runners: []store.RunnerLoadRow{{Online: true, Running: 1}, {Online: true}, {Online: false}},
		Pending: 4, Running: 1,
	})
	for _, want := range []string{
		`forge_queue_pending_jobs{tags="gpu,linux"} 4`,
		`forge_queue_oldest_wait_seconds{tags="gpu,linux"} 90`,
		"forge_jobs_pending 4", "forge_runners_online 2", "forge_runners_busy 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}
