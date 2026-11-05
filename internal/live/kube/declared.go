package kube

import (
	"context"
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Declared applications (phase D): the ones Forge's templates put on this
// cluster. The agent renders each from its template's path even when
// nothing of it exists yet — every desired object shows as Missing — so a
// sync (auto-sync, with the template's policy) creates it. An object of a
// declared app joins it by its labels like any other, root selector or not.

// Declare installs the cluster's declared applications.
func (p *Provider) Declare(apps []live.DeclaredApp) {
	m := make(map[string]live.DeclaredApp, len(apps))
	for _, d := range apps {
		m[d.App.Key] = d
	}
	p.mu.Lock()
	p.declared = m
	w := p.current
	p.mu.Unlock()
	if w != nil {
		w.declare(m)
	}
}

// declare replaces the declared apps: new ones are rendered at once,
// dropped ones that have nothing in the cluster are forgotten.
func (w *watcher) declare(m map[string]live.DeclaredApp) {
	w.mu.Lock()
	for key := range w.declared {
		if _, still := m[key]; still {
			continue
		}
		if a := w.apps[key]; a != nil {
			a.Template = ""
			if len(w.rootsByApp[key]) == 0 {
				if d := w.desired[key]; d != nil {
					for k := range d.missing {
						w.sink.Delete(*a, live.DesiredPrefix+k)
					}
				}
				w.sink.Desired(*a, live.Desired{}) // no longer declared: the tracker lets it go
				delete(w.desired, key)
				delete(w.apps, key)
			}
		}
	}
	w.declared = maps.Clone(m)
	for key, d := range m {
		a := w.apps[key]
		if a == nil {
			a = &live.App{Key: key, Name: d.App.Name, Namespace: d.App.Namespace, Group: d.App.Group}
			w.apps[key] = a
		}
		a.Template = d.App.Template
	}
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// pathOf is an app's directory in the source: its template's, or the
// agent's pattern (w.mu held).
func (w *watcher) pathOf(a live.App) string {
	if d, ok := w.declared[a.Key]; ok {
		return d.Path
	}
	return w.p.repo.appPath(a)
}

// ensureNamespace creates a declared app's namespace when it is missing.
func (s *syncRun) ensureNamespace(ctx context.Context, ns string) (live.ResourceResult, bool) {
	rr := live.ResourceResult{Key: live.Key("", "Namespace", "", ns), Kind: "Namespace", Name: ns}
	u, err := s.applier.Get(ctx, "", "v1", "Namespace", "", ns)
	if err != nil || u != nil {
		return rr, false
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": ns, "labels": map[string]any{"app.kubernetes.io/managed-by": "forge"}},
	}}
	if _, err := s.applier.Apply(ctx, obj, false, false); err != nil {
		rr.Action, rr.Message = "failed", err.Error()
		return rr, true
	}
	rr.Action = "created"
	return rr, true
}
