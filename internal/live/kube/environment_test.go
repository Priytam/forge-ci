package kube

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// TestEnvironmentLabel: an app's environment is the environment label on
// its top-level objects when they agree; objects without it do not count;
// when they disagree the app has no label environment and says which values
// conflict; fixing or deleting the odd object settles it again.
func TestEnvironmentLabel(t *testing.T) {
	w, rec := newWatcher(t, Config{})
	lbl := func(env string) map[string]string {
		l := map[string]string{"app.kubernetes.io/name": "cart-api"}
		if env != "" {
			l[DefaultEnvironmentLabel] = env
		}
		return l
	}
	app := func() (string, []string) {
		a := rec.apps["checkout/cart-api"]
		return a.Environment, a.EnvironmentConflict
	}
	w.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "", lbl("production"))})
	w.apply(kindNamed("Service"), &corev1.Service{ObjectMeta: owned("cart-api", "svc1", "", "", lbl(""))})
	// A child's label never counts: only top-level objects say.
	w.apply(kindNamed("ReplicaSet"), &appsv1.ReplicaSet{ObjectMeta: owned("cart-api-x", "rs1", "Deployment", "dep1", lbl("staging"))})
	if env, c := app(); env != "production" || c != nil {
		t.Fatalf("agreeing labels: %q %v, want production", env, c)
	}

	w.apply(kindNamed("ConfigMap"), &corev1.ConfigMap{ObjectMeta: owned("cart-api", "cm1", "", "", lbl("staging"))})
	if env, c := app(); env != "" || !slices.Equal(c, []string{"production", "staging"}) {
		t.Fatalf("conflicting labels: %q %v, want none and [production staging]", env, c)
	}

	// Relabelling the odd object settles it.
	w.apply(kindNamed("ConfigMap"), &corev1.ConfigMap{ObjectMeta: owned("cart-api", "cm1", "", "", lbl("production"))})
	if env, c := app(); env != "production" || c != nil {
		t.Fatalf("after relabel: %q %v", env, c)
	}
	// So does deleting it; deleting the last labelled object clears it.
	w.apply(kindNamed("ConfigMap"), &corev1.ConfigMap{ObjectMeta: owned("cart-api", "cm1", "", "", lbl("staging"))})
	w.remove("cm1")
	if env, c := app(); env != "production" || c != nil {
		t.Fatalf("after delete: %q %v", env, c)
	}
	w.remove("dep1")
	if env, c := app(); env != "" || c != nil {
		t.Fatalf("no labelled objects left: %q %v", env, c)
	}

	// "-" turns the label off; a custom key works.
	w2, rec2 := newWatcher(t, Config{EnvironmentLabel: "-"})
	w2.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "", lbl("production"))})
	if a := rec2.apps["checkout/cart-api"]; a.Environment != "" {
		t.Errorf("label off: %q", a.Environment)
	}
	w3, rec3 := newWatcher(t, Config{EnvironmentLabel: "acme.io/env"})
	w3.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "",
		map[string]string{"app.kubernetes.io/name": "cart-api", "acme.io/env": "qa"})})
	if a := rec3.apps["checkout/cart-api"]; a.Environment != "qa" {
		t.Errorf("custom label: %q", a.Environment)
	}
}
