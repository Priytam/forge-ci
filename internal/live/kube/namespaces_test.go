package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestNamespaceScope(t *testing.T) {
	for _, c := range []struct {
		name             string
		include, exclude []string
		selector         string
		filtered         bool
	}{
		{"nothing", nil, nil, "", false},
		{"exclude only", nil, []string{"kube-system", "kube-public"}, "metadata.namespace!=kube-system,metadata.namespace!=kube-public", false},
		{"one include: the API server filters", []string{"travafa-prod"}, []string{"kube-system"}, "metadata.namespace=travafa-prod", false},
		{"several: filtered in the agent", []string{"a", "b"}, []string{"kube-system"}, "metadata.namespace!=kube-system", true},
		{"exclude wins over include", []string{"a", "kube-system"}, []string{"kube-system"}, "metadata.namespace=a", false},
	} {
		sel, keep := namespaceScope(c.include, c.exclude)
		if sel != c.selector || (keep != nil) != c.filtered {
			t.Errorf("%s: selector %q filtered %v, want %q %v", c.name, sel, keep != nil, c.selector, c.filtered)
		}
	}
	_, keep := namespaceScope([]string{"a", "b", "kube-system"}, []string{"kube-system"})
	if !keep("a") || !keep("b") || keep("kube-system") || keep("brand-new-system-ns") {
		t.Error("include filter: want a and b only")
	}
}

// TestIncludeNamespacesFilter: with several included namespaces, objects in
// any other namespace never reach the agent's store, from the list or the
// watch.
func TestIncludeNamespacesFilter(t *testing.T) {
	pod := func(ns, name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("u-" + ns + "-" + name)}}
	}
	client := fake.NewClientset(pod("staging", "a"), pod("prod", "b"), pod("monitoring", "c"))
	_, keep := namespaceScope([]string{"staging", "prod"}, nil)
	lw := &nsFilter{inner: cache.ToListerWatcherWithContext(kindNamed("Pod").lw(client, "")), keep: keep}
	ctx := context.Background()
	obj, err := lw.ListWithContext(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range obj.(*corev1.PodList).Items {
		got = append(got, p.Namespace)
	}
	if len(got) != 2 || got[0] == "monitoring" || got[1] == "monitoring" {
		t.Fatalf("listed namespaces %v, want staging and prod", got)
	}

	w, err := lw.WatchWithContext(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	_, _ = client.CoreV1().Pods("monitoring").Create(ctx, pod("monitoring", "d"), metav1.CreateOptions{})
	_, _ = client.CoreV1().Pods("prod").Create(ctx, pod("prod", "e"), metav1.CreateOptions{})
	select {
	case e := <-w.ResultChan():
		if p := e.Object.(*corev1.Pod); p.Namespace != "prod" || p.Name != "e" {
			t.Fatalf("watch delivered %s/%s, want prod/e", p.Namespace, p.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no watch event for the included namespace")
	}
}
