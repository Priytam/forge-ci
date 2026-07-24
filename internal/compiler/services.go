package compiler

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// maxServices bounds the number of sidecar service containers per job. Each
// service is a full container (docker) or pod container (kubernetes), so the cap
// keeps a single job from exhausting host/pod resources.
const maxServices = 5

// serviceSpec is one entry of a job's services: list. It accepts either a bare
// scalar (the image) or a mapping:
//
//	services:
//	  - redis:7                       # shorthand: scalar is the image
//	  - image: postgres:16-alpine     # long form ('name:' is also accepted as
//	    alias: db                     #   the image, matching GitLab)
//	    env: {POSTGRES_PASSWORD: pw}
//	    cmd: ["postgres", "-c", "max_connections=50"]
//
// alias defaults to the image's short name (the segment after the last '/' with
// the tag stripped, sanitized to a hostname) when omitted.
type serviceSpec struct {
	Image string
	Alias string
	Env   map[string]string
	Cmd   []string
}

func (s *serviceSpec) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&s.Image)
	case yaml.MappingNode:
		var raw struct {
			Image string            `yaml:"image"`
			Name  string            `yaml:"name"`
			Alias string            `yaml:"alias"`
			Env   map[string]string `yaml:"env"`
			Cmd   []string          `yaml:"cmd"`
		}
		if err := n.Decode(&raw); err != nil {
			return err
		}
		s.Image = raw.Image
		if s.Image == "" {
			s.Image = raw.Name // GitLab long form uses 'name' for the image
		}
		s.Alias, s.Env, s.Cmd = raw.Alias, raw.Env, raw.Cmd
		return nil
	default:
		return fmt.Errorf("service must be a string (image) or a mapping")
	}
}

// defaultAlias derives a hostname from an image reference: drop any registry
// path, strip the tag/digest, and sanitize to a DNS label. e.g.
// "postgres:16-alpine" -> "postgres", "docker.io/library/redis:7" -> "redis".
func defaultAlias(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// validAlias reports whether s is a valid single-label DNS hostname (RFC1123):
// 1-63 chars, lowercase alnum and hyphens, not starting/ending with a hyphen.
func validAlias(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if alnum {
			continue
		}
		if r == '-' && i != 0 && i != len(s)-1 {
			continue
		}
		return false
	}
	return true
}

// validateServices resolves and validates a job's services into proto specs.
// It bounds the count, requires an image per service, derives a default alias
// when absent, and rejects invalid or duplicate aliases. Returns nil for an
// empty list (the job declares no services).
func validateServices(where string, specs []serviceSpec) ([]proto.ServiceSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if len(specs) > maxServices {
		return nil, fmt.Errorf("%s: too many services (%d); the limit is %d", where, len(specs), maxServices)
	}
	seen := map[string]bool{}
	out := make([]proto.ServiceSpec, 0, len(specs))
	for i, sp := range specs {
		image := strings.TrimSpace(sp.Image)
		if image == "" {
			return nil, fmt.Errorf("%s: service #%d requires an image", where, i+1)
		}
		alias := strings.TrimSpace(sp.Alias)
		if alias == "" {
			alias = defaultAlias(image)
		}
		if !validAlias(alias) {
			return nil, fmt.Errorf("%s: service %q has an invalid alias %q "+
				"(must be a lowercase DNS hostname; set services.alias explicitly)", where, image, alias)
		}
		if seen[alias] {
			return nil, fmt.Errorf("%s: duplicate service alias %q", where, alias)
		}
		seen[alias] = true
		out = append(out, proto.ServiceSpec{Image: image, Alias: alias, Env: sp.Env, Cmd: sp.Cmd})
	}
	return out, nil
}
