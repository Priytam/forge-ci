package api

import (
	"net/http"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestLiveEnvironmentPrecedence: an app's environment is its setting in
// Forge, else its environment label, else its namespace's mapping, else the
// target's — in the list, its filter and facet, the repo summary and the
// approval gate. Conflicting labels are flagged and fall through.
func TestLiveEnvironmentPrecedence(t *testing.T) {
	h := newLiveHarness(t) // target environment "fleet", repo lab/platform-gitops
	st := &live.Status{Health: live.Health{Status: live.Healthy}, Resources: 1}
	app := func(ns, name, env string, conflict ...string) live.Delta {
		return live.Delta{App: live.App{Key: ns + "/" + name, Name: name, Namespace: ns, Environment: env, EnvironmentConflict: conflict},
			Status: st, Upserts: []live.Resource{res(ns+"-"+name, "Deployment")}}
	}
	if code, ack := h.batch(live.Batch{Epoch: "a", Seq: 1, Provider: "kubernetes", Capabilities: live.Capabilities{Watch: true},
		Deltas: []live.Delta{
			app("shared", "labelled", "qa"),                     // label
			app("shared", "plain", ""),                          // namespace mapping
			app("tools", "grafana", ""),                         // target
			app("shared", "mixed", "", "production", "staging"), // conflicting labels
			app("shared", "pinned", "qa"),                       // explicit setting wins
		}}); code != 200 || ack.Resync {
		t.Fatalf("batch: %d %+v", code, ack)
	}
	h.expect(h.do(testCaller{}, http.MethodPatch, "/api/v1/live/targets/"+pid(h.target),
		map[string]any{"namespace_environments": map[string]string{"shared": "staging"}}), http.StatusOK, "namespace map")

	byName := func() map[string]store.LiveApp {
		out := map[string]store.LiveApp{}
		for _, a := range h.apps("").Apps {
			out[a.Name] = a
		}
		return out
	}
	pinned := byName()["pinned"].ID
	rec := h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/"+pid(pinned)+"/environment", map[string]string{"environment": "production"})
	h.expect(rec, http.StatusOK, "pin environment")

	want := map[string][2]string{
		"labelled": {"qa", store.EnvFromLabel},
		"plain":    {"staging", store.EnvFromNamespace},
		"grafana":  {"fleet", store.EnvFromTarget},
		"mixed":    {"staging", store.EnvFromNamespace},
		"pinned":   {"production", store.EnvFromApp},
	}
	apps := byName()
	for name, w := range want {
		if a := apps[name]; a.Environment != w[0] || a.EnvironmentSource != w[1] {
			t.Errorf("%s: %q from %q, want %q from %q", name, a.Environment, a.EnvironmentSource, w[0], w[1])
		}
	}
	if c := apps["mixed"].EnvironmentConflict; len(c) != 2 || c[0] != "production" {
		t.Errorf("mixed conflict = %v", c)
	}
	if p := h.apps("?environment=production"); p.Total != 1 || p.Apps[0].Name != "pinned" {
		t.Errorf("filter production: %+v", p.Apps)
	}
	if f := h.apps("").Facets["environment"]; f["qa"] != 1 || f["staging"] != 2 || f["fleet"] != 1 || f["production"] != 1 {
		t.Errorf("facet = %v", f)
	}

	// Environment-scoped rules follow: a sync of the labelled app waits for
	// qa's approvals, the plain one (staging) does not.
	if _, err := h.db.Exec(t.Context(), `UPDATE live_targets SET capabilities = capabilities || '{"sync": true}'`); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(testCaller{}, http.MethodPost, "/api/v1/protected-environments", map[string]any{"name": "qa", "required_approvals": 1}), http.StatusOK, "protect qa")
	_, approvals, err := h.st.LiveChangeGate(t.Context(), apps["labelled"].Repo, apps["labelled"].Environment)
	if err != nil || approvals != 1 {
		t.Fatalf("gate for the labelled app: %d %v", approvals, err)
	}
	full := map[string]bool{"auto_sync": true, "self_heal": true}
	rec = h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/"+pid(apps["labelled"].ID)+"/policy", full)
	h.expect(rec, http.StatusConflict, "self-heal refused where qa needs approvals")
	h.expect(h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/"+pid(apps["plain"].ID)+"/policy", full), http.StatusOK, "staging may self-heal")

	// Clearing the setting falls back to the label.
	h.expect(h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/"+pid(pinned)+"/environment", map[string]string{"environment": ""}), http.StatusOK, "clear")
	if a := byName()["pinned"]; a.Environment != "qa" || a.EnvironmentSource != store.EnvFromLabel {
		t.Errorf("after clearing: %q from %q", a.Environment, a.EnvironmentSource)
	}
	h.expect(h.do(testCaller{}, http.MethodPut, "/api/v1/live/apps/999999/environment", map[string]string{"environment": "x"}), http.StatusNotFound, "unknown app")
}
