//go:build windows

package executor

import (
	"io"
	"os/exec"
	"time"
)

// Windows has no POSIX process groups, so an unkillable grandchild (e.g. a
// background `sleep`) can in principle outlive a canceled job — unlike Unix,
// where the whole group is killed. cmd.Process.Kill() (TerminateProcess) is
// the best available fallback; cmd.WaitDelay still bounds how long Wait can
// block on it.
func start(cmd *exec.Cmd) (io.ReadCloser, func() int, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
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
