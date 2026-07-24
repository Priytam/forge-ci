package store

import (
	"context"
	"testing"
	"time"
)

// TestScheduleCRUD covers create/list/get/update/delete round-trips.
func TestScheduleCRUD(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	next := time.Now().UTC().Add(time.Hour)

	sc, err := st.CreateSchedule(ctx, "acme/app", "main", "0 * * * *", true, "alice@example.com", next)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if sc.ID == 0 || sc.Repo != "acme/app" || sc.Ref != "main" || !sc.Enabled {
		t.Fatalf("unexpected schedule: %+v", sc)
	}
	if sc.LastRunAt != nil {
		t.Fatalf("last_run_at should be nil on a fresh schedule, got %v", sc.LastRunAt)
	}

	list, err := st.ListSchedules(ctx, "acme/app")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSchedules: n=%d err=%v", len(list), err)
	}

	// Other repos don't see it; empty repo sees all.
	if other, _ := st.ListSchedules(ctx, "other/repo"); len(other) != 0 {
		t.Fatalf("expected no schedules for other repo, got %d", len(other))
	}
	if all, _ := st.ListSchedules(ctx, ""); len(all) != 1 {
		t.Fatalf("expected 1 schedule for all-repos list, got %d", len(all))
	}

	newCron := "*/30 * * * *"
	disabled := false
	newNext := time.Now().UTC().Add(30 * time.Minute)
	up, err := st.UpdateSchedule(ctx, sc.ID, &newCron, nil, &disabled, newNext)
	if err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if up.Cron != newCron || up.Enabled {
		t.Fatalf("update not applied: %+v", up)
	}

	if err := st.DeleteSchedule(ctx, sc.ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if err := st.DeleteSchedule(ctx, sc.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

// TestDueSchedulesFiltersEnabledAndTime proves the due scan only returns enabled
// schedules whose next_run_at has passed.
func TestDueSchedulesFiltersEnabledAndTime(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	past, _ := st.CreateSchedule(ctx, "acme/app", "main", "* * * * *", true, "", now.Add(-time.Minute))
	_, _ = st.CreateSchedule(ctx, "acme/app", "main", "* * * * *", true, "", now.Add(time.Hour))     // future
	_, _ = st.CreateSchedule(ctx, "acme/app", "main", "* * * * *", false, "", now.Add(-time.Minute)) // disabled

	due, err := st.DueSchedules(ctx, now)
	if err != nil {
		t.Fatalf("DueSchedules: %v", err)
	}
	if len(due) != 1 || due[0].ID != past.ID {
		t.Fatalf("expected only the past enabled schedule (id=%d), got %+v", past.ID, due)
	}
}

// TestClaimScheduleFireSingleFire is the core replica-safety proof: the first
// claim with the value we read wins; a second claim with the SAME expected value
// (a racing replica that read the pre-fire state) loses. It also proves the claim
// advances next_run_at and stamps last_run_at.
func TestClaimScheduleFireSingleFire(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	original := now.Add(-time.Minute) // due in the past

	sc, err := st.CreateSchedule(ctx, "acme/app", "main", "* * * * *", true, "", original)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	newNext := now.Add(time.Minute)

	// Replica A claims with the value it read (original) — wins.
	won, err := st.ClaimScheduleFire(ctx, sc.ID, original, newNext, now)
	if err != nil {
		t.Fatalf("ClaimScheduleFire A: %v", err)
	}
	if !won {
		t.Fatal("replica A should have won the claim")
	}

	// Replica B raced: it also read `original` before A advanced it. Its
	// compare-and-set must fail — this is what prevents a double-fire.
	won2, err := st.ClaimScheduleFire(ctx, sc.ID, original, now.Add(2*time.Minute), now)
	if err != nil {
		t.Fatalf("ClaimScheduleFire B: %v", err)
	}
	if won2 {
		t.Fatal("replica B must NOT win a claim on an already-advanced schedule (double-fire!)")
	}

	// State reflects exactly one fire: next_run_at advanced, last_run_at stamped.
	got, err := st.GetSchedule(ctx, sc.ID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if !got.NextRunAt.Equal(newNext) {
		t.Errorf("next_run_at = %v, want %v (advanced)", got.NextRunAt, newNext)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(now) {
		t.Errorf("last_run_at = %v, want %v", got.LastRunAt, now)
	}
}

// TestClaimScheduleFireSkipsDisabled proves a schedule disabled between the due
// read and the claim is not fired.
func TestClaimScheduleFireSkipsDisabled(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	original := now.Add(-time.Minute)

	sc, _ := st.CreateSchedule(ctx, "acme/app", "main", "* * * * *", true, "", original)
	falseVal := false
	if _, err := st.UpdateSchedule(ctx, sc.ID, nil, nil, &falseVal, original); err != nil {
		t.Fatalf("disable: %v", err)
	}
	won, err := st.ClaimScheduleFire(ctx, sc.ID, original, now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("ClaimScheduleFire: %v", err)
	}
	if won {
		t.Fatal("must not claim a disabled schedule")
	}
}
