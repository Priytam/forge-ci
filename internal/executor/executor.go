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

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const defaultImage = "alpine:3"

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
	default:
		return nil, fmt.Errorf("unknown executor %q (want shell or docker)", kind)
	}
}

func start(cmd *exec.Cmd) (io.ReadCloser, func() int, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
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
	cmd := exec.CommandContext(ctx, "sh", "-ce", job.Script)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), ciEnv(job)...)
	return start(cmd)
}

type dockerExecutor struct{}

func (dockerExecutor) Name() string { return "docker" }

func (dockerExecutor) Start(ctx context.Context, job *proto.RunnerJob, workdir string) (io.ReadCloser, func() int, error) {
	image := job.Image
	if image == "" {
		image = defaultImage
	}
	// The workspace is bind-mounted so artifacts land on the host for
	// collection after the container exits.
	args := []string{"run", "--rm", "--network", "none",
		"-v", workdir + ":/workspace", "-w", "/workspace"}
	for _, kv := range ciEnv(job) {
		args = append(args, "-e", kv)
	}
	args = append(args, image, "sh", "-ce", job.Script)
	return start(exec.CommandContext(ctx, "docker", args...))
}
