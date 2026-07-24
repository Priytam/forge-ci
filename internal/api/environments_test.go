package api

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestDriftOf(t *testing.T) {
	cases := []struct {
		name, deployed, tip, want string
	}{
		{"in sync", "abc123", "abc123", proto.DriftInSync},
		{"drifted", "abc123", "def456", proto.DriftDrifted},
		{"unknown when tip unresolved", "abc123", "", proto.DriftUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := driftOf(c.deployed, c.tip); got != c.want {
				t.Fatalf("driftOf(%q,%q)=%q want %q", c.deployed, c.tip, got, c.want)
			}
		})
	}
}

func TestParseEnvPath(t *testing.T) {
	cases := []struct {
		path, action     string
		wantRepo, wantEnv string
		wantOK            bool
	}{
		{"/api/v1/environments/owner/repo/production/deployments", "deployments", "owner/repo", "production", true},
		{"/api/v1/environments/Priytam/statemachine/production/rollback", "rollback", "Priytam/statemachine", "production", true},
		{"/api/v1/environments/single/production/deployments", "deployments", "single", "production", true},
		{"/api/v1/environments/owner/repo/production/deployments", "rollback", "", "", false},
		{"/api/v1/environments/production/deployments", "deployments", "", "", false}, // repo missing
	}
	for _, c := range cases {
		repo, env, ok := parseEnvPath(c.path, c.action)
		if ok != c.wantOK || repo != c.wantRepo || env != c.wantEnv {
			t.Fatalf("parseEnvPath(%q,%q)=(%q,%q,%v) want (%q,%q,%v)",
				c.path, c.action, repo, env, ok, c.wantRepo, c.wantEnv, c.wantOK)
		}
	}
}
