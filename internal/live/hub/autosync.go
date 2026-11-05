package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Auto-sync (phase C). For an application whose policy says so, Forge syncs
// each new revision by itself and, with self-heal, undoes drift in the
// cluster. It runs here, on the server, through the same operations a person
// starts — the same freeze and approval gates, the same per-app lock, the
// same history and audit trail — so an automatic sync is never a different
// kind of change, only a different requester.
//
// Every replica runs the loop; the per-app lock (live_ops_one_change_idx)
// makes sure only one of them starts a given sync.

const (
	autoTick = 5 * time.Second // how often out-of-sync apps are looked at
	// selfHealDelay is the pause after a sync before healing again (and the
	// first retry's backoff); it doubles per failure, and per heal beyond
	// the third within healWindow — something that keeps changing an app
	// is not fought every five seconds.
	selfHealDelay   = 5 * time.Second
	healWindow      = 10 * time.Minute
	maxAutoAttempts = 5
	autoBatch       = 1000 // apps looked at per pass
	// AutoSyncActor is who auto-syncs are requested by.
	AutoSyncActor = "Forge auto-sync"
)

// AutoSync keeps applications with auto-sync on in sync until ctx ends.
func (h *Hub) AutoSync(ctx context.Context) {
	tick := time.NewTicker(autoTick)
	defer tick.Stop()
	for {
		h.autoPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-h.autoWake:
		}
	}
}

func (h *Hub) autoPass(ctx context.Context) {
	apps, err := h.st.AutoSyncCandidates(ctx, autoBatch)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("live: auto-sync", "err", err)
		}
		return
	}
	for i := range apps {
		if err := h.autoSyncApp(ctx, &apps[i]); err != nil && ctx.Err() == nil {
			slog.Warn("live: auto-sync", "app", apps[i].ID, "err", err)
		}
	}
}

func (h *Hub) autoSyncApp(ctx context.Context, a *store.LiveApp) error {
	in := autoInput{Policy: *a.Policy, Want: a.Revision, Rev: a.Revision, Drifted: a.Drifted, Prunable: a.Prunable, Env: a.Environment, Now: time.Now()}
	op := live.Op{Kind: live.OpSync, Revision: a.Revision, Prune: a.Policy.Prune, Force: true}
	if a.DesiredDigest != "" {
		// What the app renders to, not the commit: a commit to another app
		// in the same repo changes the revision but not this.
		in.Want, op.Digest = a.DesiredDigest, a.DesiredDigest
	}
	if a.Revision == "" {
		// No Git (Cloud Run): the desired state is what Forge last deployed.
		if op.Image = a.DeployedImage(); op.Image == "" {
			return h.st.SetLivePolicyStatus(ctx, a.ID, "Waiting for a Forge deploy that reports an image digest")
		}
		in.Want, in.Rev = a.Deployed.Digest, a.Deployed.Digest
	}
	ops, err := h.st.LiveAppOps(ctx, a.ID, 30)
	if err != nil {
		return err
	}
	in.Ops = ops
	if in.Frozen, in.Approvals, err = h.st.LiveChangeGate(ctx, a.Repo, a.Environment); err != nil {
		return err
	}
	d := decideAuto(in)
	if d.Trigger != "" {
		op.Trigger = d.Trigger
		o, err := h.st.CreateLiveOp(ctx, a.ID, op, AutoSyncActor)
		switch {
		case errors.Is(err, store.ErrOpBusy):
			return nil // another replica, or a person, got there first
		case errors.Is(err, store.ErrFrozen):
			d.Status = frozenStatus(a.Environment)
		case err != nil:
			return err
		default:
			if err := h.st.InsertAudit(ctx, store.AuditEntry{Actor: AutoSyncActor, Action: "live.sync",
				Target: a.Target + "/" + a.Key, Repo: a.Repo, Result: "requested", Detail: map[string]any{
					"op": o.ID, "trigger": d.Trigger, "revision": in.Want, "prune": op.Prune, "state": o.State}}); err != nil {
				slog.Error("audit write failed", "err", err, "action", "live.sync")
			}
		}
	}
	return h.st.SetLivePolicyStatus(ctx, a.ID, d.Status)
}

// autoInput is everything auto-sync decides on.
type autoInput struct {
	Policy            store.LivePolicy
	Want              string // what the app should run: its desired digest, else revision (or image digest)
	Rev               string // the revision, for people ("" = Want)
	Drifted, Prunable int
	Ops               []store.LiveOp // the app's operations, newest first
	Frozen            bool
	Approvals         int // the environment's required approvals
	Env               string
	Now               time.Time
}

// autoDecision: sync now (Trigger set) or not, and what to tell people.
type autoDecision struct {
	Trigger string
	Status  string
}

// decideAuto is auto-sync's whole policy, for one out-of-sync app:
//
//   - a revision not synced yet is synced (auto-sync);
//   - a failed sync of it is retried with backoff, up to maxAutoAttempts;
//   - once it synced, drift is undone only with self-heal, after
//     selfHealDelay — slower the more often it has had to heal lately —
//     and never where every change needs an approval;
//   - nothing happens while the environment is frozen, while a sync or
//     rollback is in flight, or when all that differs is what auto-sync
//     would not prune anyway.
func decideAuto(in autoInput) autoDecision {
	var changes []store.LiveOp
	for _, o := range in.Ops {
		if o.Kind == live.OpSync || o.Kind == live.OpRollback {
			changes = append(changes, o)
		}
	}
	if len(changes) > 0 && inFlight(changes[0].State) {
		return autoDecision{}
	}
	if in.Drifted == 0 && in.Prunable > 0 && !in.Policy.Prune {
		return autoDecision{Status: fmt.Sprintf("%d resource%s Git no longer has; auto-sync does not prune", in.Prunable, plural(in.Prunable))}
	}
	if in.Frozen {
		return autoDecision{Status: frozenStatus(in.Env)}
	}
	want := shortRev(in.Want)
	if in.Rev != "" {
		want = shortRev(in.Rev)
	}
	if len(changes) == 0 || changes[0].Kind == live.OpRollback || opTarget(changes[0]) != in.Want {
		return autoDecision{Trigger: live.TriggerAutoSync, Status: "Syncing to " + want}
	}
	last := changes[0]
	since := in.Now.Sub(finishedAt(last))
	switch {
	case last.Result != nil && last.Result.Aborted:
		return autoDecision{Status: fmt.Sprintf("%s Auto-sync waits for the next revision after %s.", firstLine(last.Message), want)}
	}
	switch last.State {
	case "canceled":
		return autoDecision{Status: fmt.Sprintf("The sync to %s was canceled; auto-sync waits for the next revision", want)}
	case "failed":
		failures := 0
		for _, o := range changes {
			if o.State != "failed" || o.Kind != live.OpSync || opTarget(o) != in.Want {
				break
			}
			failures++
		}
		if failures >= maxAutoAttempts {
			return autoDecision{Status: fmt.Sprintf("Stopped after %d failed syncs to %s: %s. Push a fix or sync by hand.",
				failures, want, firstLine(last.Message))}
		}
		if wait := selfHealDelay << (failures - 1); since < wait {
			return autoDecision{Status: fmt.Sprintf("Sync %d of %d to %s failed: %s. Retrying in %s.",
				failures, maxAutoAttempts, want, firstLine(last.Message), (wait - since).Round(time.Second))}
		}
		trigger := last.Params.Trigger
		if trigger == "" {
			trigger = live.TriggerAutoSync // a person's sync failed: auto-sync takes it from here
		}
		return autoDecision{Trigger: trigger, Status: fmt.Sprintf("Retrying the sync to %s (%d of %d)", want, failures+1, maxAutoAttempts)}
	}
	// The last change synced want, and live has drifted since.
	if !in.Policy.SelfHeal {
		return autoDecision{Status: fmt.Sprintf("Live differs from %s since it was synced; self-heal is off", want)}
	}
	if in.Approvals > 0 {
		return autoDecision{Status: fmt.Sprintf("Self-heal is paused: every change to %s needs an approval", in.Env)}
	}
	heals := 0
	for _, o := range changes {
		if o.Params.Trigger == live.TriggerSelfHeal && in.Now.Sub(finishedAt(o)) < healWindow {
			heals++
		}
	}
	wait, busy := selfHealDelay, ""
	if heals >= 3 {
		wait = selfHealDelay << min(heals-2, 6)
		busy = fmt.Sprintf(" Healed %d times in %s: something keeps changing it.", heals, healWindow)
	}
	if since < wait {
		return autoDecision{Status: fmt.Sprintf("Healing in %s.%s", (wait - since).Round(time.Second), busy)}
	}
	return autoDecision{Trigger: live.TriggerSelfHeal, Status: "Healing drift from " + want + "." + busy}
}

func inFlight(state string) bool {
	return state == "approval" || state == "pending" || state == "running"
}

// opTarget is what a sync or rollback put in place: the rendered state it
// applied (or was asked for), else its source revision, or for a
// deploy-record provider the image digest.
func opTarget(o store.LiveOp) string {
	if _, digest, ok := strings.Cut(o.Params.Image, "@"); ok {
		return digest
	}
	if o.Result != nil && o.Result.Digest != "" {
		return o.Result.Digest
	}
	if o.Params.Digest != "" {
		return o.Params.Digest
	}
	if o.Revision != "" {
		return o.Revision
	}
	return o.Params.Revision
}

func finishedAt(o store.LiveOp) time.Time {
	if o.FinishedAt != nil {
		return *o.FinishedAt
	}
	return o.CreatedAt
}

func frozenStatus(env string) string {
	return fmt.Sprintf("Paused: deploys to %s are frozen", env)
}

func shortRev(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
