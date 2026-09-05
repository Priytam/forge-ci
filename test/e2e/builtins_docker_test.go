//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// End-to-end execution of every shipped security/* built-in on a docker runner.
//
// This is the test the two P1 docker-executor bugs slipped past. Each built-in
// was individually unit-testable as YAML, and the include: machinery had
// coverage, but nothing ever RAN one in a container — so two defects that make
// all four unrunnable went unnoticed:
//
//   - the image's own ENTRYPOINT was not overridden, so trivy and gitleaks
//     received `sh -ce <script>` as arguments (`unknown command "sh"`);
//   - service-less jobs ran with --network none, so semgrep could not resolve
//     semgrep.dev and trivy could not download its vulnerability database.
//
// The assertions are therefore specifically about those two failure modes plus
// evidence that the tool's own binary actually started. Findings are NOT
// asserted: the e2e workspace is empty (the repo is unregistered, so nothing is
// cloned), and what matters is that the scanner runs at all.

// scanImagePullTimeout is generous: these images are large and the first run on
// a fresh host pulls them.
const scanImagePullTimeout = 300 * time.Second

// entrypointBugSignatures are the shapes an un-overridden image ENTRYPOINT
// produces once the shell invocation is passed to the image's own binary.
var entrypointBugSignatures = []string{
	`unknown command "sh"`,
	"unknown command \"sh\"",
	`unknown flag: --ce`,
	"unknown shorthand flag: 'c'",
}

// networkBugSignatures are the shapes an empty network namespace produces —
// resolver failures from the various runtimes the scanners are built on.
var networkBugSignatures = []string{
	"Temporary failure in name resolution",
	"Try again",
	"NameResolutionError",
	"Could not resolve host",
	"no such host",
	"dial tcp: lookup",
}

// dockerEgress reports whether a container on the default network can reach the
// internet. The security scanners all fetch something (rules, vuln DB), so on a
// host without egress this suite has nothing to prove and self-skips rather
// than reporting a failure that is really an environment limitation.
func dockerEgress(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// getent is in busybox; a successful lookup means DNS and routing work.
	return exec.CommandContext(c, "docker", "run", "--rm", "--network", "bridge",
		"alpine:3", "getent", "hosts", "deb.debian.org").Run() == nil
}

// TestBuiltinSecurityTemplatesOnDocker runs each security/* built-in through
// include: on a docker-executor runner and asserts it actually executed.
func TestBuiltinSecurityTemplatesOnDocker(t *testing.T) {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		t.Skip("SKIP: docker not available (`docker info` failed) — the built-in scans need the docker executor")
	}
	if !dockerEgress(ctx) {
		t.Skip("SKIP: containers have no network egress — every security/* scan fetches rules or a vuln DB")
	}

	dr, err := stack.startDockerRunner(ctx, "e2e-docker-builtins", []string{"docker"}, nil)
	if err != nil {
		t.Fatalf("start docker runner: %v", err)
	}
	defer dr.kill()

	cases := []struct {
		template string // built-in template name
		job      string // job name the template declares
		// ran is a marker proving the tool's own binary produced output, rather
		// than the container dying before it started.
		ran []string
	}{
		{
			template: "security/sast",
			job:      "sast",
			ran:      []string{"semgrep", "Scan", "rules", "findings"},
		},
		{
			template: "security/dependency",
			job:      "dependency-scan",
			ran:      []string{"trivy", "vulnerability", "Need to update DB", "scanning"},
		},
		{
			template: "security/container",
			job:      "container-scan",
			ran:      []string{"trivy", "vulnerability", "Need to update DB", "alpine"},
		},
		{
			template: "security/secrets",
			job:      "secret-detection",
			ran:      []string{"gitleaks", "leaks", "scan", "○"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			repo := uniqueRepo("builtin")
			// Tag the built-in's job onto the docker runner. The shell runner has
			// no tags and will not pick up a tagged job.
			config := fmt.Sprintf(`
include:
  - template: %s
stages: [test]
jobs:
  %s:
    tags: [docker]
`, tc.template, tc.job)

			p := createPipeline(t, base(), repo, "main", randSHA(), config)
			j := waitJob(t, base(), p.ID, tc.job, scanImagePullTimeout,
				"success", "failed", "canceled")
			log := getJobLogsFull(t, base(), j.ID)

			if j.Status == "canceled" {
				t.Fatalf("%s was canceled before completing; log:\n%s", tc.template, log)
			}

			// BUG 1 regression: an un-overridden image ENTRYPOINT.
			for _, sig := range entrypointBugSignatures {
				if strings.Contains(log, sig) {
					t.Fatalf("%s: image ENTRYPOINT was not overridden (found %q); log:\n%s",
						tc.template, sig, log)
				}
			}
			// BUG 2 regression: no network namespace.
			for _, sig := range networkBugSignatures {
				if strings.Contains(log, sig) {
					t.Fatalf("%s: job had no network egress (found %q); log:\n%s",
						tc.template, sig, log)
				}
			}
			// Positive evidence that the scanner itself ran.
			lower := strings.ToLower(log)
			var sawMarker bool
			for _, m := range tc.ran {
				if strings.Contains(lower, strings.ToLower(m)) {
					sawMarker = true
					break
				}
			}
			if !sawMarker {
				t.Fatalf("%s: no sign the tool ran (expected one of %v); log:\n%s",
					tc.template, tc.ran, log)
			}
		})
	}
}

// A job that asks for no egress must still get none — `network: none` is the
// documented opt-out and the isolation it provides has to be real.
func TestJobNetworkNoneIsolatesOnDocker(t *testing.T) {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		t.Skip("SKIP: docker not available (`docker info` failed)")
	}
	dr, err := stack.startDockerRunner(ctx, "e2e-docker-netnone", []string{"docker"}, nil)
	if err != nil {
		t.Fatalf("start docker runner: %v", err)
	}
	defer dr.kill()

	repo := uniqueRepo("netnone")
	// The runner echoes each script line before running it, so the verdict is
	// reported as an exit code interpolated at RUNTIME — a literal marker would
	// also match the echoed source line and prove nothing.
	config := `
stages: [test]
jobs:
  isolated:
    stage: test
    image: alpine:3
    network: none
    tags: [docker]
    script:
      - 'getent hosts deb.debian.org >/dev/null 2>&1; echo "egress-exit=$?"'
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "isolated", dockerRun, "success", "failed", "canceled")
	log := getJobLogsFull(t, base(), j.ID)
	if !strings.Contains(log, "egress-exit=") {
		t.Fatalf("isolated job produced no verdict; log:\n%s", log)
	}
	if strings.Contains(log, "egress-exit=0") {
		t.Fatalf("network: none job resolved a public name — isolation is not real; log:\n%s", log)
	}
}

// The default (no `network:`) must give a job egress, which is the behaviour
// change that makes the built-in scans runnable.
func TestJobNetworkDefaultHasEgressOnDocker(t *testing.T) {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		t.Skip("SKIP: docker not available (`docker info` failed)")
	}
	if !dockerEgress(ctx) {
		t.Skip("SKIP: containers have no network egress on this host")
	}
	dr, err := stack.startDockerRunner(ctx, "e2e-docker-netdefault", []string{"docker"}, nil)
	if err != nil {
		t.Fatalf("start docker runner: %v", err)
	}
	defer dr.kill()

	repo := uniqueRepo("netdefault")
	config := `
stages: [test]
jobs:
  networked:
    stage: test
    image: alpine:3
    tags: [docker]
    script:
      - 'getent hosts deb.debian.org >/dev/null 2>&1; echo "egress-exit=$?"'
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "networked", dockerRun, "success", "failed", "canceled")
	log := getJobLogsFull(t, base(), j.ID)
	if j.Status != "success" {
		t.Fatalf("default-network job status=%q want success; log:\n%s", j.Status, log)
	}
	if !strings.Contains(log, "egress-exit=0") {
		t.Fatalf("default-network job could not resolve a public name; log:\n%s", log)
	}
}

// An image with its own ENTRYPOINT must run the job's script, not hand it to the
// image's binary. This is BUG 1 reduced to its smallest reproducible form, so a
// regression is diagnosable without pulling a scanner image.
func TestImageWithEntrypointRunsScriptOnDocker(t *testing.T) {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		t.Skip("SKIP: docker not available (`docker info` failed)")
	}
	dr, err := stack.startDockerRunner(ctx, "e2e-docker-entrypoint", []string{"docker"}, nil)
	if err != nil {
		t.Fatalf("start docker runner: %v", err)
	}
	defer dr.kill()

	// alpine/git declares ENTRYPOINT ["git"] and is small, so this reproduces the
	// bug without pulling a scanner image: before the fix the container ran
	// `git sh -ce "echo SCRIPT_RAN"` and died.
	repo := uniqueRepo("entrypoint")
	config := `
stages: [test]
jobs:
  entrypointed:
    stage: test
    image: alpine/git
    tags: [docker]
    script:
      - echo SCRIPT_RAN
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "entrypointed", scanImagePullTimeout,
		"success", "failed", "canceled")
	log := getJobLogsFull(t, base(), j.ID)
	if j.Status != "success" {
		t.Fatalf("job on an image with its own ENTRYPOINT status=%q want success; log:\n%s",
			j.Status, log)
	}
	if !strings.Contains(log, "SCRIPT_RAN") {
		t.Fatalf("script did not run under the image's ENTRYPOINT; log:\n%s", log)
	}
}
