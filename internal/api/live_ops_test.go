package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/agent"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

type applyLog struct {
	mu   sync.Mutex
	objs []string
}

func (a *applyLog) Apply(_ context.Context, o *unstructured.Unstructured, _, _ bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.objs = append(a.objs, o.GetKind()+"/"+o.GetName())
	return "configured", nil
}

func (a *applyLog) Delete(context.Context, string, string, string, string, string, bool) error {
	return nil
}

func (a *applyLog) Get(context.Context, string, string, string, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func gitSource(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	files := map[string]string{
		"apps/team/cart/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: cart}\nspec: {replicas: 2}\n",
		"apps/team/cart/extra.yaml":      "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cart-extra}\ndata: {A: b}\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "cart: 2 replicas"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=Lab", "-c", "user.email=lab@example.com"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func (h *liveHarness) op(id int64, body map[string]any) (*httptest.ResponseRecorder, store.LiveOp) {
	h.t.Helper()
	rec := h.do(testCaller{}, http.MethodPost, "/api/v1/live/apps/"+pid(id)+"/ops", body)
	var o store.LiveOp
	_ = json.Unmarshal(rec.Body.Bytes(), &o)
	return rec, o
}

func (h *liveHarness) opState(id int64) store.LiveOp {
	h.t.Helper()
	var o store.LiveOp
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/ops/"+pid(id), nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &o)
	return o
}

// TestLiveOperations: desired state from Git reaches the page; a sync
// requested on the page is claimed by the agent, applied and reported;
// a protected environment's sync waits for its approval; a second sync of
// the same app is refused while one runs; pod logs stream to the page.
func TestLiveOperations(t *testing.T) {
	h := newLiveHarness(t)
	cluster := livetest.NewFakeKube(t)
	cluster.Deploy(t, "cart", live.Healthy)
	applied := &applyLog{}
	p, err := kube.New(cluster.Client, kube.Config{Applier: applied,
		Source: &kube.Source{URL: gitSource(t), Poll: time.Hour, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Run(ctx, agent.Config{Provider: p, URL: srv.URL, Token: h.token, Heartbeat: time.Second})
	}()
	defer func() { cancel(); <-done }()

	var id int64
	eventually(t, "the app with its desired state", func() bool {
		ps := h.apps("")
		if ps.Total != 1 || ps.Apps[0].Revision == "" {
			return false
		}
		id = ps.Apps[0].ID
		return ps.Apps[0].Sync == live.OutOfSync
	})
	v := h.app(id)
	if v.App.RevisionMessage != "cart: 2 replicas" || v.App.SourcePath != "apps/team/cart" || !v.Target.Capabilities.Sync {
		t.Errorf("app = %+v, target = %+v", v.App, v.Target)
	}
	missing := 0
	for _, r := range v.Tree.Resources {
		if r.Health != nil && r.Health.Status == live.Missing {
			missing++
		}
	}
	if missing != 1 {
		t.Errorf("%d missing resources, want the ConfigMap only", missing)
	}
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(id)+"/desired?key=apps/Deployment/team/cart", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"replicas":2`) {
		t.Errorf("desired manifest: %d %s", rec.Code, rec.Body.String())
	}

	// A sync: queued, claimed by the agent, reported.
	rec, o := h.op(id, map[string]any{"kind": "sync"})
	if rec.Code != http.StatusCreated || o.State != "pending" {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	eventually(t, "the sync to finish", func() bool { return h.opState(o.ID).State == "succeeded" })
	if got := h.opState(o.ID); len(got.Result.Resources) != 2 || !strings.HasPrefix(got.Message, "Synced 2 resources") {
		t.Errorf("sync result = %+v", got)
	}

	// A protected environment: the sync waits for an approval.
	if _, err := h.db.Exec(context.Background(), `INSERT INTO protected_environments (repo, name, required_approvals, approver_roles)
		VALUES ('', 'fleet', 1, '{admin,owner,developer}')`); err != nil {
		t.Fatal(err)
	}
	_, o = h.op(id, map[string]any{"kind": "sync", "prune": true})
	if o.State != "approval" {
		t.Fatalf("protected sync state = %q", o.State)
	}
	if rec, _ := h.op(id, map[string]any{"kind": "sync"}); rec.Code != http.StatusConflict {
		t.Errorf("a second sync while one waits: %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	if st := h.opState(o.ID).State; st != "approval" {
		t.Fatalf("an unapproved sync ran: %s", st)
	}
	rec = h.do(testCaller{}, http.MethodPost, "/api/v1/live/ops/"+pid(o.ID)+"/approve", map[string]string{"verdict": "approved", "approver": "release@lab.local"})
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	eventually(t, "the approved sync", func() bool { return h.opState(o.ID).State == "succeeded" })
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(id)+"/ops", nil)
	var history []store.LiveOp
	_ = json.Unmarshal(rec.Body.Bytes(), &history)
	if len(history) != 2 || len(history[0].Approvals) != 0 && history[0].Approvals[0].Approver != "release@lab.local" {
		t.Errorf("history = %+v", history)
	}

	// A frozen environment refuses syncs (a dry run is still fine).
	if _, err := h.db.Exec(context.Background(), `INSERT INTO deploy_freezes (environment, starts_at, ends_at, reason)
		VALUES ('fleet', now() - interval '1 hour', now() + interval '1 hour', 'release week')`); err != nil {
		t.Fatal(err)
	}
	if rec, _ := h.op(id, map[string]any{"kind": "sync"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "frozen") {
		t.Errorf("sync during a freeze: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := h.op(id, map[string]any{"kind": "sync", "dry_run": true}); rec.Code != http.StatusCreated {
		t.Errorf("dry run during a freeze: %d %s", rec.Code, rec.Body.String())
	}

	// Logs stream to the page.
	rec, o = h.op(id, map[string]any{"kind": "logs", "pod": "team/cart-7d9f-0"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("logs: %d %s", rec.Code, rec.Body.String())
	}
	resp, err := http.Get(srv.URL + "/api/v1/live/ops/" + pid(o.ID) + "/output")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var out strings.Builder
	for sc.Scan() {
		out.WriteString(sc.Text() + "\n")
		if strings.HasPrefix(sc.Text(), "event: end") {
			break
		}
	}
	if !strings.Contains(out.String(), "fake logs") || !strings.Contains(out.String(), "event: end") {
		t.Errorf("log stream = %q", out.String())
	}
}
