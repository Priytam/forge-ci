// Package autoscale grows and shrinks pools of runners to match the queue.
//
// A pool is a set of identical runners (same tags) that a Provider can
// count and resize: a Kubernetes Deployment, containers on a Docker host, or
// anything a command can manage (cloud VMs through a CLI or Terraform). The
// loop reads demand from the queue — jobs waiting for the pool's tags plus
// jobs its runners are running — and asks the provider for
// ceil(demand / jobs_per_runner) runners, between min and max. Scaling up is
// immediate; scaling down waits until the pool has been idle for a while and
// relies on runners draining (a stopped runner finishes or requeues its job).
package autoscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Settings is a provider's configuration for one pool, as a JSON object.
type Settings map[string]string

// Pool is what the loop needs to know about one pool.
type Pool struct {
	ID             int64
	Name           string
	Provider       string
	Tags           []string
	Min, Max       int
	JobsPerRunner  int
	ScaleDownAfter time.Duration
	Settings       Settings
	// BusyRunnerIDs are runners in this pool running a job right now; a
	// provider that removes specific runners (docker) avoids them.
	BusyRunnerIDs []string
}

// Provider manages the runners of one kind of pool.
type Provider interface {
	Name() string
	// Validate checks pool settings before they are saved; it returns the
	// keys that hold secrets (masked when read back).
	Validate(s Settings) (secretKeys []string, err error)
	// Current returns how many runners the pool has now.
	Current(ctx context.Context, p Pool) (int, error)
	// Scale sets the pool to desired runners. When shrinking, idle runners
	// (not in p.BusyRunnerIDs) go first where the provider can choose.
	Scale(ctx context.Context, p Pool, desired int) error
}

var providers = map[string]Provider{}

func register(p Provider) { providers[p.Name()] = p }

// Lookup returns the provider for a name.
func Lookup(name string) (Provider, bool) {
	p, ok := providers[name]
	return p, ok
}

// Names lists the registered providers.
func Names() []string {
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Demand is what the queue wants from a pool right now.
type Demand struct {
	Pending int // jobs waiting that the pool's runners could take
	Running int // jobs running on the pool's runners
}

// Decision is what one pass decided for a pool.
type Decision struct {
	Current, Desired int
	Action           string // "scale up", "scale down", "" (hold)
	Reason           string
}

// Decide computes the target size. idleSince is when the pool last had work
// (pending or running); zero means "right now".
func Decide(p Pool, d Demand, current int, idleSince time.Time, now time.Time) Decision {
	per := p.JobsPerRunner
	if per < 1 {
		per = 1
	}
	want := (d.Pending + d.Running + per - 1) / per
	if want < p.Min {
		want = p.Min
	}
	if want > p.Max {
		want = p.Max
	}
	dec := Decision{Current: current, Desired: want}
	switch {
	case want > current:
		dec.Action = "scale up"
		dec.Reason = fmt.Sprintf("%d pending + %d running jobs need %d runners", d.Pending, d.Running, want)
	case want < current:
		if d.Pending > 0 {
			dec.Desired = current
			dec.Reason = "jobs are waiting; holding"
			return dec
		}
		if !idleSince.IsZero() && now.Sub(idleSince) < p.ScaleDownAfter {
			dec.Desired = current
			dec.Reason = fmt.Sprintf("idle for %s, scaling down after %s", now.Sub(idleSince).Round(time.Second), p.ScaleDownAfter)
			return dec
		}
		// One step at a time: the runner that goes drains its job first.
		if want < current-1 {
			dec.Desired = current - 1
		}
		dec.Action = "scale down"
		dec.Reason = fmt.Sprintf("idle; %d runners needed", want)
	default:
		dec.Reason = "at target"
	}
	return dec
}

// ParseSettings decodes a settings JSON object ("" = empty).
func ParseSettings(raw string) (Settings, error) {
	s := Settings{}
	if strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, errors.New("settings must be a JSON object of strings")
	}
	return s, nil
}

func (s Settings) get(key, def string) string {
	if v := strings.TrimSpace(s[key]); v != "" {
		return v
	}
	return def
}

func requireKeys(s Settings, keys ...string) error {
	var missing []string
	for _, k := range keys {
		if strings.TrimSpace(s[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("settings need %s", strings.Join(missing, ", "))
	}
	return nil
}
