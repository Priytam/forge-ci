package compiler

import (
	"strings"
	"testing"
)

func TestValidateNetwork(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty means the executor default", in: "", want: ""},
		{name: "none", in: "none", want: "none"},
		{name: "bridge", in: "bridge", want: "bridge"},
		{name: "host", in: "host", want: "host"},
		{name: "named network", in: "ci-egress", want: "ci-egress"},
		{name: "named with dots and underscores", in: "ci_net.v2", want: "ci_net.v2"},
		{name: "whitespace trimmed", in: "  none  ", want: "none"},
		// A value starting with '-' would be read as a flag once it reaches the
		// `docker run` argv, so it is rejected outright.
		{name: "leading dash rejected", in: "--privileged", wantErr: "may not start with '-'"},
		{name: "shell metacharacters rejected", in: "net;rm -rf /", wantErr: "invalid network"},
		{name: "spaces rejected", in: "two words", wantErr: "invalid network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateNetwork(`job "x"`, tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("validateNetwork(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// An unset network compiles to "", which the executor resolves to its default
// (bridge). The compiler deliberately does not bake the default in, so the
// executor stays the single place that decides it.
func TestCompileNetworkDefaultsToUnset(t *testing.T) {
	jobs, err := Compile(`
stages: [test]
jobs:
  scan:
    stage: test
    image: aquasec/trivy
    script: [trivy fs .]
`, "main", SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Network != "" {
		t.Errorf("Network = %q, want empty (executor default)", jobs[0].Network)
	}
}

func TestCompileNetworkExplicit(t *testing.T) {
	jobs, err := Compile(`
stages: [test]
jobs:
  isolated:
    stage: test
    network: none
    script: [echo hi]
`, "main", SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Network != "none" {
		t.Errorf("Network = %q, want none", jobs[0].Network)
	}
}

// A job with services is given its own per-job network so it can resolve the
// service aliases, so declaring both is a contradiction rather than something to
// silently resolve one way.
func TestCompileNetworkWithServicesRejected(t *testing.T) {
	_, err := Compile(`
stages: [test]
jobs:
  itest:
    stage: test
    network: host
    services: [redis:7-alpine]
    script: [echo hi]
`, "main", SourcePush, nil)
	if err == nil {
		t.Fatal("expected an error for network: together with services:")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v", err)
	}
}

func TestCompileNetworkInvalidRejected(t *testing.T) {
	_, err := Compile(`
stages: [test]
jobs:
  bad:
    stage: test
    network: "--privileged"
    script: [echo hi]
`, "main", SourcePush, nil)
	if err == nil {
		t.Fatal("expected an error for an invalid network")
	}
}

// network: must travel through extends: and include: like the other scalars, so
// a repo can set it once on a hidden base job.
func TestNetworkInheritedThroughExtends(t *testing.T) {
	jobs, err := Compile(`
stages: [test]
jobs:
  .scan-base:
    network: none
    image: alpine:3
  scan:
    stage: test
    extends: .scan-base
    script: [echo hi]
`, "main", SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Network != "none" {
		t.Errorf("Network = %q, want none inherited from the base", jobs[0].Network)
	}
}

// The main config overrides an included fragment's network, the same way it
// overrides every other job key.
func TestNetworkMainOverridesInclude(t *testing.T) {
	tmpl := func(name string) (string, bool, error) {
		if name != "scan-tmpl" {
			return "", false, nil
		}
		return `
stages: [test]
jobs:
  scan:
    stage: test
    network: none
    image: alpine:3
    script: [echo from-template]
`, true, nil
	}
	jobs, err := Compile(`
include: [{template: scan-tmpl}]
stages: [test]
jobs:
  scan:
    network: bridge
`, "main", SourcePush, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Network != "bridge" {
		t.Errorf("Network = %q, want bridge from the main config", jobs[0].Network)
	}
}
