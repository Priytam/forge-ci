package autoscale

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	p := Pool{Min: 1, Max: 5, JobsPerRunner: 1, ScaleDownAfter: 10 * time.Minute}
	now := time.Now()
	cases := []struct {
		name       string
		d          Demand
		current    int
		idleSince  time.Time
		wantAction string
		wantTo     int
	}{
		{"scale up for queue", Demand{Pending: 3, Running: 1}, 1, time.Time{}, "scale up", 4},
		{"cap at max", Demand{Pending: 20}, 2, time.Time{}, "scale up", 5},
		{"hold while pending", Demand{Pending: 1}, 4, time.Time{}, "", 4},
		{"hold while recently busy", Demand{}, 4, now.Add(-time.Minute), "", 4},
		{"one step down when idle", Demand{}, 4, now.Add(-time.Hour), "scale down", 3},
		{"never below min", Demand{}, 1, now.Add(-time.Hour), "", 1},
		{"at target", Demand{Running: 2}, 2, time.Time{}, "", 2},
	}
	for _, c := range cases {
		dec := Decide(p, c.d, c.current, c.idleSince, now)
		if dec.Action != c.wantAction || dec.Desired != c.wantTo {
			t.Errorf("%s: got %q → %d (%s), want %q → %d", c.name, dec.Action, dec.Desired, dec.Reason, c.wantAction, c.wantTo)
		}
	}
	// jobs_per_runner > 1 (kubernetes-executor runners)
	if dec := Decide(Pool{Min: 0, Max: 10, JobsPerRunner: 4}, Demand{Pending: 9}, 0, time.Time{}, now); dec.Desired != 3 {
		t.Errorf("9 jobs / 4 per runner → %d, want 3", dec.Desired)
	}
}

func TestKubernetesProviderScalesDeployment(t *testing.T) {
	replicas := 2
	var patched string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/apis/apps/v1/namespaces/ci/deployments/forge-runners/scale" {
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"spec": map[string]int{"replicas": replicas}})
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			patched = r.Header.Get("Content-Type") + " " + string(body)
			var in struct {
				Spec struct {
					Replicas int `json:"replicas"`
				} `json:"spec"`
			}
			_ = json.Unmarshal(body, &in)
			replicas = in.Spec.Replicas
			_ = json.NewEncoder(w).Encode(map[string]any{"spec": map[string]int{"replicas": replicas}})
		}
	}))
	defer srv.Close()
	p := Pool{Name: "linux", Settings: Settings{"namespace": "ci", "deployment": "forge-runners", "api_url": srv.URL, "token": "sa-token"}}
	prov, _ := Lookup("kubernetes")
	if secrets, err := prov.Validate(p.Settings); err != nil || strings.Join(secrets, ",") != "token" {
		t.Fatalf("validate = %v %v", secrets, err)
	}
	if n, err := prov.Current(context.Background(), p); err != nil || n != 2 {
		t.Fatalf("current = %d %v", n, err)
	}
	if err := prov.Scale(context.Background(), p, 5); err != nil {
		t.Fatal(err)
	}
	if replicas != 5 || !strings.HasPrefix(patched, "application/merge-patch+json") {
		t.Errorf("after scale: replicas %d, patch %q", replicas, patched)
	}
	if _, err := prov.Validate(Settings{"namespace": "ci"}); err == nil {
		t.Error("missing deployment accepted")
	}
}

// fakeDocker is enough of the Engine API for the docker provider.
type fakeDocker struct {
	containers []map[string]any // Id, Names, State, Labels
	created    []string
	stopped    []string
}

func (f *fakeDocker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.containers)
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Query().Get("name")
		env, _ := body["Env"].([]any)
		f.created = append(f.created, name+" "+strings.Join(toStrings(env), " "))
		id := "id-" + name
		f.containers = append(f.containers, map[string]any{"Id": id, "Names": []string{"/" + name}, "State": "created", "Labels": body["Labels"]})
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	})
	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		for _, c := range f.containers {
			if c["Id"] == r.PathValue("id") {
				c["State"] = "running"
			}
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.stopped = append(f.stopped, r.PathValue("id"))
		for _, c := range f.containers {
			if c["Id"] == r.PathValue("id") {
				c["State"] = "exited"
			}
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		kept := f.containers[:0]
		for _, c := range f.containers {
			if c["Id"] != r.PathValue("id") {
				kept = append(kept, c)
			}
		}
		f.containers = kept
		w.WriteHeader(204)
	})
	return mux
}

func toStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v.(string))
	}
	return out
}

func TestDockerProviderStartsAndStopsRunners(t *testing.T) {
	fd := &fakeDocker{containers: []map[string]any{
		{"Id": "a", "Names": []string{"/forge-build-1-0"}, "State": "running"},
		{"Id": "b", "Names": []string{"/forge-build-1-1"}, "State": "running"},
	}}
	srv := httptest.NewServer(fd.handler())
	defer srv.Close()
	p := Pool{Name: "build", Tags: []string{"linux", "docker"}, Settings: Settings{
		"image": "forge-runner:test", "host": "tcp://" + strings.TrimPrefix(srv.URL, "http://"),
		"server_url": "https://forge", "runner_token": "frt_x", "env.RUNNER_DRAIN_GRACE": "5m",
	}}
	prov, _ := Lookup("docker")
	if n, err := prov.Current(context.Background(), p); err != nil || n != 2 {
		t.Fatalf("current = %d %v", n, err)
	}
	if err := prov.Scale(context.Background(), p, 4); err != nil {
		t.Fatal(err)
	}
	if len(fd.created) != 2 || !strings.Contains(fd.created[0], "RUNNER_TAGS=linux,docker") || !strings.Contains(fd.created[0], "RUNNER_TOKEN=frt_x") ||
		!strings.Contains(fd.created[0], "RUNNER_DRAIN_GRACE=5m") || !strings.Contains(fd.created[0], "RUNNER_ID=forge-build-") {
		t.Errorf("created = %v", fd.created)
	}
	if n, _ := prov.Current(context.Background(), p); n != 4 {
		t.Errorf("after scale up = %d", n)
	}
	// Scale down by one: the idle runner goes, the busy one stays.
	p.BusyRunnerIDs = []string{"forge-build-1-0"}
	fd.containers = fd.containers[:2] // back to the two originals for a clear check
	if err := prov.Scale(context.Background(), p, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fd.stopped, ",") != "b" {
		t.Errorf("stopped %v, want only the idle runner b", fd.stopped)
	}
}

func TestExecProviderRunsCommands(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "count")
	_ = os.WriteFile(state, []byte("2\n"), 0o644)
	p := Pool{Name: "vms", Tags: []string{"linux"}, Min: 0, Max: 9, Settings: Settings{
		"count_cmd":   "cat " + state,
		"scale_cmd":   "echo $FORGE_DESIRED > " + state + " && echo \"$FORGE_POOL $FORGE_TAGS $FORGE_CURRENT->$FORGE_DESIRED $REGION\" > " + filepath.Join(dir, "log"),
		"env.REGION":  "eu-west-1",
		"env.API_KEY": "k",
	}}
	prov, _ := Lookup("exec")
	secrets, err := prov.Validate(p.Settings)
	if err != nil || strings.Join(secrets, ",") != "env.API_KEY" {
		t.Fatalf("validate = %v %v", secrets, err)
	}
	if n, err := prov.Current(context.Background(), p); err != nil || n != 2 {
		t.Fatalf("current = %d %v", n, err)
	}
	if err := prov.Scale(context.Background(), p, 5); err != nil {
		t.Fatal(err)
	}
	if n, _ := prov.Current(context.Background(), p); n != 5 {
		t.Errorf("after scale = %d", n)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	if strings.TrimSpace(string(log)) != "vms linux 2->5 eu-west-1" {
		t.Errorf("scale env = %q", log)
	}
	p.Settings["count_cmd"] = "echo not-a-number"
	if _, err := prov.Current(context.Background(), p); err == nil {
		t.Error("garbage count accepted")
	}
}
