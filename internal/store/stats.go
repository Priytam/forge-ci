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
		// Finished24h is the DENOMINATOR SuccessRate24h is computed over
		// (success + failed in the window; cancelled runs are excluded). A
		// percentage without its sample size reads the same at n=1 as at
		// n=200, so the UI shows this beside it.
		Finished24h int `json:"finished_24h"`
	} `json:"now"`
	Pipelines []PipelineBucket `json:"pipelines"` // dense buckets over the range
	Jobs      []JobBucket      `json:"jobs"`
	// Range describes the window the series and rates cover; Previous is
	// the same-length window before it, for "▲4 vs last week".
	Range    RangeInfo     `json:"range"`
	Previous *PeriodTotals `json:"previous,omitempty"`
	Totals   PeriodTotals  `json:"totals"`
}

// RangeInfo echoes the resolved range.
type RangeInfo struct {
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	BucketSeconds int       `json:"bucket_seconds"`
	Label         string    `json:"label"`
}

// PeriodTotals are the comparable numbers for one window.
type PeriodTotals struct {
	Pipelines   int `json:"pipelines"`
	Finished    int `json:"finished"`
	Succeeded   int `json:"succeeded"`
	SuccessRate int `json:"success_rate"` // percent, -1 when nothing finished
	JobsStarted int `json:"jobs_started"`
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

// DashboardStats keeps the 24h shape for callers that pass no range.
func (s *Store) DashboardStats(ctx context.Context) (*DashboardStats, error) {
	r, _ := ParseRange("24h", "", "", "", time.Now())
	return s.DashboardStatsRange(ctx, r)
}

// DashboardStatsRange is the dashboard over any range, with the previous
// window for comparison.
func (s *Store) DashboardStatsRange(ctx context.Context, r Range) (*DashboardStats, error) {
	var st DashboardStats
	st.Range = RangeInfo{From: r.From, To: r.To, BucketSeconds: int(r.Bucket.Seconds()), Label: r.Label}

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

	// Series over the range (live from jobs, or hourly rollups beyond 7 days).
	buckets, finished, succeeded, err := s.PipelineSeries(ctx, r)
	if err != nil {
		return nil, err
	}
	st.Pipelines = buckets
	hist, _, err := s.RunnerHistory(ctx, r, HistoryFilter{})
	if err != nil {
		return nil, err
	}
	st.Jobs = make([]JobBucket, 0, len(hist))
	jobsStarted := 0
	for _, b := range hist {
		st.Jobs = append(st.Jobs, JobBucket{Hour: b.At, Count: b.JobsStarted})
		jobsStarted += b.JobsStarted
	}
	st.Totals = periodTotals(buckets, finished, succeeded, jobsStarted)
	// The same-length window before this one.
	prev := Range{From: r.From.Add(-r.To.Sub(r.From)), To: r.From, Bucket: r.Bucket, Label: r.Label}
	if pb, pf, ps, err := s.PipelineSeries(ctx, prev); err == nil {
		ph, _, _ := s.RunnerHistory(ctx, prev, HistoryFilter{})
		pj := 0
		for _, b := range ph {
			pj += b.JobsStarted
		}
		t := periodTotals(pb, pf, ps, pj)
		st.Previous = &t
	}
	// The 24h-named fields keep their meaning for the chosen range.
	st.Now.Finished24h = finished
	st.Now.SuccessRate24h = st.Totals.SuccessRate
	return &st, nil
}

func periodTotals(buckets []PipelineBucket, finished, succeeded, jobsStarted int) PeriodTotals {
	t := PeriodTotals{Finished: finished, Succeeded: succeeded, JobsStarted: jobsStarted, SuccessRate: -1}
	for _, b := range buckets {
		t.Pipelines += b.Success + b.Failed + b.Other
	}
	if finished > 0 {
		t.SuccessRate = succeeded * 100 / finished
	}
	return t
}
