package api

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestRepoRegistryAuditDetailNeverLeaksSecrets asserts the audit detail records
// only safe metadata: never the static token and never the GitHub App private
// key, while still surfacing the (non-secret) app id / installation id and the
// has-* flags. Also checks the whole detail map (values included) contains no
// key or token material.
func TestRepoRegistryAuditDetailNeverLeaksSecrets(t *testing.T) {
	const key = "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYBYTES\n-----END RSA PRIVATE KEY-----"
	const pat = "ghp_supersecret_pat"

	t.Run("app auth", func(t *testing.T) {
		d := repoRegistryAuditDetail(proto.RepoRegistration{
			Repo:                 "acme/app",
			Provider:             "github",
			CloneURL:             "https://github.com/acme/app.git",
			DefaultBranch:        "main",
			GitHubAppID:          "111",
			GitHubInstallationID: "222",
			GitHubAppPrivateKey:  key,
		})
		if d["github_app"] != true {
			t.Errorf("github_app = %v, want true", d["github_app"])
		}
		if d["github_app_id"] != "111" || d["installation_id"] != "222" {
			t.Errorf("app id/installation not recorded: %v / %v", d["github_app_id"], d["installation_id"])
		}
		if d["has_token"] != false {
			t.Errorf("has_token = %v, want false", d["has_token"])
		}
		assertNoSecret(t, d, key)
	})

	t.Run("pat auth", func(t *testing.T) {
		d := repoRegistryAuditDetail(proto.RepoRegistration{
			Repo:          "acme/pat",
			Provider:      "github",
			CloneURL:      "https://github.com/acme/pat.git",
			DefaultBranch: "main",
			Token:         pat,
		})
		if d["has_token"] != true {
			t.Errorf("has_token = %v, want true", d["has_token"])
		}
		if d["github_app"] != false {
			t.Errorf("github_app = %v, want false", d["github_app"])
		}
		if _, present := d["github_app_id"]; present {
			t.Error("github_app_id present for a PAT-only registration")
		}
		assertNoSecret(t, d, pat)
	})
}

// assertNoSecret verifies no key or value in the detail map contains the secret.
func assertNoSecret(t *testing.T, d map[string]any, secret string) {
	t.Helper()
	for k, v := range d {
		if containsStr(k, secret) {
			t.Errorf("audit detail key %q contains secret", k)
		}
		if s, ok := v.(string); ok && containsStr(s, secret) {
			t.Errorf("audit detail[%q] value contains secret: %q", k, s)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
