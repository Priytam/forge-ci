package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// op is a finished sync to rev, ago before t0.
func op(state, rev, trigger string, ago time.Duration) store.LiveOp {
	at := t0.Add(-ago)
	return store.LiveOp{Kind: live.OpSync, State: state, Revision: rev, FinishedAt: &at, CreatedAt: at,
		Params: live.Op{Trigger: trigger}, Message: "Deployment cart: conflict"}
}

func TestDecideAuto(t *testing.T) {
	on := store.LivePolicy{AutoSync: true}
	heal := store.LivePolicy{AutoSync: true, SelfHeal: true}
	for _, tc := range []struct {
		name    string
		in      autoInput
		trigger string
		status  string
	}{
		{"a new revision syncs", autoInput{Policy: on, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{op("succeeded", "aaaaaaa1", "", time.Hour)}}, live.TriggerAutoSync, "Syncing to bbbbbbb"},
		{"the first revision syncs", autoInput{Policy: on, Want: "bbbbbbb1", Drifted: 1}, live.TriggerAutoSync, "Syncing"},
		{"after a rollback, back to Git", autoInput{Policy: on, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{{Kind: live.OpRollback, State: "succeeded", Revision: "bbbbbbb1"}}}, live.TriggerAutoSync, "Syncing"},
		{"nothing while a sync runs", autoInput{Policy: heal, Want: "b", Drifted: 1,
			Ops: []store.LiveOp{{Kind: live.OpSync, State: "running"}}}, "", ""},
		{"a refresh does not count", autoInput{Policy: on, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{{Kind: live.OpRefresh, State: "succeeded"}, op("succeeded", "bbbbbbb1", "", time.Hour)}}, "", "self-heal is off"},
		{"frozen", autoInput{Policy: on, Want: "b", Drifted: 1, Frozen: true, Env: "production"}, "", "Paused: deploys to production are frozen"},
		{"only prunable, prune off", autoInput{Policy: on, Want: "b", Prunable: 2}, "", "2 resources Git no longer has"},
		{"drift after a sync, no self-heal", autoInput{Policy: on, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{op("succeeded", "bbbbbbb1", "", time.Minute)}}, "", "Live differs from bbbbbbb since it was synced; self-heal is off"},
		{"self-heal", autoInput{Policy: heal, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{op("succeeded", "bbbbbbb1", "", time.Minute)}}, live.TriggerSelfHeal, "Healing drift from bbbbbbb"},
		{"self-heal waits after a sync", autoInput{Policy: heal, Want: "bbbbbbb1", Drifted: 1,
			Ops: []store.LiveOp{op("succeeded", "bbbbbbb1", "", 2*time.Second)}}, "", "Healing in 3s"},
		{"self-heal slows down when fought", autoInput{Policy: heal, Want: "b", Drifted: 1, Ops: []store.LiveOp{
			op("succeeded", "b", live.TriggerSelfHeal, 10*time.Second), op("succeeded", "b", live.TriggerSelfHeal, time.Minute),
			op("succeeded", "b", live.TriggerSelfHeal, 2*time.Minute), op("succeeded", "b", live.TriggerSelfHeal, 3*time.Minute)}},
			"", "Healing in 10s. Healed 4 times in 10m0s: something keeps changing it"},
		{"no self-heal where changes need approval", autoInput{Policy: heal, Want: "b", Drifted: 1, Approvals: 1, Env: "production",
			Ops: []store.LiveOp{op("succeeded", "b", "", time.Hour)}}, "", "every change to production needs an approval"},
		{"retry after backoff", autoInput{Policy: on, Want: "b", Drifted: 1, Ops: []store.LiveOp{
			op("failed", "b", live.TriggerAutoSync, 11*time.Second), op("failed", "b", live.TriggerAutoSync, time.Minute)}},
			live.TriggerAutoSync, "Retrying the sync to b (3 of 5)"},
		{"backoff doubles", autoInput{Policy: on, Want: "b", Drifted: 1, Ops: []store.LiveOp{
			op("failed", "b", live.TriggerAutoSync, 6*time.Second), op("failed", "b", live.TriggerAutoSync, time.Minute)}},
			"", "Sync 2 of 5 to b failed: Deployment cart: conflict. Retrying in 4s."},
		{"a person's failed sync is retried", autoInput{Policy: on, Want: "b", Drifted: 1, Ops: []store.LiveOp{
			op("failed", "b", "", time.Minute)}}, live.TriggerAutoSync, "Retrying"},
		{"gives up", autoInput{Policy: heal, Want: "bbbbbbb1", Drifted: 1, Ops: []store.LiveOp{
			op("failed", "bbbbbbb1", "", time.Hour), op("failed", "bbbbbbb1", "", time.Hour), op("failed", "bbbbbbb1", "", time.Hour),
			op("failed", "bbbbbbb1", "", time.Hour), op("failed", "bbbbbbb1", "", time.Hour)}},
			"", "Stopped after 5 failed syncs to bbbbbbb"},
		{"a rejected sync waits for the next revision", autoInput{Policy: on, Want: "b", Drifted: 1, Ops: []store.LiveOp{
			op("canceled", "b", "", time.Hour)}}, "", "canceled; auto-sync waits for the next revision"},
		{"cloud run: the deploy record's digest", autoInput{Policy: heal, Want: "sha256:feedface", Ops: []store.LiveOp{
			{Kind: live.OpSync, State: "succeeded", Revision: "svc-00042", Params: live.Op{Image: "reg/app@sha256:feedface"}}}},
			live.TriggerSelfHeal, "Healing drift from feedfac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now = t0
			d := decideAuto(tc.in)
			if d.Trigger != tc.trigger || !strings.Contains(d.Status, tc.status) {
				t.Errorf("decideAuto = %+v, want trigger %q, status containing %q", d, tc.trigger, tc.status)
			}
		})
	}
}

func TestDecideAutoAfterAbortedRollout(t *testing.T) {
	o := op("failed", "b", live.TriggerAutoSync, time.Hour)
	o.Result = &live.OpResult{Aborted: true}
	o.Message = "Rollout of web aborted: analysis failed at 25%. web still runs the stable version."
	d := decideAuto(autoInput{Policy: store.LivePolicy{AutoSync: true, SelfHeal: true}, Want: "b", Drifted: 1, Now: t0,
		Ops: []store.LiveOp{o}})
	if d.Trigger != "" || d.Status != "Rollout of web aborted: analysis failed at 25%. web still runs the stable version. Auto-sync waits for the next revision after b." {
		t.Errorf("decideAuto = %+v", d)
	}
}
