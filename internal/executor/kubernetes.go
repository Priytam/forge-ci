package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// kubeExecutor is the GitLab-style Kubernetes executor: the runner process is
// a resident manager, and every job runs in its own ephemeral Pod —
// created on acquire, deleted after the job, nothing fixed per job.
//
// Lifecycle per job:
//  1. kubectl run forge-job-<id> (job image, `sleep <timeout>` entrypoint)
//  2. kubectl wait --for=condition=Ready
//  3. kubectl cp <workspace> -> pod:/workspace   (source + restored artifacts)
//  4. kubectl exec sh -ce '<env exports> + cd /workspace + script'  — streamed
//  5. kubectl cp pod:/workspace/<artifact paths> -> workspace       (collection)
//  6. kubectl delete pod (always, asynchronous)
//
// KUBE_CONTEXT is REQUIRED — the executor refuses to run against the
// kubeconfig's current context to prevent accidents with production clusters.
// The sentinel value "in-cluster" runs kubectl with no --context flag, which
// makes it use the pod's ServiceAccount when the manager runs inside the
// target cluster. KUBE_NAMESPACE defaults to "default". Job images need
// sh + tar (kubectl cp uses tar), which alpine and most build images have.
type kubeExecutor struct {
	kubeContext string // "" means in-cluster (ServiceAccount auth)
	namespace   string
}

func newKubeExecutor() (Executor, error) {
	kctx := os.Getenv("KUBE_CONTEXT")
	if kctx == "" {
		return nil, fmt.Errorf("kubernetes executor requires KUBE_CONTEXT to be set explicitly " +
			"(a named kubeconfig context, or \"in-cluster\" to use the pod ServiceAccount)")
	}
	if kctx == "in-cluster" {
		kctx = ""
	}
	ns := os.Getenv("KUBE_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return &kubeExecutor{kubeContext: kctx, namespace: ns}, nil
}

func (k *kubeExecutor) Name() string { return "kubernetes" }

func (k *kubeExecutor) kubectl(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"-n", k.namespace}
	if k.kubeContext != "" {
		base = append([]string{"--context", k.kubeContext}, base...)
	}
	return exec.CommandContext(ctx, "kubectl", append(base, args...)...)
}

func (k *kubeExecutor) runQuiet(ctx context.Context, args ...string) error {
	out, err := k.kubectl(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

// posixQuote single-quotes s for POSIX sh (no expansion inside).
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// servicesPodManifest builds a Pod manifest (JSON) for a job with sidecar
// services: the main "job" container parked on sleep, one container per service,
// and a hostAliases entry mapping every service alias to 127.0.0.1. All
// containers share the pod network namespace, so services listen on localhost
// and the alias hostnames resolve there — the same `-h <alias>` the docker
// executor supports. Pod deletion (cleanup) tears everything down.
func servicesPodManifest(pod, image string, services []proto.ServiceSpec) (string, error) {
	type k8sEnv struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type k8sContainer struct {
		Name    string   `json:"name"`
		Image   string   `json:"image"`
		Command []string `json:"command,omitempty"`
		Args    []string `json:"args,omitempty"`
		Env     []k8sEnv `json:"env,omitempty"`
	}
	type hostAlias struct {
		IP        string   `json:"ip"`
		Hostnames []string `json:"hostnames"`
	}

	containers := []k8sContainer{{
		Name:    "job",
		Image:   image,
		Command: []string{"sleep", "7200"},
	}}
	aliases := make([]string, 0, len(services))
	for i, svc := range services {
		c := k8sContainer{
			Name:  fmt.Sprintf("svc-%d", i),
			Image: svc.Image,
			Args:  svc.Cmd, // optional command override -> container args
		}
		keys := make([]string, 0, len(svc.Env))
		for k := range svc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			c.Env = append(c.Env, k8sEnv{Name: key, Value: svc.Env[key]})
		}
		containers = append(containers, c)
		aliases = append(aliases, svc.Alias)
	}

	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":   pod,
			"labels": map[string]string{"app": "forge-ci-job"},
		},
		"spec": map[string]any{
			"restartPolicy": "Never",
			"containers":    containers,
			"hostAliases":   []hostAlias{{IP: "127.0.0.1", Hostnames: aliases}},
		},
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (k *kubeExecutor) Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error) {
	image := job.Image
	if image == "" {
		image = defaultImage
	}
	pod := fmt.Sprintf("forge-job-%d", job.ID)

	cleanup := func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k.runQuiet(dctx, "delete", "pod", pod, "--wait=false", "--ignore-not-found")
	}

	// 1. Ephemeral pod, parked on sleep. Bounded so an orphaned pod
	//    self-terminates even if cleanup never runs.
	//
	//    With services, the pod holds extra containers (one per service) sharing
	//    the pod's network namespace, and hostAliases map each alias to 127.0.0.1
	//    so the SAME script (`psql -h db`) works on docker and kubernetes: the
	//    alias resolves to localhost, where the service listens. The main
	//    container is named "job" so cp/exec target it explicitly. Without
	//    services we keep the simpler single-container `kubectl run`.
	container := "" // container flag for cp/exec; empty = pod's only container
	if len(job.Services) == 0 {
		if err := k.runQuiet(ctx, "run", pod, "--image", image, "--restart=Never",
			"--labels=app=forge-ci-job", "--command", "--", "sleep", "7200"); err != nil {
			return nil, nil, err
		}
	} else {
		container = "job"
		manifest, err := servicesPodManifest(pod, image, job.Services)
		if err != nil {
			return nil, nil, err
		}
		apply := k.kubectl(ctx, "apply", "-f", "-")
		apply.Stdin = strings.NewReader(manifest)
		if out, aerr := apply.CombinedOutput(); aerr != nil {
			return nil, nil, fmt.Errorf("create pod with services: %s", strings.TrimSpace(string(out)))
		}
	}

	// 2. Wait for the pod (all containers Ready), 3. ship the workspace in.
	if err := k.runQuiet(ctx, "wait", "--for=condition=Ready", "--timeout=180s", "pod/"+pod); err != nil {
		cleanup()
		return nil, nil, err
	}
	cpArgs := []string{"cp", workdir + "/.", pod + ":/workspace"}
	if container != "" {
		cpArgs = append(cpArgs, "-c", container)
	}
	if err := k.runQuiet(ctx, cpArgs...); err != nil {
		cleanup()
		return nil, nil, err
	}

	// 4. Exec the script with env exported inside the pod. kubectl exec
	//    propagates the remote exit code.
	var sb strings.Builder
	env := job.Env
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&sb, "export %s=%s\n", key, posixQuote(env[key]))
	}
	fmt.Fprintf(&sb, "export CI=true CI_PIPELINE_ID=%d CI_JOB_ID=%d CI_JOB_NAME=%s\n",
		job.PipelineID, job.ID, posixQuote(job.Name))
	sb.WriteString("cd /workspace\n")
	sb.WriteString(job.Script)

	execArgs := []string{"exec", "-i", pod}
	if container != "" {
		execArgs = append(execArgs, "-c", container)
	}
	execArgs = append(execArgs, "--", "sh", "-ce", sb.String())
	cmd := k.kubectl(ctx, execArgs...)
	pr, wait, err := start(cmd)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	// 5+6. After the script: pull declared artifacts back out, then delete.
	waitAndCollect := func() int {
		code := wait()
		if code == 0 {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			for _, p := range job.ArtifactPaths {
				clean := strings.TrimSuffix(strings.TrimSpace(p), "/")
				if clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
					continue
				}
				// Missing paths are tolerated; uploadArtifacts logs skips.
				cpOut := []string{"cp", pod + ":/workspace/" + clean, workdir + "/" + clean}
				if container != "" {
					cpOut = append(cpOut, "-c", container)
				}
				_ = k.runQuiet(cctx, cpOut...)
			}
			cancel()
		}
		cleanup()
		return code
	}
	return pr, waitAndCollect, nil
}
