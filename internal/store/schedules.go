package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// ---- cron-scheduled pipelines ----

// CreateSchedule inserts a schedule. nextRunAt is the caller-computed next fire
// time (from the cron expression, in UTC); the store does not parse cron itself.
func (s *Store) CreateSchedule(ctx context.Context, repo, ref, cron string, enabled bool, createdBy string, nextRunAt time.Time) (*proto.Schedule, error) {
	var sc proto.Schedule
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pipeline_schedules (repo, ref, cron, enabled, created_by, next_run_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id, repo, ref, cron, enabled, created_by, created_at, last_run_at, next_run_at`,
		repo, ref, cron, enabled, createdBy, nextRunAt.UTC()).
		Scan(&sc.ID, &sc.Repo, &sc.Ref, &sc.Cron, &sc.Enabled, &sc.CreatedBy,
			&sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt)
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// ListSchedules returns a repo's schedules (all schedules when repo==""),
// newest first.
func (s *Store) ListSchedules(ctx context.Context, repo string) ([]proto.Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, ref, cron, enabled, created_by, created_at, last_run_at, next_run_at
		 FROM pipeline_schedules
		 WHERE ($1 = '' OR repo = $1)
		 ORDER BY id DESC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Schedule{}
	for rows.Next() {
		var sc proto.Schedule
		if err := rows.Scan(&sc.ID, &sc.Repo, &sc.Ref, &sc.Cron, &sc.Enabled,
			&sc.CreatedBy, &sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetSchedule returns one schedule by id (ErrNotFound when missing).
func (s *Store) GetSchedule(ctx context.Context, id int64) (*proto.Schedule, error) {
	var sc proto.Schedule
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, ref, cron, enabled, created_by, created_at, last_run_at, next_run_at
		 FROM pipeline_schedules WHERE id=$1`, id).
		Scan(&sc.ID, &sc.Repo, &sc.Ref, &sc.Cron, &sc.Enabled, &sc.CreatedBy,
			&sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// UpdateSchedule applies partial updates (nil = leave unchanged) and always
// resets next_run_at to the caller-recomputed value (from the effective cron).
// Returns ErrNotFound when the id does not exist.
func (s *Store) UpdateSchedule(ctx context.Context, id int64, cron, ref *string, enabled *bool, nextRunAt time.Time) (*proto.Schedule, error) {
	var sc proto.Schedule
	err := s.pool.QueryRow(ctx,
		`UPDATE pipeline_schedules SET
		   cron        = COALESCE($2, cron),
		   ref         = COALESCE($3, ref),
		   enabled     = COALESCE($4, enabled),
		   next_run_at = $5
		 WHERE id=$1
		 RETURNING id, repo, ref, cron, enabled, created_by, created_at, last_run_at, next_run_at`,
		id, cron, ref, enabled, nextRunAt.UTC()).
		Scan(&sc.ID, &sc.Repo, &sc.Ref, &sc.Cron, &sc.Enabled, &sc.CreatedBy,
			&sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sc, nil
}

// DeleteSchedule removes a schedule (ErrNotFound when missing).
func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pipeline_schedules WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DueSchedules returns enabled schedules whose next_run_at is at or before now,
// so the scheduler can attempt to claim and fire each one.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]proto.Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, ref, cron, enabled, created_by, created_at, last_run_at, next_run_at
		 FROM pipeline_schedules
		 WHERE enabled AND next_run_at <= $1
		 ORDER BY id`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Schedule{}
	for rows.Next() {
		var sc proto.Schedule
		if err := rows.Scan(&sc.ID, &sc.Repo, &sc.Ref, &sc.Cron, &sc.Enabled,
			&sc.CreatedBy, &sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ClaimScheduleFire atomically claims a due schedule for firing. It is the
// single-fire, replica-safe, catch-up-safe compare-and-set: it advances
// next_run_at to newNext and stamps last_run_at=now ONLY when next_run_at still
// equals the value the caller read (expected). A concurrent tick/replica that
// already fired this schedule advanced next_run_at, so the compare fails and this
// call returns false — the caller must not fire. Advancing next_run_at to the
// next FUTURE slot (rather than the slot immediately after the missed one) is the
// catch-up policy: a schedule that missed windows fires once, then resumes on its
// normal cadence — it never storm-fires the backlog.
func (s *Store) ClaimScheduleFire(ctx context.Context, id int64, expected, newNext, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE pipeline_schedules
		 SET next_run_at = $3, last_run_at = $4
		 WHERE id = $1 AND enabled AND next_run_at = $2`,
		id, expected.UTC(), newNext.UTC(), now.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// BuildScheduledPipeline creates a pipeline for a schedule's (repo, ref),
// reproducing the webhook trigger path: load the repo's registered config,
// resolve the ref to its tip sha, compile with source=schedule (so rules keyed
// on CI_PIPELINE_SOURCE == "schedule" apply), stamp the current config version,
// and create the pipeline. Returns ErrNotFound when the repo has no registered
// config (a schedule cannot run without one) — the caller logs and skips.
func (s *Store) BuildScheduledPipeline(ctx context.Context, repo, ref string) (*proto.Pipeline, error) {
	// Resolve the ref tip first so config-from-repo can fetch .forge-ci.yml at the
	// exact sha the scheduled run builds on (same helper as the webhook path).
	sha, err := s.ResolveRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	config, configVersion, cfgSource, err := s.ResolvePipelineConfig(ctx, repo, sha)
	if err != nil {
		return nil, err // ErrNotFound when neither in-repo nor registered config exists
	}
	jobs, err := compiler.Compile(config, ref, compiler.SourceSchedule, s.TemplateResolver(ctx, repo))
	if err != nil {
		return nil, err
	}
	opts, err := compiler.Options(config)
	if err != nil {
		return nil, err
	}
	return s.CreatePipeline(ctx,
		proto.CreatePipelineRequest{Repo: repo, Ref: ref, SHA: sha, Config: config, TriggeredBy: "schedule", ConfigSource: cfgSource, Source: compiler.SourceSchedule},
		jobs, configVersion, opts.AutoCancel, opts.FailFast)
}
