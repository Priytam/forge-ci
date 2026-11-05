package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

// QueueStat is the pending work for one set of required runner tags.
type QueueStat struct {
	Tags []string `json:"tags"`
	// Pending jobs waiting for a runner with (at least) these tags.
	Pending int `json:"pending"`
	// OldestWaitSeconds is how long the oldest of them has been queued.
	OldestWaitSeconds int `json:"oldest_wait_seconds"`
	// EligibleOnline / EligibleBusy: online, unpaused runners whose tags
	// cover these, and how many of those are running something right now.
	EligibleOnline int `json:"eligible_online"`
	EligibleBusy   int `json:"eligible_busy"`
}

// RunnerLoadRow is one runner with what it is doing.
type RunnerLoadRow struct {
	ID            string    `json:"id"`
	Executor      string    `json:"executor"`
	Tags          []string  `json:"tags"`
	Paused        bool      `json:"paused"`
	Online        bool      `json:"online"`
	LastContactAt time.Time `json:"last_contact_at"`
	Running       int       `json:"running"`
	Concurrency   int       `json:"concurrency"`
	// RunningJobs: up to 5 jobs it is running now.
	RunningJobs []RunnerJobRef `json:"running_jobs"`
	// Jobs / BusySeconds: jobs started, and time spent running jobs, within
	// the load's range (utilisation = BusySeconds / (range seconds × slots)).
	Jobs        int `json:"jobs"`
	BusySeconds int `json:"busy_seconds"`
}

// RunnerJobRef points at a running job.
type RunnerJobRef struct {
	JobID      int64     `json:"job_id"`
	PipelineID int64     `json:"pipeline_id"`
	Repo       string    `json:"repo"`
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
}

// RunnerLoad is the runner load page: per-tag queue depth and per-runner
// activity.
type RunnerLoad struct {
	// From / To: the range per-runner Jobs and BusySeconds cover.
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Queues  []QueueStat     `json:"queues"`
	Runners []RunnerLoadRow `json:"runners"`
	Pending int             `json:"pending"`
	Running int             `json:"running"`
}

func tagsKey(tags []string) string {
	t := append([]string(nil), tags...)
	sort.Strings(t)
	return strings.Join(t, ",")
}

func covers(have, need []string) bool {
	set := map[string]bool{}
	for _, t := range have {
		set[t] = true
	}
	for _, t := range need {
		if !set[t] {
			return false
		}
	}
	return true
}

// GetRunnerLoad computes queue depth per required tag set and per-runner load,
// with each runner's jobs and busy time over [from, to).
func (s *Store) GetRunnerLoad(ctx context.Context, from, to time.Time) (*RunnerLoad, error) {
	out := &RunnerLoad{From: from, To: to, Queues: []QueueStat{}, Runners: []RunnerLoadRow{}}

	// Busy time is each job's run clipped to the range, running jobs included.
	rrows, err := s.pool.Query(ctx,
		`SELECT r.id, r.executor, r.tags, r.paused, r.last_contact_at, r.concurrency,
		        (SELECT count(*) FROM jobs j WHERE j.runner_id = r.id AND j.status = 'running'),
		        (SELECT count(*) FROM jobs j WHERE j.runner_id = r.id AND j.started_at >= $1 AND j.started_at < $2),
		        COALESCE((SELECT sum(EXTRACT(EPOCH FROM (LEAST(COALESCE(j.finished_at, now()), $2) - GREATEST(j.started_at, $1))))::int
		                  FROM jobs j WHERE j.runner_id = r.id AND j.started_at < $2
		                    AND COALESCE(j.finished_at, now()) > $1), 0)
		 FROM runners r ORDER BY r.last_contact_at DESC`, from, to)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var r RunnerLoadRow
		if err := rrows.Scan(&r.ID, &r.Executor, &r.Tags, &r.Paused, &r.LastContactAt, &r.Concurrency, &r.Running,
			&r.Jobs, &r.BusySeconds); err != nil {
			rrows.Close()
			return nil, err
		}
		r.Online = time.Since(r.LastContactAt) < onlineWindow
		r.RunningJobs = []RunnerJobRef{}
		out.Runners = append(out.Runners, r)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	jrows, err := s.pool.Query(ctx,
		`SELECT j.runner_id, j.id, j.pipeline_id, p.repo, j.name, j.started_at
		 FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE j.status = 'running' AND j.runner_id IS NOT NULL ORDER BY j.started_at`)
	if err != nil {
		return nil, err
	}
	byRunner := map[string]*RunnerLoadRow{}
	for i := range out.Runners {
		byRunner[out.Runners[i].ID] = &out.Runners[i]
	}
	for jrows.Next() {
		var rid string
		var ref RunnerJobRef
		if err := jrows.Scan(&rid, &ref.JobID, &ref.PipelineID, &ref.Repo, &ref.Name, &ref.StartedAt); err != nil {
			jrows.Close()
			return nil, err
		}
		out.Running++
		if r := byRunner[rid]; r != nil && len(r.RunningJobs) < 5 {
			r.RunningJobs = append(r.RunningJobs, ref)
		}
	}
	jrows.Close()
	if err := jrows.Err(); err != nil {
		return nil, err
	}

	qrows, err := s.pool.Query(ctx,
		`SELECT tags, count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(queued_at))::int, 0)
		 FROM jobs WHERE status = 'pending' GROUP BY tags`)
	if err != nil {
		return nil, err
	}
	merged := map[string]*QueueStat{}
	for qrows.Next() {
		var tags []string
		var n, wait int
		if err := qrows.Scan(&tags, &n, &wait); err != nil {
			qrows.Close()
			return nil, err
		}
		k := tagsKey(tags)
		q := merged[k]
		if q == nil {
			sorted := append([]string{}, tags...)
			sort.Strings(sorted)
			q = &QueueStat{Tags: sorted}
			merged[k] = q
		}
		q.Pending += n
		if wait > q.OldestWaitSeconds {
			q.OldestWaitSeconds = wait
		}
		out.Pending += n
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return nil, err
	}
	for _, q := range merged {
		for _, r := range out.Runners {
			if r.Online && !r.Paused && covers(r.Tags, q.Tags) {
				q.EligibleOnline++
				if r.Running > 0 {
					q.EligibleBusy++
				}
			}
		}
		out.Queues = append(out.Queues, *q)
	}
	sort.Slice(out.Queues, func(i, j int) bool {
		if out.Queues[i].Pending != out.Queues[j].Pending {
			return out.Queues[i].Pending > out.Queues[j].Pending
		}
		return tagsKey(out.Queues[i].Tags) < tagsKey(out.Queues[j].Tags)
	})
	return out, nil
}
