//go:build !windows

package executor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// fakeDocker puts a `docker` on PATH that appends each invocation's arguments
// to a log (one line per call), reports every container running, and succeeds.
func fakeDocker(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\n" +
		"case \"$1\" in inspect) echo 'true none' ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// dockerRuns returns the logged `docker run -d` calls (service containers)
// and the `docker run --rm` call (the job container).
func dockerRuns(t *testing.T, logPath string) (services []string, job string) {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(line, "run -d "):
			services = append(services, line)
		case strings.HasPrefix(line, "run --rm "):
			job = line
		}
	}
	return services, job
}

func runWithDind(t *testing.T, e dockerExecutor) (services []string, job string) {
	t.Helper()
	logPath := fakeDocker(t)
	j := &proto.RunnerJob{ID: 7, Image: "docker:27", Script: "docker info",
		Services: []proto.ServiceSpec{{Image: "docker:27-dind", Alias: "docker"}}}
	out, wait, err := e.Start(context.Background(), j, t.TempDir())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = io.Copy(io.Discard, out)
	if code := wait(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	return dockerRuns(t, logPath)
}

// docker-in-docker needs a privileged service container; without the runner
// opt-in no container is privileged, and with it only the services are.
func TestDockerServicesPrivilegedOnlyWhenRunnerOptsIn(t *testing.T) {
	services, job := runWithDind(t, dockerExecutor{})
	if len(services) != 1 || strings.Contains(services[0], "--privileged") {
		t.Fatalf("default service run = %q, want one unprivileged run", services)
	}

	services, job = runWithDind(t, dockerExecutor{privilegedServices: true})
	if len(services) != 1 || !strings.Contains(services[0], "--privileged") {
		t.Fatalf("opted-in service run = %q, want --privileged", services)
	}
	if job == "" || strings.Contains(job, "--privileged") {
		t.Fatalf("job container run = %q, want unprivileged", job)
	}
}

func TestPrivilegedServicesFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "false": false, "true": true, "1": true, "YES": true} {
		t.Setenv("RUNNER_PRIVILEGED_SERVICES", v)
		if got := privilegedServicesFromEnv(); got != want {
			t.Errorf("RUNNER_PRIVILEGED_SERVICES=%q -> %v, want %v", v, got, want)
		}
		e, _ := New("docker")
		if e.(dockerExecutor).privilegedServices != want {
			t.Errorf("New(docker) with %q: privilegedServices != %v", v, want)
		}
	}
}
