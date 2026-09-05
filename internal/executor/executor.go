// Package executor runs a job's script and streams its combined output.
// Executors are the isolation boundary: shell for trusted local dev,
// docker for containerized runs. A production system would add kubernetes
// and microVM executors behind this same interface.
package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const defaultImage = "alpine:3"

// defaultNetwork is the container network a service-less docker job runs on
// when it declares no `network:`.
//
// It is the default bridge rather than `none`. Running every job in an empty
// network namespace left them with no DNS and no egress, which broke any tool
// that has to fetch something — including Forge's own security/* built-ins
// (semgrep pulls rules from semgrep.dev, trivy downloads its vulnerability
// database) — while every other CI runner gives jobs network by default. Jobs
// that genuinely want isolation ask for it with `network: none`.
const defaultNetwork = "bridge"

// jobNetwork resolves the docker network for a service-less job.
func jobNetwork(job *proto.RunnerJob) string {
	if job.Network != "" {
		return job.Network
	}
	return defaultNetwork
}

type Executor interface {
	Name() string
	// Start launches the job in workdir (the job's workspace — artifacts are
	// collected from it afterwards) and returns a reader of combined
	// stdout+stderr and a wait function returning the exit code.
	Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error)
}

func New(kind string) (Executor, error) {
	switch kind {
	case "shell":
		return shellExecutor{}, nil
	case "docker":
		return dockerExecutor{}, nil
	case "kubernetes":
		return newKubeExecutor()
	default:
		return nil, fmt.Errorf("unknown executor %q (want shell, docker or kubernetes)", kind)
	}
}

func start(cmd *exec.Cmd) (io.ReadCloser, func() int, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	// Kill the whole process group on cancel/timeout — killing only the
	// shell leaves grandchildren (e.g. `sleep`) holding the output pipe,
	// which would block Wait until they exit on their own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Even if something survives the group kill, stop waiting for the pipe.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	// Reap the process from a goroutine and close the write end there —
	// the caller reads the pipe to EOF *before* calling wait, so closing
	// inside wait would deadlock: EOF never arrives while wait is pending.
	done := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		switch e := err.(type) {
		case nil:
			done <- 0
		case *exec.ExitError:
			done <- e.ExitCode()
		default:
			done <- 1
		}
	}()
	return pr, func() int { return <-done }, nil
}

func ciEnv(job *proto.RunnerJob) []string {
	env := []string{
		"CI=true",
		fmt.Sprintf("CI_PIPELINE_ID=%d", job.PipelineID),
		fmt.Sprintf("CI_JOB_ID=%d", job.ID),
		fmt.Sprintf("CI_JOB_NAME=%s", job.Name),
	}
	for k, v := range job.Env {
		env = append(env, k+"="+v)
	}
	return env
}

type shellExecutor struct{}

func (shellExecutor) Name() string { return "shell" }

func (shellExecutor) Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error) {
	// Services require a container runtime and a per-job network; the shell
	// executor has neither. Reject rather than silently drop them.
	if len(job.Services) > 0 {
		return nil, nil, fmt.Errorf("shell executor cannot run service containers: "+
			"this job declares %d service(s); route it to a docker or kubernetes runner (add matching runner tags)", len(job.Services))
	}
	cmd := exec.CommandContext(ctx, "sh", "-ce", job.Script)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), ciEnv(job)...)
	return start(cmd)
}

type dockerExecutor struct{}

func (dockerExecutor) Name() string { return "docker" }

// dockerRunArgs builds the `docker run` argv for a job container.
//
// The script is run through `sh -ce` as the container's ENTRYPOINT, not as its
// command. Passing it as the command leaves the image's own ENTRYPOINT in
// place, so an image that declares one (aquasec/trivy is `["trivy"]`,
// zricethezav/gitleaks is `["gitleaks"]`) receives the shell invocation as
// arguments — `trivy sh -ce <script>` — and fails with `unknown command "sh"`.
// Forcing --entrypoint means a job's script runs the same way whatever the
// image does, which is the contract every other executor already keeps.
//
// Both the service-less and the with-services paths go through here so they
// cannot drift apart again.
func dockerRunArgs(job *proto.RunnerJob, workdir, image, name, network string, extraEnv ...string) []string {
	args := []string{"run", "--rm", "--name", name, "--network", network,
		"-v", workdir + ":/workspace", "-w", "/workspace"}
	for _, kv := range ciEnv(job) {
		args = append(args, "-e", kv)
	}
	for _, kv := range extraEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, "--entrypoint", "sh", image, "-ce", job.Script)
	return args
}

func (dockerExecutor) Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error) {
	image := job.Image
	if image == "" {
		image = defaultImage
	}
	name := fmt.Sprintf("forge-job-%d", job.ID)

	// No services: a single container on the job's chosen network.
	if len(job.Services) == 0 {
		// The workspace is bind-mounted so artifacts land on the host for
		// collection after the container exits.
		args := dockerRunArgs(job, workdir, image, name, jobNetwork(job))
		pr, wait, err := start(exec.CommandContext(ctx, "docker", args...))
		if err != nil {
			return nil, nil, err
		}
		// Killing the docker CLI does not stop the container — remove it
		// explicitly when the context ended (timeout/shutdown).
		waitCleanup := func() int {
			code := wait()
			if ctx.Err() != nil {
				rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = exec.CommandContext(rctx, "docker", "rm", "-f", name).Run()
			}
			return code
		}
		return pr, waitCleanup, nil
	}

	return startDockerWithServices(ctx, job, workdir, image, name)
}

// startDockerWithServices runs a job that declares sidecar services. It creates
// a dedicated user-defined bridge network, starts each service container on it
// with its alias as a network-alias (so the job reaches it by hostname, e.g.
// `psql -h db`), waits for the containers to be running (and healthy when the
// image ships a HEALTHCHECK), then runs the job container on the same network.
// Everything — services, job container, network — is torn down in all exit
// paths (success, failure, timeout, cancel, and any error during setup).
func startDockerWithServices(ctx context.Context, job *proto.RunnerJob, workdir, image, name string) (io.ReadCloser, func() int, error) {
	network := fmt.Sprintf("forge-net-%d", job.ID)

	// Best-effort teardown of every resource this job created. Safe to call
	// repeatedly; errors (already-gone resources) are ignored.
	svcNames := make([]string, len(job.Services))
	for i := range job.Services {
		svcNames[i] = fmt.Sprintf("forge-svc-%d-%d", job.ID, i)
	}
	cleanup := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(rctx, "docker", "rm", "-f", name).Run()
		for _, sn := range svcNames {
			_ = exec.CommandContext(rctx, "docker", "rm", "-f", sn).Run()
		}
		// Remove the network last, once nothing is attached.
		_ = exec.CommandContext(rctx, "docker", "network", "rm", network).Run()
	}

	// Create the per-job network. A stale one from a crashed prior run is
	// removed first so create doesn't fail.
	_ = exec.CommandContext(ctx, "docker", "network", "rm", network).Run()
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", network).CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("create service network: %s", strings.TrimSpace(string(out)))
	}

	// Start each service container detached, attached to the network under its
	// alias hostname.
	for i, svc := range job.Services {
		args := []string{"run", "-d", "--rm", "--name", svcNames[i],
			"--network", network, "--network-alias", svc.Alias}
		for k, v := range svc.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, svc.Image)
		args = append(args, svc.Cmd...) // optional command override
		if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("start service %q (%s): %s", svc.Alias, svc.Image, strings.TrimSpace(string(out)))
		}
	}

	// Readiness: wait for each service container to report Running (and, when the
	// image declares a HEALTHCHECK, healthy), bounded to 60s. The job script is
	// still expected to poll the service protocol (e.g. `until pg_isready`) —
	// container-running does not guarantee the service inside is accepting
	// connections. See docs/pipeline-dsl.md.
	for i, svc := range job.Services {
		if err := waitServiceReady(ctx, svcNames[i], 60*time.Second); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("service %q (%s) not ready: %v", svc.Alias, svc.Image, err)
		}
	}

	// Run the job container on the same network so the script resolves service
	// aliases. A convenience env var lists the aliases available.
	aliases := make([]string, len(job.Services))
	for i, svc := range job.Services {
		aliases[i] = svc.Alias
	}
	args := dockerRunArgs(job, workdir, image, name, network,
		"FORGE_SERVICE_ALIASES="+strings.Join(aliases, ","))

	pr, wait, err := start(exec.CommandContext(ctx, "docker", args...))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	waitCleanup := func() int {
		code := wait()
		cleanup() // always tear down services + network (and the job container)
		return code
	}
	return pr, waitCleanup, nil
}

// waitServiceReady polls `docker inspect` until the container is running (and,
// if it has a healthcheck, healthy), or the timeout elapses.
func waitServiceReady(ctx context.Context, container string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const format = "{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"
	for {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", format, container).CombinedOutput()
		if err == nil {
			fields := strings.Fields(strings.TrimSpace(string(out)))
			if len(fields) == 2 {
				running, health := fields[0], fields[1]
				switch {
				case running != "true":
					// still starting or has already exited
					if exited, _ := containerExited(ctx, container); exited {
						return fmt.Errorf("container exited before becoming ready")
					}
				case health == "none" || health == "healthy":
					return nil
				case health == "unhealthy":
					return fmt.Errorf("healthcheck reported unhealthy")
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// containerExited reports whether the container's state is no longer running and
// it is not merely still being created (i.e. it ran and stopped).
func containerExited(ctx context.Context, container string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Status}}", container).CombinedOutput()
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(out))
	return status == "exited" || status == "dead", nil
}
