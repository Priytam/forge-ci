//go:build !windows

package executor

import (
	"io"
	"os/exec"
	"syscall"
	"time"
)

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
