package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/buildinfo"
)

// Self-update: when the server asks (an admin pressed Upgrade, or "Upgrade
// all runners" reached this one), an idle runner downloads the server's
// bundled binary for its platform, checks it runs and reports a version,
// swaps it in next to itself and re-executes. Opt-in with
// RUNNER_SELF_UPDATE=on; a packaged runner (apt, Helm image) should update
// the way it was installed instead.

func selfUpdateEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_SELF_UPDATE")), "on")
}

// downloadName is the server's name for this platform's binary.
func downloadName() string {
	name := "forge-runner-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// selfUpdate replaces the running binary with the server's and re-execs.
// It returns only on failure (the old binary keeps running).
func selfUpdate(ctx context.Context, c *client, wantVersion string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".forge-runner-update-*")
	if err != nil {
		return fmt.Errorf("the runner's directory must be writable for self-update: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/downloads/"+downloadName(), nil)
	if err != nil {
		tmp.Close()
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		tmp.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("download returned %s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if want := resp.Header.Get("X-Checksum-Sha256"); want != "" && want != hex.EncodeToString(h.Sum(nil)) {
		return fmt.Errorf("downloaded binary checksum mismatch")
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// The new binary must run and report a version before it replaces us.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmpPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("downloaded binary does not run: %w", err)
	}
	got := strings.TrimSpace(string(out))
	if wantVersion != "" && got != wantVersion {
		return fmt.Errorf("downloaded binary reports version %q, server has %q", got, wantVersion)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	slog.Info("self-update: binary replaced; restarting", "from", buildinfo.Version, "to", got)
	return reexec(exe)
}

// reexec restarts this process with the same arguments. On Unix it replaces
// the process image; on Windows it starts the new binary and exits.
func reexec(exe string) error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			return err
		}
		os.Exit(0)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
