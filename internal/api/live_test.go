package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/agent"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

type liveHarness struct {
	*apiHarness
	token  string
	target int64
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	st, db := newCFRStore(t, "")
	h := &liveHarness{apiHarness: &apiHarness{t: t, st: st, db: db, srv: New(st, blob.NewLocal(t.TempDir()), nil)}}
	rec := h.do(testCaller{}, http.MethodPost, "/api/v1/live/targets", map[string]string{"name": "forge-lab", "environment": "fleet", "repo": "lab/platform-gitops"})
	h.expect(rec, http.StatusCreated, "create target")
	var out struct {
		Target store.LiveTarget `json:"target"`
		Token  string           `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	h.token, h.target = out.Token, out.Target.ID
	return h
}

func (h *liveHarness) batch(b live.Batch) (int, live.Ack) {
	h.t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(b)
	_ = zw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/live/agent/batches", &buf)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	var ack live.Ack
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	return rec.Code, ack
}

type appsPage struct {
	Apps   []store.LiveApp           `json:"apps"`
	Total  int                       `json:"total"`
	Facets map[string]map[string]int `json:"facets"`
}

func (h *liveHarness) apps(query string) appsPage {
	h.t.Helper()
	var p appsPage
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps"+query, nil)
	h.expect(rec, http.StatusOK, "list apps")
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return p
}

type appView struct {
	App  store.LiveApp `json:"app"`
	Tree struct {
		Version   int64           `json:"version"`
		Resources []live.Resource `json:"resources"`
	} `json:"tree"`
	Target liveAppTarget `json:"target"`
}

func (h *liveHarness) app(id int64) appView {
	h.t.Helper()
	var v appView
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(id), nil)
	h.expect(rec, http.StatusOK, "get app")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func res(uid, kind string, parents ...string) live.Resource {
	return live.Resource{Kind: kind, Name: uid, UID: uid, Parents: parents, Health: &live.Health{Status: live.Healthy}, Manifest: json.RawMessage(`{"kind":"` + kind + `"}`)}
}

// The agent's resume token: (epoch, seq). A retried batch is acknowledged
// without being applied twice; a gap or an unknown epoch mid-way asks for a
// fresh report; a fresh report merges, then its Keep lists and Synced drop
// whatever the cluster no longer has.
func TestLiveIngestResumeTokens(t *testing.T) {
	h := newLiveHarness(t)
	cart := live.App{Key: "checkout/cart-api", Name: "cart-api", Namespace: "checkout", Group: "checkout"}
	old := live.App{Key: "checkout/legacy", Name: "legacy", Namespace: "checkout"}
	st := &live.Status{Health: live.Health{Status: live.Healthy}, Resources: 2}
	first := live.Batch{Epoch: "a", Seq: 1, Provider: "kubernetes", Capabilities: live.Capabilities{Watch: true, Manifests: true},
		Deltas: []live.Delta{{App: cart, Status: st, Upserts: []live.Resource{res("d1", "Deployment"), res("p1", "Pod", "d1")}},
			{App: old, Status: st, Upserts: []live.Resource{res("l1", "Deployment")}}}}
	if code, ack := h.batch(first); code != 200 || ack.Seq != 1 || ack.Resync {
		t.Fatalf("first batch: %d %+v", code, ack)
	}
	p := h.apps("")
	if p.Total != 2 {
		t.Fatalf("apps = %+v", p)
	}
	id := p.Apps[0].ID
	v1 := h.app(id).Tree.Version
	if code, ack := h.batch(first); code != 200 || ack.Seq != 1 || h.app(id).Tree.Version != v1 {
		t.Errorf("a retried batch was applied again: %d %+v", code, ack)
	}
	if _, ack := h.batch(live.Batch{Epoch: "a", Seq: 3}); !ack.Resync {
		t.Errorf("a gap must ask for a fresh report: %+v", ack)
	}
	if _, ack := h.batch(live.Batch{Epoch: "b", Seq: 2}); !ack.Resync {
		t.Errorf("an epoch joined mid-way must ask for a fresh report: %+v", ack)
	}
	// Epoch b: the agent restarted; p1 is gone, p2 is new, legacy is no more.
	if _, ack := h.batch(live.Batch{Epoch: "b", Seq: 1, Deltas: []live.Delta{{App: cart, Upserts: []live.Resource{res("p2", "Pod", "d1")}}}}); ack.Seq != 1 {
		t.Fatalf("epoch b seq 1: %+v", ack)
	}
	if n := len(h.app(id).Tree.Resources); n != 3 {
		t.Errorf("within an epoch deltas merge: %d resources, want 3", n)
	}
	if _, ack := h.batch(live.Batch{Epoch: "b", Seq: 2, Synced: true, Deltas: []live.Delta{{App: cart, Status: st, Keep: []string{"d1", "p2"}}}}); ack.Seq != 2 {
		t.Fatalf("epoch b seq 2: %+v", ack)
	}
	v := h.app(id)
	uids := []string{}
	for _, r := range v.Tree.Resources {
		uids = append(uids, r.UID)
	}
	if strings.Join(uids, ",") != "d1,p2" {
		t.Errorf("after keep: %v, want d1,p2", uids)
	}
	if p := h.apps(""); p.Total != 1 || p.Apps[0].Name != "cart-api" {
		t.Errorf("the app the new epoch never mentioned is still listed: %+v", p.Apps)
	}
	var queued int
	_ = h.db.QueryRow(context.Background(), `SELECT count(*) FROM live_blob_gc`).Scan(&queued)
	if queued < 3 { // p1's manifest, legacy's tree and manifest, cart's old trees
		t.Errorf("only %d blobs queued for collection", queued)
	}
	h.token = "fla_wrong"
	if code, _ := h.batch(first); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
}

// TestLiveEndToEnd: a real agent runtime and the Kubernetes provider against
// a fake cluster, reporting to the real API over HTTP — apps listed with
// their health and facets, a tree with manifests and events, and a scale
// change arriving on the page's stream as a one-resource patch.
func TestLiveEndToEnd(t *testing.T) {
	h := newLiveHarness(t)
	cluster := livetest.NewFakeKube(t)
	cluster.Deploy(t, "cart-api", live.Healthy)
	cluster.Deploy(t, "fraud-scorer", live.Degraded)
	cluster.Deploy(t, "indexer", live.Progressing)
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Run(ctx, agent.Config{Provider: cluster.Provider(t), URL: srv.URL, Token: h.token, Heartbeat: time.Second})
	}()
	defer func() { cancel(); <-done }()

	eventually(t, "3 apps with their health", func() bool {
		p := h.apps("")
		return p.Total == 3 && p.Facets["health"][live.Degraded] == 1 && p.Facets["health"][live.Progressing] == 1
	})
	p := h.apps("?health=Degraded")
	if p.Total != 1 || p.Apps[0].Name != "fraud-scorer" || p.Facets["health"][live.Healthy] != 1 || p.Apps[0].Environment != "fleet" {
		t.Errorf("filter by health: %+v", p)
	}
	if !strings.Contains(p.Apps[0].HealthMessage, "CrashLoopBackOff") {
		t.Errorf("degraded app says why: %q", p.Apps[0].HealthMessage)
	}
	cart := h.apps("?q=cart").Apps[0]
	v := h.app(cart.ID)
	kinds := map[string]int{}
	var pod live.Resource
	for _, r := range v.Tree.Resources {
		kinds[r.Kind]++
		if r.Kind == "Pod" {
			pod = r
		}
		if len(r.Manifest) > 0 {
			t.Errorf("trees carry no manifests: %s", r.Name)
		}
	}
	if kinds["Deployment"] != 1 || kinds["ReplicaSet"] != 1 || kinds["Pod"] != 2 || kinds["Service"] != 1 || kinds["ConfigMap"] != 1 {
		t.Errorf("tree kinds = %v", kinds)
	}
	if !v.Target.Capabilities.Events || v.Target.Provider != "kubernetes" || !v.Target.Synced {
		t.Errorf("target = %+v", v.Target)
	}
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(cart.ID)+"/manifests/"+pod.UID, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"kind":"Pod"`) {
		t.Errorf("manifest: %d %s", rec.Code, rec.Body.String())
	}
	fraud := h.apps("?q=fraud").Apps[0]
	eventually(t, "the BackOff event", func() bool {
		rec := h.do(testCaller{}, http.MethodGet, "/api/v1/live/apps/"+pid(fraud.ID)+"/events", nil)
		return strings.Contains(rec.Body.String(), `"reason":"BackOff"`)
	})

	// The page's stream: a scale change is one patch with one resource.
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, srv.URL+"/api/v1/live/stream?app="+pid(cart.ID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	next := func(event string) string {
		for lines.Scan() {
			if lines.Text() == "event: "+event && lines.Scan() {
				return strings.TrimPrefix(lines.Text(), "data: ")
			}
		}
		t.Fatalf("stream ended before %q: %v", event, lines.Err())
		return ""
	}
	next("ready")
	time.Sleep(200 * time.Millisecond) // LISTEN connects asynchronously
	cluster.Change(t, "team/cart-api")
	var patch struct {
		Base, Version int64
		Upserts       []live.Resource
		Deletes       []string
	}
	_ = json.Unmarshal([]byte(next("patch")), &patch)
	if patch.Version != patch.Base+1 || len(patch.Upserts) != 1 || patch.Upserts[0].Kind != "Deployment" || patch.Upserts[0].Summary.Ready != "2/3" {
		t.Errorf("patch = %+v", patch)
	}

	// A deleted application leaves the list.
	cluster.Remove(t, "team/indexer")
	eventually(t, "indexer to go", func() bool { return h.apps("").Total == 2 })

	// The pipeline graph's summary for the target's repo.
	rec = h.do(testCaller{}, http.MethodGet, "/api/v1/live/summary?repo=lab/platform-gitops", nil)
	if !strings.Contains(rec.Body.String(), `"fleet":{"apps":2,"health":"Degraded"`) {
		t.Errorf("summary = %s", rec.Body.String())
	}
}

// TestLiveReadRoutesServeFromForgeOnly walks EVERY live read route against
// seeded state; each answers from Postgres, the blob store and memory.
func TestLiveReadRoutesServeFromForgeOnly(t *testing.T) {
	h := newLiveHarness(t)
	app := live.App{Key: "checkout/cart-api", Name: "cart-api", Namespace: "checkout"}
	ev := live.Event{UID: "e1", ResourceUID: "p1", Kind: "Pod", Name: "p1", Type: "Warning", Reason: "BackOff", Count: 3, FirstSeen: time.Now(), LastSeen: time.Now()}
	h.batch(live.Batch{Epoch: "a", Seq: 1, Synced: true, Deltas: []live.Delta{{App: app, Status: &live.Status{Health: live.Health{Status: live.Healthy}},
		Upserts: []live.Resource{res("d1", "Deployment"), res("p1", "Pod", "d1")}, Events: []live.Event{ev}, Keep: []string{"d1", "p1"},
		Desired: &live.Desired{Revision: live.Revision{SHA: "abc"}, Manifests: map[string]json.RawMessage{"apps/Deployment//d1": json.RawMessage(`{}`)}}}}})
	id := h.apps("").Apps[0].ID
	o, err := h.st.CreateLiveOp(context.Background(), id, live.Op{Kind: live.OpRefresh}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.FinishLiveOp(context.Background(), h.target, o.ID, live.OpResult{OK: true}); err == nil {
		t.Fatal("finished an operation nobody claimed")
	}
	if _, err := h.st.ClaimLiveOp(context.Background(), h.target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.FinishLiveOp(context.Background(), h.target, o.ID, live.OpResult{OK: true}); err != nil {
		t.Fatal(err)
	}
	for pattern := range h.srv.liveReadRoutes() {
		method, path, _ := strings.Cut(pattern, " ")
		ids := pid(id)
		if strings.Contains(path, "/live/ops/") {
			ids = pid(o.ID)
		}
		path = strings.NewReplacer("{id}", ids, "{uid}", "p1").Replace(path)
		switch {
		case strings.HasSuffix(path, "/summary"):
			path += "?repo=lab/platform-gitops"
		case strings.HasSuffix(path, "/desired"):
			path += "?key=apps/Deployment//d1"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, httptest.NewRequest(method, path, nil).WithContext(ctx))
		cancel()
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", pattern, rec.Code, rec.Body.String())
		}
	}
}

// TestServerHasNoProviderCode is the read path's guarantee: the server
// binary links no provider (no Kubernetes client, no cloud SDK), so nothing
// it serves can call a cluster or a cloud API. Providers live in forge-agent.
func TestServerHasNoProviderCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/priytamjeepandey/forge-ci/cmd/server").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	forbidden := []string{
		"github.com/priytamjeepandey/forge-ci/internal/live/kube",
		"github.com/priytamjeepandey/forge-ci/internal/live/cloudrun",
		"github.com/priytamjeepandey/forge-ci/internal/live/agent",
		"k8s.io/client-go/",
		"golang.org/x/oauth2/google",
	}
	for _, dep := range strings.Split(string(out), "\n") {
		for _, f := range forbidden {
			if strings.HasPrefix(dep, f) {
				t.Errorf("cmd/server depends on %s: provider code belongs in forge-agent", dep)
			}
		}
	}
}
