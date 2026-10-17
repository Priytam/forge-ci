package compiler

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

var secretVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// compileSecrets validates a job's secrets: block.
func compileSecrets(job string, in map[string]secretSpec) (map[string]proto.SecretSpec, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]proto.SecretSpec{}
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s := in[name]
		if !secretVarRe.MatchString(name) {
			return nil, fmt.Errorf("job %q: secret %q must be a valid variable name", job, name)
		}
		set := 0
		for _, v := range []string{s.Vault, s.AWS, s.GCP, s.Azure} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return nil, fmt.Errorf("job %q: secret %s needs exactly one of vault:, aws:, gcp: or azure:", job, name)
		}
		out[name] = proto.SecretSpec{Vault: s.Vault, AWS: s.AWS, GCP: s.GCP, Azure: s.Azure, File: s.File}
	}
	return out, nil
}
