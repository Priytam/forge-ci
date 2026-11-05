package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestLiveDefaultAndBulkPolicy: a target's default policy is what new apps
// start with; a bulk change applies one policy to every app a filter
// matches, previews it with dry_run, and skips (with the reason) apps the
// per-app rules refuse.
func TestLiveDefaultAndBulkPolicy(t *testing.T) {
	h := newLiveHarness(t)
	caps := live.Capabilities{Watch: true}
	st := &live.Status{Health: live.Health{Status: live.Healthy}, Resources: 1}
	app := func(ns, name string) live.Delta {
		return live.Delta{App: live.App{Key: ns + "/" + name, Name: name, Namespace: ns}, Status: st, Upserts: []live.Resource{res(ns+"-"+name, "Deployment")}}
	}
	if code, _ := h.batch(live.Batch{Epoch: "a", Seq: 1, Provider: "kubernetes", Capabilities: caps,
		Deltas: []live.Delta{app("staging", "api"), app("staging", "web")}}); code != 200 {
		t.Fatalf("batch 1: %d", code)
	}

	if _, err := h.db.Exec(t.Context(), `UPDATE live_targets SET capabilities = capabilities || '{"sync": true}'`); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/live/targets/" + pid(h.target)
	h.expect(h.do(testCaller{}, http.MethodPatch, path, map[string]any{"default_policy": map[string]bool{"prune": true}}), http.StatusBadRequest, "prune without auto_sync")
	rec := h.do(testCaller{}, http.MethodPatch, path, map[string]any{"default_policy": map[string]bool{"auto_sync": true, "prune": true}})
	h.expect(rec, http.StatusOK, "set default policy")
	if tg := decodeInto[store.LiveTarget](t, rec); tg.DefaultPolicy == nil || !tg.DefaultPolicy.AutoSync || !tg.DefaultPolicy.Prune {
		t.Fatalf("target default = %+v", tg.DefaultPolicy)
	}

	// A new app inherits it; apps that existed before keep theirs.
	if code, _ := h.batch(live.Batch{Epoch: "a", Seq: 2, Deltas: []live.Delta{app("prod", "api")}}); code != 200 {
		t.Fatalf("batch 2: %d", code)
	}
	byKey := map[string]store.LiveApp{}
	for _, a := range h.apps("").Apps {
		byKey[a.Key] = a
	}
	if p := byKey["prod/api"].Policy; p == nil || !p.AutoSync || !p.Prune || p.SelfHeal || p.UpdatedBy != store.TargetDefaultPolicyActor {
		t.Fatalf("new app policy = %+v", p)
	}
	if p := byKey["staging/api"].Policy; p != nil && p.AutoSync {
		t.Fatalf("existing app changed by the default: %+v", p)
	}

	type bulkOut struct {
		DryRun  bool               `json:"dry_run"`
		Changed []bulkPolicyResult `json:"changed"`
		Skipped []bulkPolicyResult `json:"skipped"`
	}
	if _, err := h.db.Exec(t.Context(), `UPDATE live_targets SET capabilities = capabilities || '{"sync": true}'`); err != nil {
		t.Fatal(err)
	}
	full := map[string]bool{"auto_sync": true, "prune": true, "self_heal": true}
	rec = h.do(testCaller{}, http.MethodPost, "/api/v1/live/apps/policy", map[string]any{"namespace": []string{"staging"}, "policy": full, "dry_run": true})
	h.expect(rec, http.StatusOK, "dry run")
	if out := decodeInto[bulkOut](t, rec); !out.DryRun || len(out.Changed) != 2 || len(out.Skipped) != 0 {
		t.Fatalf("dry run = %+v", out)
	}
	if p := h.app(byKey["staging/api"].ID).App.Policy; p != nil && p.AutoSync {
		t.Fatalf("dry run changed a policy: %+v", p)
	}
	rec = h.do(testCaller{}, http.MethodPost, "/api/v1/live/apps/policy", map[string]any{"namespace": []string{"staging"}, "policy": full})
	h.expect(rec, http.StatusOK, "bulk apply")
	if out := decodeInto[bulkOut](t, rec); len(out.Changed) != 2 {
		t.Fatalf("bulk = %+v", out)
	}
	for _, k := range []string{"staging/api", "staging/web"} {
		if p := h.app(byKey[k].ID).App.Policy; p == nil || !p.AutoSync || !p.Prune || !p.SelfHeal {
			t.Errorf("%s policy = %+v", k, p)
		}
	}
	var n int
	_ = h.db.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE action = 'live.policy.bulk'`).Scan(&n)
	if n != 1 {
		t.Errorf("bulk audit entries = %d, want 1", n)
	}

	// Self-heal is refused per app where every change needs an approval.
	h.expect(h.do(testCaller{}, http.MethodPost, "/api/v1/protected-environments",
		map[string]any{"name": "fleet", "required_approvals": 1}), http.StatusOK, "protect fleet")
	rec = h.do(testCaller{}, http.MethodPost, "/api/v1/live/apps/policy", map[string]any{"environment": []string{"fleet"}, "policy": full})
	h.expect(rec, http.StatusOK, "bulk on a protected environment")
	out := decodeInto[bulkOut](t, rec)
	if len(out.Changed) != 0 || len(out.Skipped) != 3 || out.Skipped[0].Reason == "" {
		raw, _ := json.Marshal(out)
		t.Fatalf("protected bulk = %s", raw)
	}
	h.expect(h.do(testCaller{}, http.MethodPost, "/api/v1/live/apps/policy", map[string]any{"policy": full}), http.StatusBadRequest, "no filter")
}
