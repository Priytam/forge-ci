package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// History: load, queue latency and job duration over time, per runner and
// overall; pipeline outcomes over time for the dashboard. Short ranges are
// computed from jobs; long ranges read hourly rollups (job_stats_hourly,
// pipeline_stats_hourly) that the scheduler fills for completed hours, so
// a 30-day view stays one small scan as history grows.

// Range is a time window and the bucket it is drawn in.
type Range struct {
	From, To time.Time
	Bucket   time.Duration
	Label    string // 24h | 7d | 30d | custom
}

// ParseRange reads range=24h|7d|30d, or from=/to= (RFC 3339) with an
// optional bucket=1h|6h|1d; the bucket defaults by span.
func ParseRange(rangeParam, from, to, bucket string, now time.Time) (Range, error) {
	r := Range{To: now, Label: rangeParam}
	switch rangeParam {
	case "", "24h":
		r.From, r.Bucket, r.Label = now.Add(-24*time.Hour), time.Hour, "24h"
	case "7d":
		r.From, r.Bucket = now.Add(-7*24*time.Hour), 6*time.Hour
	case "30d":
		r.From, r.Bucket = now.Add(-30*24*time.Hour), 24*time.Hour
	case "custom":
		f, err1 := time.Parse(time.RFC3339, from)
		t, err2 := time.Parse(time.RFC3339, to)
		if err1 != nil || err2 != nil || !t.After(f) {
			return r, errors.New("custom range needs from and to (RFC 3339, from < to)")
		}
		if t.Sub(f) > 400*24*time.Hour {
			return r, errors.New("range is longer than 400 days")
		}
		r.From, r.To = f, t
		switch span := t.Sub(f); {
		case span <= 48*time.Hour:
			r.Bucket = time.Hour
		case span <= 14*24*time.Hour:
			r.Bucket = 6 * time.Hour
		default:
			r.Bucket = 24 * time.Hour
		}
	default:
		return r, errors.New("range must be 24h, 7d, 30d or custom")
	}
	switch bucket {
	case "":
	case "1h":
		r.Bucket = time.Hour
	case "6h":
		r.Bucket = 6 * time.Hour
	case "1d":
		r.Bucket = 24 * time.Hour
	default:
		return r, errors.New("bucket must be 1h, 6h or 1d")
	}
	return r, nil
}

// useRollups: ranges beyond a week read the hourly rollups.
func (r Range) useRollups() bool { return r.To.Sub(r.From) > 7*24*time.Hour }

// bucketStart floors t to the bucket (UTC).
func (r Range) bucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(r.Bucket)
}

// HistoryBucket is one point of the runner history.
type HistoryBucket struct {
	At          time.Time `json:"at"`
	JobsStarted int       `json:"jobs_started"`
	BusySeconds int       `json:"busy_seconds"`
	BusySlots   float64   `json:"busy_slots"` // busy_seconds / bucket seconds
	WaitP50     float64   `json:"wait_p50"`   // queued_at → started_at, seconds
	WaitP95     float64   `json:"wait_p95"`
	DurationP50 float64   `json:"duration_p50"` // started_at → finished_at, seconds
	DurationP95 float64   `json:"duration_p95"`
	Finished    int       `json:"finished"`
	Failed      int       `json:"failed"`
}

// HistoryFilter narrows the history.
type HistoryFilter struct {
	Runner string // one runner, "" = all
	Repo   string
	Job    string // job name
}

// RunnerHistory returns the dense bucket series for a range.
func (s *Store) RunnerHistory(ctx context.Context, r Range, f HistoryFilter) ([]HistoryBucket, error) {
	byAt := map[time.Time]*HistoryBucket{}
	if r.useRollups() && f.Repo == "" && f.Job == "" {
		rows, err := s.pool.Query(ctx,
			`SELECT hour, jobs_started, busy_seconds, wait_p50, wait_p95, dur_p50, dur_p95, finished, failed
			 FROM job_stats_hourly
			 WHERE hour >= $1 AND hour < $2 AND runner_id = $3`, r.From, r.To, f.Runner)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var h time.Time
			var b HistoryBucket
			var w50, w95, d50, d95 float64
			if err := rows.Scan(&h, &b.JobsStarted, &b.BusySeconds, &w50, &w95, &d50, &d95, &b.Finished, &b.Failed); err != nil {
				return nil, err
			}
			at := r.bucketStart(h)
			acc := byAt[at]
			if acc == nil {
				acc = &HistoryBucket{At: at}
				byAt[at] = acc
			}
			// Percentiles of hourly percentiles: weighted by jobs started.
			n := float64(acc.JobsStarted + b.JobsStarted)
			if n > 0 {
				acc.WaitP50 = (acc.WaitP50*float64(acc.JobsStarted) + w50*float64(b.JobsStarted)) / n
				acc.WaitP95 = (acc.WaitP95*float64(acc.JobsStarted) + w95*float64(b.JobsStarted)) / n
				acc.DurationP50 = (acc.DurationP50*float64(acc.JobsStarted) + d50*float64(b.JobsStarted)) / n
				acc.DurationP95 = (acc.DurationP95*float64(acc.JobsStarted) + d95*float64(b.JobsStarted)) / n
			}
			acc.JobsStarted += b.JobsStarted
			acc.BusySeconds += b.BusySeconds
			acc.Finished += b.Finished
			acc.Failed += b.Failed
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	} else {
		conds := []string{"j.started_at >= $1", "j.started_at < $2"}
		args := []any{r.From, r.To, r.Bucket.Seconds()}
		if f.Runner != "" {
			args = append(args, f.Runner)
			conds = append(conds, fmt.Sprintf("j.runner_id = $%d", len(args)))
		}
		if f.Repo != "" {
			args = append(args, f.Repo)
			conds = append(conds, fmt.Sprintf("p.repo = $%d", len(args)))
		}
		if f.Job != "" {
			args = append(args, f.Job)
			conds = append(conds, fmt.Sprintf("j.name = $%d", len(args)))
		}
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT to_timestamp(floor(EXTRACT(EPOCH FROM j.started_at) / $3) * $3) AS b,
			       count(*),
			       COALESCE(sum(EXTRACT(EPOCH FROM (COALESCE(j.finished_at, now()) - j.started_at)))::int, 0),
			       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (j.started_at - j.queued_at))), 0),
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (j.started_at - j.queued_at))), 0),
			       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (j.finished_at - j.started_at))), 0),
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (j.finished_at - j.started_at))), 0),
			       count(*) FILTER (WHERE j.status IN ('success','failed')),
			       count(*) FILTER (WHERE j.status = 'failed')
			FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id
			WHERE %s AND j.started_at IS NOT NULL
			GROUP BY 1 ORDER BY 1`, strings.Join(conds, " AND ")), args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var b HistoryBucket
			if err := rows.Scan(&b.At, &b.JobsStarted, &b.BusySeconds, &b.WaitP50, &b.WaitP95, &b.DurationP50, &b.DurationP95, &b.Finished, &b.Failed); err != nil {
				return nil, err
			}
			b.At = b.At.UTC()
			byAt[b.At] = &b
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// Dense series: every bucket in the range, zeros where nothing ran.
	out := []HistoryBucket{}
	for at := r.bucketStart(r.From); at.Before(r.To); at = at.Add(r.Bucket) {
		b := byAt[at]
		if b == nil {
			b = &HistoryBucket{At: at}
		}
		b.BusySlots = float64(b.BusySeconds) / r.Bucket.Seconds()
		out = append(out, *b)
	}
	return out, nil
}

// JobsInBucket lists the jobs a history point was made of (hover → list).
func (s *Store) JobsInBucket(ctx context.Context, from, to time.Time, f HistoryFilter, limit int) ([]JobRow, error) {
	filter := JobFilter{Runner: f.Runner, Name: f.Job}
	if f.Repo != "" {
		filter.Repos = []string{f.Repo}
	}
	filter.StartedFrom, filter.StartedTo = &from, &to
	rows, _, err := s.ListJobsFiltered(ctx, filter, limit, 0)
	return rows, err
}

// ---- rollups ----

// RollupHour computes the hourly rollups for every completed hour since the
// last one recorded (bounded to backfill days per call), idempotently.
func (s *Store) RollupHour(ctx context.Context, backfill time.Duration) (int, error) {
	var last *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(hour) FROM job_stats_hourly`).Scan(&last); err != nil {
		return 0, err
	}
	start := time.Now().UTC().Add(-backfill).Truncate(time.Hour)
	if last != nil && last.Add(time.Hour).After(start) {
		start = last.UTC().Add(time.Hour)
	}
	end := time.Now().UTC().Truncate(time.Hour) // the current hour is not complete
	n := 0
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO job_stats_hourly (hour, runner_id, jobs_started, busy_seconds, wait_p50, wait_p95, dur_p50, dur_p95, finished, failed)
			SELECT $1::timestamptz, r.runner_id, r.n, r.busy, r.w50, r.w95, r.d50, r.d95, r.fin, r.failed FROM (
			  SELECT COALESCE(runner_id, '') AS runner_id, count(*) AS n,
			         COALESCE(sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(finished_at, now()), $1::timestamptz + interval '1 hour') - GREATEST(started_at, $1::timestamptz))))::int, 0) AS busy,
			         COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at))), 0) AS w50,
			         COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at))), 0) AS w95,
			         COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at))), 0) AS d50,
			         COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at))), 0) AS d95,
			         count(*) FILTER (WHERE status IN ('success','failed')) AS fin,
			         count(*) FILTER (WHERE status = 'failed') AS failed
			  FROM jobs WHERE started_at >= $1::timestamptz AND started_at < $1::timestamptz + interval '1 hour'
			  GROUP BY GROUPING SETS ((runner_id), ())
			) r
			ON CONFLICT (hour, runner_id) DO UPDATE SET jobs_started = EXCLUDED.jobs_started, busy_seconds = EXCLUDED.busy_seconds,
			  wait_p50 = EXCLUDED.wait_p50, wait_p95 = EXCLUDED.wait_p95, dur_p50 = EXCLUDED.dur_p50, dur_p95 = EXCLUDED.dur_p95,
			  finished = EXCLUDED.finished, failed = EXCLUDED.failed`, h); err != nil {
			return n, err
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO pipeline_stats_hourly (hour, repo, success, failed, other)
			SELECT $1::timestamptz, ps.repo, count(*) FILTER (WHERE ps.status='success'), count(*) FILTER (WHERE ps.status='failed'), count(*) FILTER (WHERE ps.status='other')
			FROM (
			  SELECT p.repo, CASE WHEN bool_or(j.status='failed') THEN 'failed' WHEN bool_or(j.status='canceled') THEN 'other'
			                      WHEN bool_and(j.status='success') THEN 'success' ELSE 'other' END AS status
			  FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id AND NOT j.superseded
			  WHERE p.created_at >= $1::timestamptz AND p.created_at < $1::timestamptz + interval '1 hour'
			  GROUP BY p.id, p.repo
			) ps GROUP BY ps.repo
			ON CONFLICT (hour, repo) DO UPDATE SET success = EXCLUDED.success, failed = EXCLUDED.failed, other = EXCLUDED.other`, h); err != nil {
			return n, err
		}
		n++
	}
	// Rows for an empty hour mark it done.
	if n == 0 && last == nil {
		_, _ = s.pool.Exec(ctx, `INSERT INTO job_stats_hourly (hour, runner_id) VALUES ($1, '') ON CONFLICT DO NOTHING`, end.Add(-time.Hour))
	}
	return n, nil
}

// PipelineSeries returns pipeline outcomes per bucket for a range (live from
// pipelines up to 7 days, rollups beyond), plus totals for the range.
func (s *Store) PipelineSeries(ctx context.Context, r Range) ([]PipelineBucket, int, int, error) {
	byAt := map[time.Time]*PipelineBucket{}
	var rows pgx.Rows
	var err error
	if r.useRollups() {
		rows, err = s.pool.Query(ctx,
			`SELECT hour, sum(success), sum(failed), sum(other) FROM pipeline_stats_hourly
			 WHERE hour >= $1 AND hour < $2 GROUP BY hour`, r.From, r.To)
	} else {
		rows, err = s.pool.Query(ctx, `
			WITH ps AS (
			  SELECT p.id, p.created_at,
			    CASE WHEN bool_or(j.status='failed') THEN 'failed' WHEN bool_or(j.status='canceled') THEN 'other'
			         WHEN bool_and(j.status='success') THEN 'success' ELSE 'other' END AS status
			  FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id AND NOT j.superseded
			  WHERE p.created_at >= $1 AND p.created_at < $2 GROUP BY p.id
			)
			SELECT created_at, (status='success')::int, (status='failed')::int, (status='other')::int FROM ps`, r.From, r.To)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	finished, succeeded := 0, 0
	for rows.Next() {
		var t time.Time
		var ok, failed, other int
		if err := rows.Scan(&t, &ok, &failed, &other); err != nil {
			return nil, 0, 0, err
		}
		at := r.bucketStart(t)
		b := byAt[at]
		if b == nil {
			b = &PipelineBucket{Hour: at}
			byAt[at] = b
		}
		b.Success += ok
		b.Failed += failed
		b.Other += other
		finished += ok + failed
		succeeded += ok
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	out := []PipelineBucket{}
	for at := r.bucketStart(r.From); at.Before(r.To); at = at.Add(r.Bucket) {
		b := byAt[at]
		if b == nil {
			b = &PipelineBucket{Hour: at}
		}
		out = append(out, *b)
	}
	return out, finished, succeeded, nil
}

// PruneRollups keeps rollups for keep.
func (s *Store) PruneRollups(ctx context.Context, keep time.Duration) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM job_stats_hourly WHERE hour < now() - $1::interval`, keep.String()); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM pipeline_stats_hourly WHERE hour < now() - $1::interval`, keep.String())
	return err
}

// Latency is the last window's queue wait and duration percentiles plus
// busy slots per runner (Prometheus).
type Latency struct {
	WaitP50, WaitP95         float64
	DurationP50, DurationP95 float64
	JobsStarted              int
	Runners                  []RunnerBusy
}

// RunnerBusy is a runner's average busy slots over the window.
type RunnerBusy struct {
	RunnerID  string
	BusySlots float64
}

// RecentLatency computes Latency over the last window.
func (s *Store) RecentLatency(ctx context.Context, window time.Duration) (*Latency, error) {
	var l Latency
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at))), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (started_at - queued_at))), 0),
		       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at))), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at))), 0),
		       count(*)
		FROM jobs WHERE started_at > now() - $1::interval`, window.String()).
		Scan(&l.WaitP50, &l.WaitP95, &l.DurationP50, &l.DurationP95, &l.JobsStarted); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT runner_id, COALESCE(sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(finished_at, now()), now()) - GREATEST(started_at, now() - $1::interval)))), 0) / $2
		FROM jobs WHERE runner_id IS NOT NULL AND started_at IS NOT NULL AND COALESCE(finished_at, now()) > now() - $1::interval
		GROUP BY runner_id ORDER BY runner_id`, window.String(), window.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r RunnerBusy
		if err := rows.Scan(&r.RunnerID, &r.BusySlots); err != nil {
			return nil, err
		}
		l.Runners = append(l.Runners, r)
	}
	return &l, rows.Err()
}
