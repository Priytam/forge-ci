package store

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestMaskedValuesCoverShellTrace: a debug rerun runs the script under
// sh -x, and the shells re-quote a value containing ' when they trace it.
// Every trace form must still be masked.
func TestMaskedValuesCoverShellTrace(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	const secret = "it's-a-s3cret"
	if _, err := st.CreateVariable(ctx, proto.VariableRequest{Repo: "acme/cos", Key: "API_TOKEN", Value: secret, Masked: true, EnvironmentScope: "*"}); err != nil {
		t.Fatal(err)
	}
	p := createPipelineT(t, st, "acme/cos", "main", retryDAG)
	jobs, _ := liveJobs(t, st, p.ID)
	masked, err := st.MaskedValuesForJob(ctx, jobs["build"].ID)
	if err != nil {
		t.Fatal(err)
	}
	mask := func(s string) string {
		for _, v := range masked {
			s = strings.ReplaceAll(s, v, "[MASKED]")
		}
		return s
	}
	traces := map[string]string{
		"bash":         `+ curl -H 'Authorization: Bearer it'\''s-a-s3cret' https://api`,
		"busybox ash":  `+ curl -H 'Authorization: Bearer it'"'"'s-a-s3cret' https://api`,
		"dash / plain": `+ curl -H Authorization: Bearer it's-a-s3cret https://api`,
	}
	// Also the real thing, from whatever shells this machine has.
	for _, sh := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(sh); err == nil {
			cmd := exec.Command(path, "-xc", `curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:1/ 2>/dev/null || true`)
			cmd.Env = []string{"API_TOKEN=" + secret, "PATH=/usr/bin:/bin"}
			out, _ := cmd.CombinedOutput()
			traces["local "+sh] = string(out)
		}
	}
	for name, trace := range traces {
		if got := mask(trace); strings.Contains(got, "s3cret") {
			t.Errorf("%s trace leaks the secret after masking: %s", name, got)
		}
	}
}
