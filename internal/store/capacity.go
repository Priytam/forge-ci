package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Capacity history: a snapshot of slots and queue depth each minute, so the
// runners page can chart capacity and the queue over a range and compare
// with the previous period. Kept long enough for a 30-day range and the 30
// days before it.

// CapacityRetention is how long snapshots are kept.
const CapacityRetention = 62 * 24 * time.Hour

// CapacitySnapshot is the fleet at one moment. Slots are runner
// concurrency; stale runners are left out. A runner counts once, by its
// state: offline, else draining, else paused, else available.
type CapacitySnapshot struct {
	At             time.Time `json:"at"`
	TotalSlots     int       `json:"total_slots"`
	AvailableSlots int       `json:"available_slots"`
	BusySlots      int       `json:"busy_slots"`
	PausedSlots    int       `json:"paused_slots"`
	DrainingSlots  int       `json:"draining_slots"`
	OfflineSlots   int       `json:"offline_slots"`
	Pending        int       `json:"pending"`
	OldestWait     int       `json:"oldest_wait_seconds"`
	Starved        []string  `json:"starved_tags"`
}

// CurrentCapacity computes the fleet's capacity and queue depth now.
func (s *Store) CurrentCapacity(ctx context.Context) (CapacitySnapshot, error) {
	snap := CapacitySnapshot{At: time.Now().UTC(), Starved: []string{}}
	runners, err := s.ListRunners(ctx)
	if err != nil {
		return snap, err
	}
	type tagCap struct{ total, available int }
	tags := map[string]*tagCap{}
	for _, r := range runners {
		if r.Stale {
			continue
		}
		slots := max(1, r.Concurrency)
		snap.TotalSlots += slots
		available := false
		switch {
		case !r.Online:
			snap.OfflineSlots += slots
		case r.Draining:
			snap.DrainingSlots += slots
		case r.Paused:
			snap.PausedSlots += slots
		default:
			available = true
			snap.AvailableSlots += slots
			snap.BusySlots += min(r.RunningJobs, slots)
		}
		for _, t := range r.Tags {
			tc := tags[t]
			if tc == nil {
				tc = &tagCap{}
				tags[t] = tc
			}
			tc.total += slots
			if available {
				tc.available += slots
			}
		}
	}
	for t, tc := range tags {
		if tc.available == 0 {
			snap.Starved = append(snap.Starved, t)
		}
	}
	sort.Strings(snap.Starved)
	err = s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(queued_at))::int, 0)
		 FROM jobs WHERE status = 'pending'`).Scan(&snap.Pending, &snap.OldestWait)
	return snap, err
}

// RecordCapacitySnapshot stores this minute's snapshot once, however many
// servers run the scheduler. It reports whether a row was written.
func (s *Store) RecordCapacitySnapshot(ctx context.Context) (bool, error) {
	snap, err := s.CurrentCapacity(ctx)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO runner_capacity_snapshots
		   (at, total_slots, available_slots, busy_slots, paused_slots, draining_slots, offline_slots, pending, oldest_wait_s, starved_tags)
		 VALUES (date_trunc('minute', now()), $1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (at) DO NOTHING`,
		snap.TotalSlots, snap.AvailableSlots, snap.BusySlots, snap.PausedSlots, snap.DrainingSlots,
		snap.OfflineSlots, snap.Pending, snap.OldestWait, snap.Starved)
	return err == nil && tag.RowsAffected() > 0, err
}

// PruneCapacitySnapshots drops snapshots older than CapacityRetention.
func (s *Store) PruneCapacitySnapshots(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM runner_capacity_snapshots WHERE at < now() - $1::interval`, CapacityRetention.String())
	return err
}

// capacityBucket is the snapshots that fell in one history bucket.
type capacityBucket struct {
	total, available, pendingAvg float64
	pendingMax, oldestMax, n     int
}

// capacityHistory aggregates snapshots per bucket over the range, plus the
// summary's capacity and queue figures.
func (s *Store) capacityHistory(ctx context.Context, r Range) (map[time.Time]capacityBucket, capacityBucket, int, error) {
	out := map[time.Time]capacityBucket{}
	var all capacityBucket
	var availableSlotSeconds int
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT to_timestamp(floor(EXTRACT(EPOCH FROM at) / %[1]d) * %[1]d), avg(total_slots), avg(available_slots),
		       avg(pending), max(pending), max(oldest_wait_s), count(*), sum(available_slots)
		FROM runner_capacity_snapshots WHERE at >= $1 AND at < $2 GROUP BY 1`, int(r.Bucket.Seconds())), r.From, r.To)
	if err != nil {
		return nil, all, 0, err
	}
	defer rows.Close()
	var totalSum, availSum, pendingSum float64
	for rows.Next() {
		var at time.Time
		var b capacityBucket
		var slotMinutes int
		if err := rows.Scan(&at, &b.total, &b.available, &b.pendingAvg, &b.pendingMax, &b.oldestMax, &b.n, &slotMinutes); err != nil {
			return nil, all, 0, err
		}
		out[at.UTC()] = b
		all.n += b.n
		totalSum += b.total * float64(b.n)
		availSum += b.available * float64(b.n)
		pendingSum += b.pendingAvg * float64(b.n)
		all.pendingMax = max(all.pendingMax, b.pendingMax)
		all.oldestMax = max(all.oldestMax, b.oldestMax)
		availableSlotSeconds += slotMinutes * 60
	}
	if all.n > 0 {
		all.total, all.available, all.pendingAvg = totalSum/float64(all.n), availSum/float64(all.n), pendingSum/float64(all.n)
	}
	return out, all, availableSlotSeconds, rows.Err()
}

func ptr[T any](v T) *T { return &v }
