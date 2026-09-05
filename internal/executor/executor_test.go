package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// argValue returns the value following the first occurrence of flag.
func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i == len(args)-1 {
		return "", false
	}
	return args[i+1], true
}

// The script must run through the shell as the container's ENTRYPOINT. Passing
// it as the command leaves an image's own ENTRYPOINT in place, so an image
// declaring one (trivy, gitleaks) receives `sh -ce <script>` as arguments and
// fails with `unknown command "sh"`.
func TestDockerRunArgsOverridesImageEntrypoint(t *testing.T) {
	job := &proto.RunnerJob{ID: 7, Name: "dependency-scan", Script: "trivy fs ."}
	args := dockerRunArgs(job, "/tmp/ws", "aquasec/trivy", "forge-job-7", "bridge")

	entrypoint, ok := argValue(args, "--entrypoint")
	if !ok {
		t.Fatalf("no --entrypoint in argv: %v", args)
	}
	if entrypoint != "sh" {
		t.Errorf("--entrypoint = %q, want sh", entrypoint)
	}

	// --entrypoint must precede the image, and the script must follow it as
	// `<image> -ce <script>` — docker only treats flags before the image as its
	// own, and anything after the image as the container's arguments.
	iEntry := slices.Index(args, "--entrypoint")
	iImage := slices.Index(args, "aquasec/trivy")
	if iImage < 0 {
		t.Fatalf("image not in argv: %v", args)
	}
	if iEntry > iImage {
		t.Errorf("--entrypoint (%d) must come before the image (%d): %v", iEntry, iImage, args)
	}
	tail := args[iImage:]
	want := []string{"aquasec/trivy", "-ce", "trivy fs ."}
	if !slices.Equal(tail, want) {
		t.Errorf("argv tail = %v, want %v", tail, want)
	}
	// The old form passed "sh" as the container command; it must be gone.
	if slices.Contains(args[iImage:], "sh") {
		t.Errorf("`sh` still passed as a container argument: %v", args[iImage:])
	}
}

// The workspace bind-mount and working directory are what let artifacts land on
// the host for collection after the container exits.
func TestDockerRunArgsMountsWorkspace(t *testing.T) {
	job := &proto.RunnerJob{ID: 1, Script: "true"}
	args := dockerRunArgs(job, "/tmp/ws", "alpine:3", "forge-job-1", "none")

	if v, _ := argValue(args, "-v"); v != "/tmp/ws:/workspace" {
		t.Errorf("-v = %q, want /tmp/ws:/workspace", v)
	}
	if v, _ := argValue(args, "-w"); v != "/workspace" {
		t.Errorf("-w = %q, want /workspace", v)
	}
	if v, _ := argValue(args, "--name"); v != "forge-job-1" {
		t.Errorf("--name = %q", v)
	}
	if !slices.Contains(args, "--rm") {
		t.Errorf("--rm missing: %v", args)
	}
}

func TestDockerRunArgsNetwork(t *testing.T) {
	job := &proto.RunnerJob{ID: 1, Script: "true"}
	for _, network := range []string{"none", "bridge", "forge-net-1"} {
		args := dockerRunArgs(job, "/tmp/ws", "alpine:3", "forge-job-1", network)
		if v, _ := argValue(args, "--network"); v != network {
			t.Errorf("--network = %q, want %q", v, network)
		}
	}
}

// CI_* metadata plus the job's own variables must reach the container, and the
// services path adds FORGE_SERVICE_ALIASES on top.
func TestDockerRunArgsEnv(t *testing.T) {
	job := &proto.RunnerJob{
		ID: 5, PipelineID: 9, Name: "itest", Script: "true",
		Env: map[string]string{"MY_VAR": "value"},
	}
	args := dockerRunArgs(job, "/tmp/ws", "alpine:3", "forge-job-5", "forge-net-5",
		"FORGE_SERVICE_ALIASES=cachedb")

	var env []string
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			env = append(env, args[i+1])
		}
	}
	for _, want := range []string{
		"CI=true",
		"CI_PIPELINE_ID=9",
		"CI_JOB_ID=5",
		"CI_JOB_NAME=itest",
		"MY_VAR=value",
		"FORGE_SERVICE_ALIASES=cachedb",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("missing -e %s (got %v)", want, env)
		}
	}
}

// A multi-line script is passed as ONE argument; splitting it would run only
// the first line.
func TestDockerRunArgsScriptIsSingleArgument(t *testing.T) {
	const script = "echo one\necho two\necho three"
	job := &proto.RunnerJob{ID: 1, Script: script}
	args := dockerRunArgs(job, "/tmp/ws", "alpine:3", "forge-job-1", "none")

	last := args[len(args)-1]
	if last != script {
		t.Errorf("last arg = %q, want the whole script %q", last, script)
	}
	if strings.Count(strings.Join(args, "\x00"), "echo") != 3 {
		t.Errorf("script appears split across arguments: %v", args)
	}
}

// A service-less job with no `network:` must get egress. Running it in an empty
// network namespace is what stopped semgrep resolving semgrep.dev and trivy
// downloading its vulnerability database.
func TestJobNetworkDefaultsToBridge(t *testing.T) {
	if got := jobNetwork(&proto.RunnerJob{ID: 1}); got != "bridge" {
		t.Errorf("jobNetwork = %q, want bridge", got)
	}
}

// `network: none` remains available as an explicit opt-out for jobs that want
// no egress at all.
func TestJobNetworkHonoursExplicitValue(t *testing.T) {
	for _, network := range []string{"none", "host", "ci-egress"} {
		if got := jobNetwork(&proto.RunnerJob{ID: 1, Network: network}); got != network {
			t.Errorf("jobNetwork(%q) = %q, want %q", network, got, network)
		}
	}
}
