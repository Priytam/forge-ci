package livetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/cloudrun"
)

// FakeCloudRun is the Cloud Run Admin API v2 (services and revisions lists)
// for one project and region.
type FakeCloudRun struct {
	Server *httptest.Server

	mu       sync.Mutex
	services map[string]map[string]any // name → service JSON
	revs     map[string][]map[string]any
	calls    int
	// Patches counts services.patch calls (validate-only included).
	Patches int
}

const fakeProject, fakeRegion = "acme-prod", "asia-south1"

// NewFakeCloudRun starts an empty project.
func NewFakeCloudRun(t *testing.T) *FakeCloudRun {
	f := &FakeCloudRun{services: map[string]map[string]any{}, revs: map[string][]map[string]any{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// Calls is how many API requests the fake has answered.
func (f *FakeCloudRun) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *FakeCloudRun) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	prefix := fmt.Sprintf("/v2/projects/%s/locations/%s/services", fakeProject, fakeRegion)
	switch {
	case r.URL.Path == prefix:
		out := []map[string]any{}
		for _, s := range f.services {
			out = append(out, s)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"services": out})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, prefix+"/"):
		f.patch(w, r, strings.TrimPrefix(r.URL.Path, prefix+"/"))
	case strings.HasPrefix(r.URL.Path, prefix+"/") && strings.HasSuffix(r.URL.Path, "/revisions"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"/"), "/revisions")
		_ = json.NewEncoder(w).Encode(map[string]any{"revisions": f.revs[name]})
	case strings.HasPrefix(r.URL.Path, prefix+"/"):
		svc := f.services[strings.TrimPrefix(r.URL.Path, prefix+"/")]
		if svc == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(svc)
	default:
		http.NotFound(w, r)
	}
}

// patch is services.patch: a new image makes a new revision taking all
// traffic; a traffic list moves traffic. validateOnly changes nothing.
func (f *FakeCloudRun) patch(w http.ResponseWriter, r *http.Request, name string) {
	svc := f.services[name]
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	f.Patches++
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("validateOnly") == "true" {
		_, _ = w.Write([]byte(`{}`))
		return
	}
	image := body["template"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"]
	old := svc["template"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"]
	full := svc["name"].(string)
	if image != old {
		n := len(f.revs[name]) + 1
		rev := fmt.Sprintf("%s-%05d", name, n)
		f.revs[name] = append(f.revs[name], map[string]any{"name": full + "/revisions/" + rev, "uid": fmt.Sprintf("%s-rev-%d", name, n),
			"createTime": time.Now().UTC().Format(time.RFC3339), "conditions": []any{cond("Ready", "CONDITION_SUCCEEDED", "")},
			"containers": []any{map[string]any{"image": image}}})
		svc["template"] = body["template"]
		svc["latestCreatedRevision"], svc["latestReadyRevision"] = full+"/revisions/"+rev, full+"/revisions/"+rev
		svc["trafficStatuses"] = []any{map[string]any{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": rev, "percent": 100}}
	} else if t, ok := body["traffic"].([]any); ok {
		svc["trafficStatuses"] = t
	}
	svc["etag"] = fmt.Sprint(time.Now().UnixNano())
	_, _ = w.Write([]byte(`{}`))
}

func (f *FakeCloudRun) Provider(t *testing.T) live.Provider {
	p, err := cloudrun.New(cloudrun.Config{Project: fakeProject, Regions: []string{fakeRegion},
		Period: 20 * time.Millisecond, CallsPerMinute: 60_000, BaseURL: f.Server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cond(typ, state, msg string) map[string]any {
	return map[string]any{"type": typ, "state": state, "message": msg}
}

// Deploy creates a service: Healthy (one ready revision, all traffic),
// Degraded (its newest revision failed to start) or Progressing (a revision
// rolling out).
func (f *FakeCloudRun) Deploy(t *testing.T, name, health string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := fmt.Sprintf("projects/%s/locations/%s/services/%s", fakeProject, fakeRegion, name)
	created := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rev := func(n int, conds ...map[string]any) map[string]any {
		return map[string]any{"name": fmt.Sprintf("%s/revisions/%s-%05d", full, name, n), "uid": fmt.Sprintf("%s-rev-%d", name, n),
			"createTime": created, "conditions": conds, "containers": []any{map[string]any{"image": fmt.Sprintf("gcr.io/acme/%s:v%d", name, n)}}}
	}
	ready := cond("Ready", "CONDITION_SUCCEEDED", "")
	svc := map[string]any{"name": full, "uid": name + "-svc", "createTime": created, "uri": "https://" + name + "-x.a.run.app",
		"latestReadyRevision": full + "/revisions/" + name + "-00001", "latestCreatedRevision": full + "/revisions/" + name + "-00001",
		"terminalCondition": cond("Ready", "CONDITION_SUCCEEDED", ""),
		"template":          map[string]any{"containers": []any{map[string]any{"image": "gcr.io/acme/" + name + ":v1"}}},
		"trafficStatuses":   []any{map[string]any{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": name + "-00001", "percent": 100}}}
	revs := []map[string]any{rev(1, ready)}
	switch health {
	case live.Degraded:
		svc["latestCreatedRevision"] = full + "/revisions/" + name + "-00002"
		svc["terminalCondition"] = cond("Ready", "CONDITION_FAILED", "Revision '"+name+"-00002' is not ready and cannot serve traffic.")
		revs = append(revs, rev(2, cond("Ready", "CONDITION_FAILED", "The user-provided container failed to start and listen on PORT=8080")))
	case live.Progressing:
		svc["reconciling"] = true
		svc["latestCreatedRevision"] = full + "/revisions/" + name + "-00002"
		r := rev(2, cond("Ready", "CONDITION_RECONCILING", ""))
		r["reconciling"] = true
		revs = append(revs, r)
	}
	f.services[name], f.revs[name] = svc, revs
	return fakeRegion + "/" + name
}

// Change relabels the service (the service changes, its revisions do not).
func (f *FakeCloudRun) Change(t *testing.T, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[key[len(fakeRegion)+1:]]["labels"] = map[string]any{"changed": fmt.Sprint(time.Now().UnixNano())}
}

// Remove deletes the service.
func (f *FakeCloudRun) Remove(t *testing.T, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.services, key[len(fakeRegion)+1:])
}
