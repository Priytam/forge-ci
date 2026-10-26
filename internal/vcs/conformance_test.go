package vcs_test

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// Every code-review plugin runs the same conformance suite. A provider joins
// this list when it implements vcs.ChangeSource.
func TestChangeSourceConformance(t *testing.T) {
	harnesses := []vcstest.Harness{
		{Provider: "github", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitHub(t) }},
		{Provider: "git", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGit(t) }},
	}
	covered := map[string]bool{}
	for _, h := range harnesses {
		covered[h.Provider] = true
		t.Run(h.Provider, func(t *testing.T) { vcstest.RunChangeSource(t, h) })
	}
	// A plugin that implements ChangeSource without a harness here would ship
	// untested: fail instead.
	for _, name := range vcs.Names() {
		if vcs.ChangeSourceFor(name) != nil && !covered[name] {
			t.Errorf("provider %q implements ChangeSource but has no conformance harness", name)
		}
	}
}
