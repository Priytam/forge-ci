package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/autoscale"
	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestRunnerPoolsCRUDAndMasking(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	h.expect(h.do(h.login("dev@acme.io"), "GET", "/api/v1/runner-pools", nil), 403, "")
	for body, want := range map[string]string{
		`{"name":"Bad Name","provider":"kubernetes","max_runners":3,"settings":"{\"namespace\":\"ci\",\"deployment\":\"r\"}"}`:  "lowercase",
		`{"name":"x","provider":"nomad","max_runners":3}`:                                                                       "provider must be",
		`{"name":"x","provider":"kubernetes","max_runners":3,"settings":"{\"namespace\":\"ci\"}"}`:                              "settings need",
		`{"name":"x","provider":"exec","min_runners":5,"max_runners":3,"settings":"{\"count_cmd\":\"a\",\"scale_cmd\":\"b\"}"}`: "max between 1 and 1000",
	} {
		var b map[string]any
		_ = json.Unmarshal([]byte(body), &b)
		if rec := h.do(admin, "POST", "/api/v1/runner-pools", b); rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s → %d %s, want %q", body, rec.Code, rec.Body.String(), want)
		}
	}
	rec := h.do(admin, "POST", "/api/v1/runner-pools", map[string]any{
		"name": "linux", "provider": "kubernetes", "tags": []string{"linux"}, "min_runners": 1, "max_runners": 5,
		"settings": `{"namespace":"ci","deployment":"forge-runners","api_url":"https://k8s","token":"sa-secret"}`, "enabled": true,
	})
	h.expect(rec, 201, "")
	var pool store.RunnerPool
	_ = json.Unmarshal(rec.Body.Bytes(), &pool)
	if strings.Contains(pool.Settings, "sa-secret") || !strings.Contains(pool.Settings, `"token":"••••••••"`) {
		t.Errorf("token not masked: %s", pool.Settings)
	}
	// Saving the masked form back keeps the real token.
	h.expect(h.do(admin, "PUT", "/api/v1/runner-pools/"+strconv.FormatInt(pool.ID, 10), map[string]any{
		"name": "linux", "provider": "kubernetes", "tags": []string{"linux"}, "min_runners": 2, "max_runners": 6, "settings": pool.Settings, "enabled": true,
	}), 200, "")
	stored, _ := h.st.GetRunnerPool(context.Background(), pool.ID)
	if !strings.Contains(stored.Settings, `"token":"sa-secret"`) || stored.Min != 2 {
		t.Errorf("stored after masked update = %+v", stored)
	}
	h.expect(h.do(admin, "POST", "/api/v1/runner-pools", map[string]any{"name": "linux", "provider": "exec", "max_runners": 1, "settings": `{"count_cmd":"a","scale_cmd":"b"}`}), 409, "")
	h.expect(h.do(admin, "DELETE", "/api/v1/runner-pools/"+strconv.FormatInt(pool.ID, 10), nil), 204, "")
}

// TestAutoscalePassFollowsTheQueue drives a real pass with the exec
// provider: pending jobs for the pool's tags make it grow; idleness after the
// window makes it shrink one step at a time; a pool for other tags is untouched.
func TestAutoscalePassFollowsTheQueue(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	dir := t.TempDir()
	mk := func(name, tag string) (*store.RunnerPool, string) {
		state := filepath.Join(dir, name)
		_ = os.WriteFile(state, []byte("0"), 0o644)
		p, err := h.st.SaveRunnerPool(ctx, store.RunnerPool{Name: name, Provider: "exec", Tags: []string{tag}, Min: 0, Max: 3, JobsPerRunner: 1,
			ScaleDownAfter: 60, Enabled: true,
			Settings: `{"count_cmd":"cat ` + state + `","scale_cmd":"echo $FORGE_DESIRED > ` + state + `"}`})
		if err != nil {
			t.Fatal(err)
		}
		return p, state
	}
	linux, linuxState := mk("linux", "linux")
	_, gpuState := mk("gpu", "gpu")
	count := func(state string) string { b, _ := os.ReadFile(state); return strings.TrimSpace(string(b)) }

	yml := "stages: [b]\njobs:\n  a:\n    stage: b\n    tags: [linux]\n    script: [x]\n  b:\n    stage: b\n    tags: [linux]\n    script: [x]\n  c:\n    stage: b\n    tags: [linux, docker]\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	if _, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false); err != nil {
		t.Fatal(err)
	}
	_, _ = h.st.PromoteReadyJobs(ctx)
	if err := autoscale.Pass(ctx, h.st, time.Now()); err != nil {
		t.Fatal(err)
	}
	// a and b need [linux] (covered); c needs docker too (not covered).
	if count(linuxState) != "2" || count(gpuState) != "0" {
		t.Fatalf("after pass: linux=%s gpu=%s, want 2 / 0", count(linuxState), count(gpuState))
	}
	p, _ := h.st.GetRunnerPool(ctx, linux.ID)
	if p.Current != 2 || p.PendingJobs != 2 || !strings.HasPrefix(p.LastAction, "scale up 0 → 2") {
		t.Errorf("status = %+v", p)
	}
	// Queue drained; still inside the idle window: hold.
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET status='success', finished_at=now() WHERE status='pending'`)
	if err := autoscale.Pass(ctx, h.st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count(linuxState) != "2" {
		t.Errorf("scaled down inside the idle window: %s", count(linuxState))
	}
	// Idle past the window: one step down per pass.
	_, _ = h.db.Exec(ctx, `UPDATE runner_pools SET last_busy_at = now() - interval '5 minutes'`)
	if err := autoscale.Pass(ctx, h.st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count(linuxState) != "1" {
		t.Errorf("first idle pass → %s, want 1", count(linuxState))
	}
	// A provider failure is recorded, not fatal.
	_, _ = h.db.Exec(ctx, `UPDATE runner_pools SET last_busy_at = now() - interval '5 minutes'`)
	_ = os.Chmod(linuxState, 0o000)
	defer os.Chmod(linuxState, 0o644)
	_ = autoscale.Pass(ctx, h.st, time.Now())
	p, _ = h.st.GetRunnerPool(ctx, linux.ID)
	if !strings.Contains(p.LastError, "count") {
		t.Errorf("provider error not recorded: %+v", p)
	}
}
