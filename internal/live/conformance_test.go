package live_test

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
)

// Every Forge Live provider runs the same conformance suite. A provider
// ships when its harness is listed here and green.
func TestProviderConformance(t *testing.T) {
	harnesses := []livetest.Harness{
		{Provider: "kubernetes", New: func(t *testing.T) livetest.Env { return livetest.NewFakeKube(t) }},
		{Provider: "cloudrun", New: func(t *testing.T) livetest.Env { return livetest.NewFakeCloudRun(t) }},
	}
	for _, h := range harnesses {
		t.Run(h.Provider, func(t *testing.T) { livetest.Run(t, h) })
	}
}
