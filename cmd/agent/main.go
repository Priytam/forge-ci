// forge-agent: Forge Live's agent. It runs next to what it watches — inside
// a Kubernetes cluster under a read-only ServiceAccount, or anywhere with a
// Cloud Run viewer credential — and reports applications to Forge over an
// outbound connection. Forge never holds cluster or cloud credentials.
//
//	FORGE_URL=https://forge.example.com FORGE_AGENT_TOKEN=… forge-agent --provider kubernetes
//	forge-agent --provider cloudrun --project acme-prod --regions asia-south1,us-central1
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/oauth2/google"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/priytamjeepandey/forge-ci/internal/buildinfo"
	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/agent"
	"github.com/priytamjeepandey/forge-ci/internal/live/cloudrun"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
)

func main() {
	var (
		provider   = flag.String("provider", "kubernetes", "what to watch: kubernetes | cloudrun")
		kubeconfig = flag.String("kubeconfig", "", "kubeconfig to use outside a cluster (default: in-cluster)")
		appLabels  = flag.String("app-labels", "app.kubernetes.io/instance,app.kubernetes.io/name", "labels naming a top-level object's application; the first present wins")
		groupLabel = flag.String("group-label", "app.kubernetes.io/part-of", "label naming the system an application is part of")
		rootSel    = flag.String("root-selector", "", "label selector limiting which top-level objects are applications")
		excludeNS  = flag.String("exclude-namespaces", "kube-system,kube-public,kube-node-lease", "namespaces never watched")
		sourceURL  = flag.String("source-url", "", "Git repository holding desired state (enables Diff; GIT_TOKEN authenticates)")
		branch     = flag.String("source-branch", "main", "branch of the source")
		sourcePath = flag.String("source-path", "apps/{group}/{name}", "an application's directory in the source ({group}, {name}, {namespace})")
		sourcePoll = flag.Duration("source-poll", 30*time.Second, "how often the source is fetched")
		allowSync  = flag.Bool("allow-sync", false, "let Forge sync and prune (needs deploy/live/agent-sync.yaml)")
		project    = flag.String("project", "", "Cloud Run: GCP project")
		regions    = flag.String("regions", "", "Cloud Run: comma-separated regions")
		period     = flag.Duration("period", 2*time.Minute, "Cloud Run: time between reconciles")
		budget     = flag.Int("calls-per-minute", 60, "Cloud Run: API call budget")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	url, token := os.Getenv("FORGE_URL"), os.Getenv("FORGE_AGENT_TOKEN")
	if url == "" || token == "" {
		fatal("FORGE_URL and FORGE_AGENT_TOKEN are required")
	}
	// The footprint budget is 150 MB; collect garbage before getting near it.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(kube.AgentMemoryLimit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var p live.Provider
	switch *provider {
	case "kubernetes":
		cfg, err := restConfig(*kubeconfig)
		if err != nil {
			fatal("kubernetes config: %v", err)
		}
		client, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			fatal("kubernetes client: %v", err)
		}
		kc := kube.Config{AppLabels: split(*appLabels), GroupLabel: *groupLabel, RootSelector: *rootSel, ExcludeNamespaces: split(*excludeNS)}
		if *sourceURL != "" {
			kc.Source = &kube.Source{URL: *sourceURL, Branch: *branch, Path: *sourcePath, Poll: *sourcePoll, Token: os.Getenv("GIT_TOKEN")}
		}
		if *allowSync {
			jsonCfg := rest.CopyConfig(cfg)
			jsonCfg.ContentType, jsonCfg.AcceptContentTypes = "", ""
			dyn, err := dynamic.NewForConfig(jsonCfg)
			if err != nil {
				fatal("dynamic client: %v", err)
			}
			kc.Applier = kube.NewApplier(dyn, client.Discovery())
		}
		if p, err = kube.New(client, kc); err != nil {
			fatal("%v", err)
		}
	case "cloudrun":
		ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			fatal("google credentials: %v", err)
		}
		if p, err = cloudrun.New(cloudrun.Config{Project: *project, Regions: split(*regions), Period: *period,
			CallsPerMinute: *budget, Tokens: ts, AllowSync: *allowSync}); err != nil {
			fatal("%v", err)
		}
	default:
		fatal("unknown provider %q", *provider)
	}
	slog.Info("forge-agent starting", "version", buildinfo.Version, "provider", p.Name(), "forge", url)
	if err := agent.Run(ctx, agent.Config{Provider: p, URL: url, Token: token, Version: buildinfo.Version}); err != nil {
		fatal("%v", err)
	}
}

// restConfig is the in-cluster config (or a kubeconfig), speaking protobuf:
// it decodes several times faster than JSON at startup.
func restConfig(kubeconfig string) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	cfg.ContentType = "application/vnd.kubernetes.protobuf"
	cfg.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	cfg.UserAgent = "forge-agent/" + buildinfo.Version
	return cfg, nil
}

func split(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func fatal(format string, a ...any) {
	slog.Error(fmt.Sprintf(format, a...))
	os.Exit(1)
}
