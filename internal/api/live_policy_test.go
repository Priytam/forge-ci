package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/agent"
	"github.com/priytamjeepandey/forge-ci/internal/live/hub"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func (h *liveHarness) policy(id int64, p map[string]bool) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/"+pid(id)+"/policy", p)
}

func (h *liveHarness) history(id int64) []store.LiveOp {
	h.t.Helper()
	var ops []store.LiveOp
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(id)+"/ops", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &ops)
	return ops
}

// TestLiveAutoSync: with auto-sync on, Forge syncs the revision Git has by
// itself — as an ordinary operation the agent runs, in the history and the
// audit trail; drift after that is left alone until self-heal is on, and
// then undone. A sync to another revision is refused while auto-sync is on;
// a rollback turns it off. Self-heal is refused where changes need approval.
func TestLiveAutoSync(t *testing.T) {
	h := newLiveHarness(t)
	cluster := livetest.NewFakeKube(t)
	cluster.Deploy(t, "cart", live.Healthy)
	applied := &applyLog{}
	p, err := kube.New(cluster.Client, kube.Config{Applier: applied, Orphans: true,
		Source: &kube.Source{URL: gitSource(t), Poll: time.Hour, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		_ = agent.Run(ctx, agent.Config{Provider: p, URL: srv.URL, Token: h.token, Heartbeat: time.Second})
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		h.srv.live.AutoSync(ctx)
	}()
	defer func() { cancel(); <-done; <-done }()

	var id int64
	eventually(t, "the out-of-sync app", func() bool {
		ps := h.apps("")
		if ps.Total != 1 || ps.Apps[0].Revision == "" {
			return false
		}
		id = ps.Apps[0].ID
		return ps.Apps[0].Sync == live.OutOfSync && ps.Apps[0].Drifted == 1
	})
	rev := h.app(id).App.Revision
	if got := h.app(id).App.Policy; got == nil || got.AutoSync {
		t.Fatalf("default policy = %+v", got)
	}

	// Auto-sync on: Forge syncs Git's revision by itself, with force.
	if rec := h.policy(id, map[string]bool{"auto_sync": true}); rec.Code != 200 {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body.String())
	}
	eventually(t, "the auto-sync", func() bool {
		ops := h.history(id)
		return len(ops) == 1 && ops[0].State == "succeeded"
	})
	o := h.history(id)[0]
	if o.RequestedBy != hub.AutoSyncActor || o.Params.Trigger != live.TriggerAutoSync || !o.Params.Force || o.Revision != rev {
		t.Errorf("auto-sync op = %+v", o)
	}
	var audited int
	_ = h.db.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE actor = $1 AND action = 'live.sync'`, hub.AutoSyncActor).Scan(&audited)
	if audited != 1 {
		t.Errorf("auto-syncs audited: %d", audited)
	}

	// The fake applier changes nothing, so the app is still out of sync:
	// that is drift now, and without self-heal Forge leaves it.
	eventually(t, "auto-sync to stand down", func() bool {
		return strings.Contains(h.app(id).App.Policy.Status, "self-heal is off")
	})
	if n := len(h.history(id)); n != 1 {
		t.Errorf("auto-sync synced the same revision again: %d ops", n)
	}

	// Self-heal undoes it (after the pause that follows a sync).
	h.policy(id, map[string]bool{"auto_sync": true, "self_heal": true})
	eventually(t, "the self-heal", func() bool {
		ops := h.history(id)
		return len(ops) == 2 && ops[0].Params.Trigger == live.TriggerSelfHeal && ops[0].State == "succeeded"
	})

	// Another revision while auto-sync is on would be undone: refused. A
	// rollback is the way, and it turns auto-sync off.
	if rec, _ := h.op(id, map[string]any{"kind": "sync", "revision": "0000000"}); rec.Code != http.StatusConflict {
		t.Errorf("sync to another revision with auto-sync on: %d", rec.Code)
	}
	if rec, _ := h.op(id, map[string]any{"kind": "rollback", "revision": rev}); rec.Code != http.StatusCreated {
		t.Fatalf("rollback: %d %s", rec.Code, rec.Body.String())
	}
	if pol := h.app(id).App.Policy; pol.AutoSync || !pol.SelfHeal || !strings.Contains(pol.Status, "Turned off by the rollback") {
		t.Errorf("policy after a rollback = %+v", pol)
	}

	// Where every change needs an approval, nothing heals itself.
	if _, err := h.db.Exec(context.Background(), `INSERT INTO protected_environments (repo, name, required_approvals, approver_roles)
		VALUES ('', 'fleet', 1, '{admin,owner,developer}')`); err != nil {
		t.Fatal(err)
	}
	if rec := h.policy(id, map[string]bool{"auto_sync": true, "self_heal": true}); rec.Code != http.StatusConflict {
		t.Errorf("self-heal on a protected environment: %d", rec.Code)
	}
	if rec := h.policy(id, map[string]bool{"auto_sync": true}); rec.Code != 200 {
		t.Errorf("auto-sync on a protected environment: %d %s", rec.Code, rec.Body.String())
	}

	// The namespace's orphaned resources: none in this cluster yet.
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(id)+"/orphans", nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Errorf("orphans: %d %s", rec.Code, rec.Body.String())
	}
	if ps := h.apps(""); ps.Total != 1 {
		t.Errorf("orphans listed as apps: %d", ps.Total)
	}
}
