package kube_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
)

// gitRepo is a throwaway source repository.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Lab", "-c", "user.email=lab@example.com"}, args...)...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func (r *gitRepo) commit(msg string, files map[string]string) {
	r.t.Helper()
	for name, body := range files {
		p := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
}

const cartDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cart
  labels: {app.kubernetes.io/name: cart, app.kubernetes.io/part-of: team}
spec:
  replicas: %REPLICAS%
  template:
    spec:
      containers: [{name: app, image: "registry/cart:v1"}]
`

func cart(replicas string) map[string]string {
	return map[string]string{
		"apps/team/cart/deployment.yaml": strings.ReplaceAll(cartDeployment, "%REPLICAS%", replicas),
		"apps/team/cart/service.yaml": `apiVersion: v1
kind: Service
metadata: {name: cart}
spec: {type: ClusterIP}
`,
		"apps/team/cart/extra.yaml": `apiVersion: v1
kind: ConfigMap
metadata: {name: cart-extra}
data: {FEATURE: "on"}
`,
	}
}

// fakeApplier records what a sync applied and pruned.
type fakeApplier struct {
	mu      sync.Mutex
	applied []string
	pruned  []string
}

func (f *fakeApplier) Apply(_ context.Context, o *unstructured.Unstructured, dryRun, _ bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, o.GetKind()+"/"+o.GetName())
	if dryRun {
		return "would-configure", nil
	}
	return "configured", nil
}

func (f *fakeApplier) Delete(_ context.Context, _, _, kind, _, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, kind+"/"+name)
	return nil
}

func (f *fakeApplier) Get(context.Context, string, string, string, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}

// state is the last report for one app: resources by name, its desired state.
type state struct {
	sync    map[string]string // Kind/name → sync
	health  map[string]string
	desired *live.Desired
	status  *live.Status
}

func take(t *testing.T, tr *live.Tracker, st *state, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ds, _ := tr.Take(1 << 30)
		for _, d := range ds {
			if d.App.Key != "team/cart" {
				continue
			}
			for _, r := range d.Upserts {
				st.sync[r.Kind+"/"+r.Name] = r.Sync
				if r.Health != nil {
					st.health[r.Kind+"/"+r.Name] = r.Health.Status
				}
			}
			if d.Desired != nil {
				st.desired = d.Desired
			}
			if d.Status != nil {
				st.status = d.Status
			}
		}
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDesiredStateDriftAndSync(t *testing.T) {
	repo := newGitRepo(t)
	repo.commit("cart v1", cart("2"))
	cluster := livetest.NewFakeKube(t)
	cluster.Deploy(t, "cart", live.Healthy)
	applier := &fakeApplier{}
	p, err := kube.New(cluster.Client, kube.Config{Applier: applier,
		Source: &kube.Source{URL: repo.dir, Poll: 50 * time.Millisecond, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Capabilities(); !c.Diff || !c.Sync || !c.Rollback {
		t.Fatalf("capabilities = %+v", c)
	}
	tr := live.NewTracker(live.DefaultWindow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Watch(ctx, tr) }()

	st := &state{sync: map[string]string{}, health: map[string]string{}}
	take(t, tr, st, "the desired state", func() bool {
		return st.desired != nil && st.sync["Deployment/cart"] == live.Synced && st.sync["Service/cart"] == live.Synced &&
			st.health["ConfigMap/cart-extra"] == live.Missing
	})
	if st.desired.Tool != "plain" || st.desired.Revision.Message != "cart v1" || len(st.desired.Manifests) != 3 || st.desired.Source != "apps/team/cart" {
		t.Errorf("desired = %+v", st.desired)
	}
	if st.sync["ConfigMap/cart-config"] != "" {
		t.Errorf("a live object not in Git has a sync status: %q", st.sync["ConfigMap/cart-config"])
	}
	if st.status.Sync != live.OutOfSync || st.status.Drifted != 1 || st.status.Prunable != 0 {
		t.Errorf("app sync with a missing resource = %+v", st.status)
	}

	// Once Forge has applied it (field manager "forge"), a live object Git
	// does not have requires pruning; until then it is someone else's.
	cm, err := cluster.Client.CoreV1().ConfigMaps("team").Get(ctx, "cart-config", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data = map[string]string{"touched": "by forge"}
	if _, err := cluster.Client.CoreV1().ConfigMaps("team").Update(ctx, cm, metav1.UpdateOptions{FieldManager: kube.FieldManager}); err != nil {
		t.Fatal(err)
	}
	take(t, tr, st, "the prunable object", func() bool {
		return st.sync["ConfigMap/cart-config"] == live.OutOfSync && st.status != nil && st.status.Prunable == 1
	})

	// Drift: someone scales by hand.
	cluster.Change(t, "team/cart")
	take(t, tr, st, "the drift", func() bool { return st.sync["Deployment/cart"] == live.OutOfSync })

	// Sync applies in dependency order; prune would remove what Git no
	// longer has.
	res := p.Operate(ctx, live.Op{Kind: live.OpSync, App: live.App{Key: "team/cart"}, Prune: true, DryRun: true}, nil)
	if !res.OK || len(res.Resources) != 4 {
		t.Fatalf("sync = %+v", res)
	}
	if strings.Join(applier.applied, ",") != "ConfigMap/cart-extra,Service/cart,Deployment/cart" ||
		strings.Join(applier.pruned, ",") != "ConfigMap/cart-config" {
		t.Errorf("applied %v, pruned %v", applier.applied, applier.pruned)
	}

	// Git moves to 3 replicas: the hand-scaled Deployment is in sync again.
	repo.commit("cart: 3 replicas", cart("3"))
	take(t, tr, st, "the new revision", func() bool {
		return st.desired.Revision.Message == "cart: 3 replicas" && st.sync["Deployment/cart"] == live.Synced
	})

	// A rollback renders the old revision.
	applier.applied = nil
	res = p.Operate(ctx, live.Op{Kind: live.OpRollback, App: live.App{Key: "team/cart"}, Revision: revisionOf(t, repo, "HEAD~1"),
		Resources: []string{"apps/Deployment/team/cart"}}, nil)
	if !res.OK || len(applier.applied) != 1 || applier.applied[0] != "Deployment/cart" {
		t.Errorf("rollback = %+v, applied %v", res, applier.applied)
	}
}

func revisionOf(t *testing.T, r *gitRepo, ref string) string {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A template's app exists in Forge before anything of it is in the cluster:
// every object Missing, the template named, its own path rendered; a sync
// creates the namespace first. Dropping it from the templates forgets it.
func TestDeclaredApps(t *testing.T) {
	repo := newGitRepo(t)
	files := cart("2")
	// A Kustomize overlay per cluster over a shared base: the render needs
	// files outside the app's own directory.
	if _, err := exec.LookPath("kubectl"); err != nil {
		if _, err := exec.LookPath("kustomize"); err != nil {
			t.Skip("neither kustomize nor kubectl installed")
		}
	}
	files["apps/edge/base/kustomization.yaml"] = "resources: [configmap.yaml]\n"
	files["apps/edge/base/configmap.yaml"] = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: edge, labels: {app.kubernetes.io/name: edge}}\ndata: {REGION: any}\n"
	files["apps/edge/overlays/lab-east/kustomization.yaml"] = "resources: [../../base]\npatches:\n  - patch: '{\"apiVersion\": \"v1\", \"kind\": \"ConfigMap\", \"metadata\": {\"name\": \"edge\"}, \"data\": {\"REGION\": \"east\"}}'\n"
	repo.commit("edge for lab-east", files)
	cluster := livetest.NewFakeKube(t)
	applier := &fakeApplier{}
	p, err := kube.New(cluster.Client, kube.Config{Applier: applier, RootSelector: "app.kubernetes.io/part-of=team",
		Source: &kube.Source{URL: repo.dir, Poll: time.Hour, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	edge := live.App{Key: "edge-ns/edge", Name: "edge", Namespace: "edge-ns", Template: "edge"}
	p.Declare([]live.DeclaredApp{{App: edge, Path: "apps/edge/overlays/lab-east", CreateNamespace: true}})
	tr := live.NewTracker(live.DefaultWindow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Watch(ctx, tr) }()

	var got *live.Delta
	deadline := time.Now().Add(10 * time.Second)
	for got == nil || got.Desired == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no desired state for the declared app: %+v", got)
		}
		ds, _ := tr.Take(1 << 30)
		for i := range ds {
			if ds[i].App.Key == edge.Key {
				d := ds[i]
				if got != nil && d.Desired == nil {
					d.Desired = got.Desired
				}
				got = &d
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.App.Template != "edge" || got.Desired.Source != "apps/edge/overlays/lab-east" || len(got.Desired.Manifests) != 1 {
		t.Fatalf("declared app delta = %+v", got)
	}
	if m := string(got.Desired.Manifests["/ConfigMap/edge-ns/edge"]); !strings.Contains(m, `"REGION":"east"`) {
		t.Errorf("overlay not applied: %s", m)
	}
	res := p.Operate(ctx, live.Op{Kind: live.OpSync, App: edge}, nil)
	if !res.OK || len(res.Resources) != 2 || res.Resources[0].Kind != "Namespace" || res.Resources[0].Action != "created" {
		t.Fatalf("sync = %+v", res)
	}
	if strings.Join(applier.applied, ",") != "Namespace/edge-ns,ConfigMap/edge" {
		t.Errorf("applied %v", applier.applied)
	}
}
