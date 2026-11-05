package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func (h *liveHarness) agentApps(token string) []live.DeclaredApp {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/live/agent/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	h.expect(rec, http.StatusOK, "agent apps")
	var out []live.DeclaredApp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func (h *liveHarness) batchFor(token string, b live.Batch) (int, live.Ack) {
	h.t.Helper()
	defer func(old string) { h.token = old }(h.token)
	h.token = token
	return h.batch(b)
}

// TestLiveTemplates: a template declares its app on each matching target,
// with {target} and {environment} filled in; the target's agent gets it,
// and the app gets the template's sync policy once (a person's change
// stands, and a template edit only reaches apps nobody changed). The fleet
// lays apps out by name across targets.
func TestLiveTemplates(t *testing.T) {
	h := newLiveHarness(t) // forge-lab, environment fleet
	rec := h.do(testCaller{}, http.MethodPost, "/api/v1/live/targets", map[string]string{"name": "lab-east", "environment": "staging"})
	h.expect(rec, http.StatusCreated, "second target")
	var east struct {
		Target store.LiveTarget `json:"target"`
		Token  string           `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &east)

	rec = h.do(testCaller{}, http.MethodPost, "/api/v1/live/templates", map[string]any{"name": "edge", "namespace": "edge-{environment}",
		"path": "apps/edge/overlays/{target}", "environments": []string{"staging"}, "create_namespace": true, "auto_sync": true, "self_heal": true})
	h.expect(rec, http.StatusCreated, "create template")
	var tmpl store.LiveTemplate
	_ = json.Unmarshal(rec.Body.Bytes(), &tmpl)
	if rec := h.do(testCaller{}, http.MethodPost, "/api/v1/live/templates", map[string]any{"name": "Bad Name", "namespace": "x", "path": "p"}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", rec.Code)
	}
	if rec := h.do(testCaller{}, http.MethodPost, "/api/v1/live/templates", map[string]any{"name": "edge", "namespace": "x", "path": "p"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d", rec.Code)
	}

	if apps := h.agentApps(h.token); len(apps) != 0 {
		t.Errorf("fleet target got %+v", apps) // environment fleet: not matched
	}
	apps := h.agentApps(east.Token)
	want := live.DeclaredApp{App: live.App{Key: "edge-staging/edge", Name: "edge", Namespace: "edge-staging", Template: "edge"},
		Path: "apps/edge/overlays/lab-east", CreateNamespace: true}
	if len(apps) != 1 || apps[0] != want {
		t.Fatalf("declared = %+v", apps)
	}

	// The agent reports the app; its policy is the template's.
	code, _ := h.batchFor(east.Token, live.Batch{Epoch: "e1", Seq: 1, Provider: "kubernetes", Synced: true,
		Capabilities: live.Capabilities{Diff: true, Sync: true}, Deltas: []live.Delta{{App: want.App,
			Status: &live.Status{Health: live.Health{Status: live.Missing}, Sync: live.OutOfSync, Drifted: 1},
			Upserts: []live.Resource{{Kind: "ConfigMap", Name: "edge", Namespace: "edge-staging", UID: "desired:/ConfigMap/edge-staging/edge",
				Health: &live.Health{Status: live.Missing}, Sync: live.OutOfSync}}}}})
	if code != http.StatusOK {
		t.Fatalf("batch: %d", code)
	}
	ps := h.apps("?q=edge")
	if len(ps.Apps) != 1 || ps.Apps[0].Template != "edge" || !ps.Apps[0].Policy.AutoSync || !ps.Apps[0].Policy.SelfHeal {
		t.Fatalf("app = %+v", ps.Apps)
	}
	id := ps.Apps[0].ID

	// A person turns self-heal off for this app; editing the template leaves it.
	h.policy(id, map[string]bool{"auto_sync": true})
	tmpl.Prune = true
	h.expect(h.do(testCaller{}, http.MethodPut, "/api/v1/live/templates/"+pid(tmpl.ID), tmpl), http.StatusOK, "edit template")
	if p := h.app(id).App.Policy; p.SelfHeal || p.Prune {
		t.Errorf("a person's policy was overridden: %+v", p)
	}

	// The fleet: one row per app name, a cell per target it runs on.
	var fleet struct {
		Rows    []store.FleetRow `json:"rows"`
		Total   int              `json:"total"`
		Targets []liveAppTarget  `json:"targets"`
	}
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/live/fleet", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &fleet)
	if fleet.Total != 1 || len(fleet.Targets) != 2 || fleet.Rows[0].Cells[east.Target.ID].Health != live.Missing {
		t.Errorf("fleet = %s", rec.Body.String())
	}
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/live/fleet?environment=staging", nil)
	if !strings.Contains(rec.Body.String(), `"lab-east"`) || strings.Contains(rec.Body.String(), `"forge-lab"`) {
		t.Errorf("fleet for staging = %s", rec.Body.String())
	}

	h.expect(h.do(testCaller{}, http.MethodDelete, "/api/v1/live/templates/"+pid(tmpl.ID), nil), http.StatusNoContent, "delete")
	if apps := h.agentApps(east.Token); len(apps) != 0 {
		t.Errorf("after delete: %+v", apps)
	}
}
