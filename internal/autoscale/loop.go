package autoscale

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Interval between passes (AUTOSCALE_INTERVAL, default 30s).
func interval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("AUTOSCALE_INTERVAL")); err == nil && d >= 5*time.Second {
		return d
	}
	return 30 * time.Second
}

// Loop runs the autoscaler until ctx ends. Several server replicas may run
// it: each pass takes a database advisory lock, so only one acts at a time.
func Loop(ctx context.Context, st *store.Store) {
	t := time.NewTicker(interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := Pass(ctx, st, time.Now()); err != nil {
				slog.Error("autoscale: pass failed", "err", err)
			}
		}
	}
}

// Pass evaluates every enabled pool once.
func Pass(ctx context.Context, st *store.Store, now time.Time) error {
	release, ok, err := st.TryAdvisoryLock(ctx, "forge-autoscale")
	if err != nil || !ok {
		return err
	}
	defer release()
	pools, err := st.ListRunnerPools(ctx, true)
	if err != nil {
		return err
	}
	for _, p := range pools {
		if !p.Enabled {
			continue
		}
		dec, err := evaluate(ctx, st, p, now)
		if err != nil {
			slog.Warn("autoscale: pool", "pool", p.Name, "err", err)
		}
		if dec.Action != "" && err == nil {
			slog.Info("autoscale: "+dec.Action, "pool", p.Name, "from", dec.Current, "to", dec.Desired, "reason", dec.Reason)
		}
	}
	return nil
}

// evaluate decides and acts for one pool, recording the outcome.
func evaluate(ctx context.Context, st *store.Store, sp store.RunnerPool, now time.Time) (Decision, error) {
	prov, ok := Lookup(sp.Provider)
	if !ok {
		err := fmt.Errorf("unknown provider %q", sp.Provider)
		_ = st.RecordPoolStatus(ctx, sp.ID, sp.Current, sp.Desired, sp.PendingJobs, "", err.Error(), false, false)
		return Decision{}, err
	}
	settings, err := ParseSettings(sp.Settings)
	if err != nil {
		_ = st.RecordPoolStatus(ctx, sp.ID, sp.Current, sp.Desired, sp.PendingJobs, "", err.Error(), false, false)
		return Decision{}, err
	}
	demand, err := st.PoolDemand(ctx, sp.Tags)
	if err != nil {
		return Decision{}, err
	}
	p := Pool{
		ID: sp.ID, Name: sp.Name, Provider: sp.Provider, Tags: sp.Tags, Min: sp.Min, Max: sp.Max,
		JobsPerRunner: sp.JobsPerRunner, ScaleDownAfter: time.Duration(sp.ScaleDownAfter) * time.Second,
		Settings: settings, BusyRunnerIDs: demand.BusyRunnerIDs,
	}
	current, err := prov.Current(ctx, p)
	if err != nil {
		_ = st.RecordPoolStatus(ctx, sp.ID, sp.Current, sp.Desired, demand.Pending, "", "count: "+err.Error(), false, false)
		return Decision{}, err
	}
	busy := demand.Pending > 0 || demand.Running > 0
	idleSince := time.Time{}
	if !busy {
		if sp.LastBusyAt != nil {
			idleSince = *sp.LastBusyAt
		} else if sp.CheckedAt != nil {
			idleSince = *sp.CheckedAt
		} else {
			idleSince = now
		}
	}
	dec := Decide(p, Demand{Pending: demand.Pending, Running: demand.Running}, current, idleSince, now)
	if dec.Action == "" {
		return dec, st.RecordPoolStatus(ctx, sp.ID, current, dec.Desired, demand.Pending, "", "", false, busy)
	}
	if err := prov.Scale(ctx, p, dec.Desired); err != nil {
		_ = st.RecordPoolStatus(ctx, sp.ID, current, dec.Desired, demand.Pending, "", dec.Action+": "+err.Error(), false, busy)
		return dec, err
	}
	action := fmt.Sprintf("%s %d → %d (%s)", dec.Action, current, dec.Desired, strings.TrimSpace(dec.Reason))
	return dec, st.RecordPoolStatus(ctx, sp.ID, dec.Desired, dec.Desired, demand.Pending, action, "", true, busy)
}
