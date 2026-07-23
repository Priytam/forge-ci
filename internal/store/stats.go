package store

import (
	"context"
	"time"
)

// DashboardStats feeds the home-page dashboard: instantaneous counts plus
// 24h hourly activity series.
type DashboardStats struct {
	Now struct {
		RunningJobs     int `json:"running_jobs"`
		PendingJobs     int `json:"pending_jobs"`
		BlockedJobs     int `json:"blocked_jobs"`
		OnlineRunners   int `json:"online_runners"`
		ActiveExecutors int `json:"active_executors"` // forked executors/pods right now
		PipelinesToday  int `json:"pipelines_today"`
		SuccessRate24h  int `json:"success_rate_24h"` // percent, -1 when no finished pipelines
	} `json:"now"`
	Pipelines []PipelineBucket `json:"pipelines"` // last 24 hourly buckets
	Jobs      []JobBucket      `json:"jobs"`
}

type PipelineBucket struct {
	Hour    time.Time `json:"hour"`
	Success int       `json:"success"`
	Failed  int       `json:"failed"`
	Other   int       `json:"other"`
}

type JobBucket struct {
	Hour  time.Time `json:"hour"`
	Count int       `json:"count"`
}

func (s *Store) DashboardStats(ctx context.Context) (*DashboardStats, error) {
	var st DashboardStats

	err := s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE status='running'),
		  count(*) FILTER (WHERE status='pending'),
		  count(*) FILTER (WHERE status='blocked')
		FROM jobs`).Scan(&st.Now.RunningJobs, &st.Now.PendingJobs, &st.Now.BlockedJobs)
	if err != nil {
		return nil, err
	}
	st.Now.ActiveExecutors = st.Now.RunningJobs

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM runners
		 WHERE NOT paused AND last_contact_at > now() - interval '30 seconds'`).
		Scan(&st.Now.OnlineRunners); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pipelines WHERE created_at > date_trunc('day', now())`).
		Scan(&st.Now.PipelinesToday); err != nil {
		return nil, err
	}

	// Hourly pipeline outcomes for the last 24h, derived from job statuses
	// with the same precedence as deriveStatus.
	rows, err := s.pool.Query(ctx, `
		WITH ps AS (
		  SELECT p.id, date_trunc('hour', p.created_at) AS hour,
		    CASE
		      WHEN bool_or(j.status='failed')  THEN 'failed'
		      WHEN bool_or(j.status='canceled') THEN 'other'
		      WHEN bool_and(j.status='success') THEN 'success'
		      ELSE 'other'
		    END AS status
		  FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id
		  WHERE p.created_at > now() - interval '24 hours'
		  GROUP BY p.id
		)
		SELECT hour,
		  count(*) FILTER (WHERE status='success'),
		  count(*) FILTER (WHERE status='failed'),
		  count(*) FILTER (WHERE status='other')
		FROM ps GROUP BY hour ORDER BY hour`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byHour := map[time.Time]PipelineBucket{}
	finished, succeeded := 0, 0
	for rows.Next() {
		var b PipelineBucket
		if err := rows.Scan(&b.Hour, &b.Success, &b.Failed, &b.Other); err != nil {
			return nil, err
		}
		byHour[b.Hour.UTC()] = b
		finished += b.Success + b.Failed
		succeeded += b.Success
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	jrows, err := s.pool.Query(ctx, `
		SELECT date_trunc('hour', started_at), count(*)
		FROM jobs WHERE started_at > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer jrows.Close()
	jobsByHour := map[time.Time]int{}
	for jrows.Next() {
		var h time.Time
		var n int
		if err := jrows.Scan(&h, &n); err != nil {
			return nil, err
		}
		jobsByHour[h.UTC()] = n
	}
	if err := jrows.Err(); err != nil {
		return nil, err
	}

	// Dense series: emit all 24 buckets so charts show flat zeros, not gaps.
	start := time.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	for i := 0; i < 24; i++ {
		h := start.Add(time.Duration(i) * time.Hour)
		b := byHour[h]
		b.Hour = h
		st.Pipelines = append(st.Pipelines, b)
		st.Jobs = append(st.Jobs, JobBucket{Hour: h, Count: jobsByHour[h]})
	}

	st.Now.SuccessRate24h = -1
	if finished > 0 {
		st.Now.SuccessRate24h = succeeded * 100 / finished
	}
	return &st, nil
}
