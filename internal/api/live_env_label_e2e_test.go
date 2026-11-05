package api

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/agent"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestLiveEnvironmentLabelEndToEnd: the real agent and Kubernetes provider
// read forge.dev/environment off an app's objects in a (fake) cluster, the
// hub stores it, and the API shows the app in that environment; a
// disagreeing object flags the app and the environment falls back.
func TestLiveEnvironmentLabelEndToEnd(t *testing.T) {
	h := newLiveHarness(t)
	cluster := livetest.NewFakeKube(t)
	cluster.Deploy(t, "cart-api", live.Healthy)
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Run(ctx, agent.Config{Provider: cluster.Provider(t), URL: srv.URL, Token: h.token, Heartbeat: time.Second})
	}()
	defer func() { cancel(); <-done }()

	label := func(kind, name, env string) {
		t.Helper()
		c := cluster.Client
		var err error
		switch kind {
		case "Deployment":
			o, _ := c.AppsV1().Deployments("team").Get(ctx, name, metav1.GetOptions{})
			o.Labels[kube.DefaultEnvironmentLabel] = env
			_, err = c.AppsV1().Deployments("team").Update(ctx, o, metav1.UpdateOptions{})
		case "Service":
			o, _ := c.CoreV1().Services("team").Get(ctx, name, metav1.GetOptions{})
			o.Labels[kube.DefaultEnvironmentLabel] = env
			_, err = c.CoreV1().Services("team").Update(ctx, o, metav1.UpdateOptions{})
		case "ConfigMap":
			o, _ := c.CoreV1().ConfigMaps("team").Get(ctx, name, metav1.GetOptions{})
			o.Labels[kube.DefaultEnvironmentLabel] = env
			_, err = c.CoreV1().ConfigMaps("team").Update(ctx, o, metav1.UpdateOptions{})
		}
		if err != nil {
			t.Fatalf("label %s/%s: %v", kind, name, err)
		}
	}
	cart := func() store.LiveApp {
		for _, a := range h.apps("").Apps {
			if a.Name == "cart-api" {
				return a
			}
		}
		return store.LiveApp{}
	}
	eventually(t, "the app", func() bool { return cart().ID != 0 })
	if a := cart(); a.Environment != "fleet" || a.EnvironmentSource != store.EnvFromTarget {
		t.Fatalf("unlabelled: %q from %q", a.Environment, a.EnvironmentSource)
	}

	label("Deployment", "cart-api", "production")
	label("Service", "cart-api", "production")
	eventually(t, "the label environment", func() bool {
		a := cart()
		return a.Environment == "production" && a.EnvironmentSource == store.EnvFromLabel
	})
	if p := h.apps("?environment=production"); p.Total != 1 {
		t.Errorf("filter by the label's environment: %+v", p.Apps)
	}

	label("ConfigMap", "cart-api-config", "staging")
	eventually(t, "the conflict flagged", func() bool {
		a := cart()
		return a.Environment == "fleet" && slices.Equal(a.EnvironmentConflict, []string{"production", "staging"})
	})
}
