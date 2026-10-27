package kube

import (
	"encoding/json"
	"testing"
)

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Drift is what Git sets and the cluster does not have; what the API
// server fills in (defaults, status, uid) never counts.
func TestInSync(t *testing.T) {
	live := obj(t, `{"apiVersion":"apps/v1","kind":"Deployment",
		"metadata":{"name":"cart","namespace":"checkout","uid":"u1","resourceVersion":"9","labels":{"app":"cart","extra":"x"},"annotations":{"deployment.kubernetes.io/revision":"3"}},
		"spec":{"replicas":2,"revisionHistoryLimit":10,"template":{"spec":{"containers":[{"name":"app","image":"cart:v1","ports":[{"containerPort":8080,"protocol":"TCP"}],
		"resources":{"requests":{"cpu":"500m","memory":"16Mi"}}}]}}},
		"status":{"replicas":2}}`)
	cases := []struct {
		name, desired string
		ok            bool
		drift         string
	}{
		{"defaults and status ignored", `{"metadata":{"name":"cart","labels":{"app":"cart"}},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"cart:v1","ports":[{"containerPort":8080}]}]}}}}`, true, ""},
		{"quantities by amount", `{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"requests":{"cpu":"0.5","memory":"16Mi"}}}]}}}}`, true, ""},
		{"replicas drift", `{"spec":{"replicas":3}}`, false, "spec.replicas"},
		{"image drift", `{"spec":{"template":{"spec":{"containers":[{"name":"app","image":"cart:v2"}]}}}}`, false, "spec.template.spec.containers[0].image"},
		{"a missing label", `{"metadata":{"labels":{"team":"checkout"}}}`, false, "metadata.labels"},
		{"an extra container in Git", `{"spec":{"template":{"spec":{"containers":[{"name":"app"},{"name":"sidecar"}]}}}}`, false, "spec.template.spec.containers"},
		{"null means unset", `{"spec":{"paused":null}}`, true, ""},
	}
	for _, c := range cases {
		ok, drift := inSync(obj(t, c.desired), live)
		if ok != c.ok || drift != c.drift {
			t.Errorf("%s: inSync = %v %q, want %v %q", c.name, ok, drift, c.ok, c.drift)
		}
	}
}
