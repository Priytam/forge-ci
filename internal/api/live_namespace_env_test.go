package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestLiveNamespaceEnvironments: one target (one agent) reports a cluster
// with staging and prod namespaces; a namespace → environment map gives each
// app its own environment for the list, its filters and the repo summary.
func TestLiveNamespaceEnvironments(t *testing.T) {
	h := newLiveHarness(t)
	st := &live.Status{Health: live.Health{Status: live.Healthy}, Resources: 1}
	b := live.Batch{Epoch: "a", Seq: 1, Provider: "kubernetes", Capabilities: live.Capabilities{Watch: true},
		Deltas: []live.Delta{
			{App: live.App{Key: "travafa-staging/api", Name: "api", Namespace: "travafa-staging"}, Status: st, Upserts: []live.Resource{res("s1", "Deployment")}},
			{App: live.App{Key: "travafa-prod/api", Name: "api", Namespace: "travafa-prod"}, Status: st, Upserts: []live.Resource{res("p1", "Deployment")}},
			{App: live.App{Key: "tools/grafana", Name: "grafana", Namespace: "tools"}, Status: st, Upserts: []live.Resource{res("g1", "Deployment")}},
		}}
	if code, ack := h.batch(b); code != 200 || ack.Resync {
		t.Fatalf("batch: %d %+v", code, ack)
	}

	rec := h.do(testCaller{}, http.MethodPatch, "/api/v1/live/targets/"+pid(h.target),
		map[string]any{"namespace_environments": map[string]string{"travafa-staging": "staging", "travafa-prod": "prod"}})
	h.expect(rec, http.StatusOK, "set namespace environments")
	var tgt store.LiveTarget
	_ = json.Unmarshal(rec.Body.Bytes(), &tgt)
	if tgt.Environment != "fleet" || tgt.NamespaceEnvironments["travafa-prod"] != "prod" {
		t.Fatalf("target = %+v", tgt)
	}

	p := h.apps("")
	envs := map[string]string{}
	for _, a := range p.Apps {
		envs[a.Namespace] = a.Environment
	}
	if envs["travafa-staging"] != "staging" || envs["travafa-prod"] != "prod" || envs["tools"] != "fleet" {
		t.Fatalf("app environments = %v", envs)
	}
	if f := p.Facets["environment"]; f["staging"] != 1 || f["prod"] != 1 || f["fleet"] != 1 {
		t.Errorf("environment facet = %v", f)
	}
	if p := h.apps("?environment=prod"); p.Total != 1 || p.Apps[0].Namespace != "travafa-prod" {
		t.Errorf("filter environment=prod: %+v", p.Apps)
	}
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/live/summary?repo=lab/platform-gitops", nil)
	h.expect(rec, http.StatusOK, "summary")
	var sum map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sum)
	if sum["prod"] == nil || sum["staging"] == nil || sum["fleet"] == nil {
		t.Errorf("summary environments = %v", sum)
	}

	rec = h.do(testCaller{}, http.MethodPatch, "/api/v1/live/targets/"+pid(h.target),
		map[string]any{"namespace_environments": map[string]string{"travafa-prod": " "}})
	h.expect(rec, http.StatusBadRequest, "an empty environment is refused")
	rec = h.do(testCaller{}, http.MethodPatch, "/api/v1/live/targets/999999", map[string]any{"environment": "x"})
	h.expect(rec, http.StatusNotFound, "unknown target")
}
