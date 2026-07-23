package executor

import (
	"context"
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

func (k *kubeExecutor) Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error) {
	image := job.Image
	if image == "" {
		image = defaultImage
	}
	pod := fmt.Sprintf("forge-job-%d", job.ID)

	// 1. Ephemeral pod, parked on sleep. Bounded so an orphaned pod
	//    self-terminates even if cleanup never runs.
	if err := k.runQuiet(ctx, "run", pod, "--image", image, "--restart=Never",
		"--labels=app=forge-ci-job", "--command", "--", "sleep", "7200"); err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = k.runQuiet(dctx, "delete", "pod", pod, "--wait=false", "--ignore-not-found")
	}

	// 2. Wait for the pod, 3. ship the workspace in.
	if err := k.runQuiet(ctx, "wait", "--for=condition=Ready", "--timeout=180s", "pod/"+pod); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := k.runQuiet(ctx, "cp", workdir+"/.", pod+":/workspace"); err != nil {
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

	cmd := k.kubectl(ctx, "exec", "-i", pod, "--", "sh", "-ce", sb.String())
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
				_ = k.runQuiet(cctx, "cp", pod+":/workspace/"+clean, workdir+"/"+clean)
			}
			cancel()
		}
		cleanup()
		return code
	}
	return pr, waitAndCollect, nil
}
