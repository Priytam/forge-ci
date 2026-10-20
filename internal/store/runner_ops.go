package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Runner operations: drain / resume, version tracking, self-update requests
// and a rolling "upgrade all runners".

// RunnerState is what the acquire path needs to know about a runner.
type RunnerState struct {
	Paused          bool
	Draining        bool
	UpdateRequested bool
}

// TouchRunnerV records a runner's contact (with its version) and returns its
// state. A runner that reconnects with the version it was asked to update
// to has its update request cleared.
func (s *Store) TouchRunnerV(ctx context.Context, id, executor string, tags []string, version string) (RunnerState, error) {
	if tags == nil {
		tags = []string{}
	}
	var st RunnerState
	err := s.pool.QueryRow(ctx,
		`INSERT INTO runners (id, executor, tags, version)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (id) DO UPDATE
		   SET last_contact_at = now(), executor = EXCLUDED.executor, tags = EXCLUDED.tags,
		       version = EXCLUDED.version,
		       update_requested_at = CASE WHEN runners.update_requested_at IS NOT NULL AND runners.version <> EXCLUDED.version
		                                  AND EXCLUDED.version <> '' THEN NULL ELSE runners.update_requested_at END,
		       draining = CASE WHEN runners.update_requested_at IS NOT NULL AND runners.version <> EXCLUDED.version
		                       AND EXCLUDED.version <> '' THEN FALSE ELSE runners.draining END
		 RETURNING paused, draining, update_requested_at IS NOT NULL`, id, executor, tags, version).
		Scan(&st.Paused, &st.Draining, &st.UpdateRequested)
	return st, err
}

// SetRunnerDraining starts (true) or ends (false) a drain: a draining runner
// takes no new jobs and finishes what it has.
func (s *Store) SetRunnerDraining(ctx context.Context, id string, draining bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runners SET draining=$2, drain_requested_at = CASE WHEN $2 THEN now() ELSE NULL END,
		        update_requested_at = CASE WHEN $2 THEN update_requested_at ELSE NULL END
		 WHERE id=$1`, id, draining)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RequestRunnerUpdate drains a runner and asks it to self-update once idle.
func (s *Store) RequestRunnerUpdate(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runners SET draining=TRUE, drain_requested_at=COALESCE(drain_requested_at, now()), update_requested_at=now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RunnerRunningJobs counts a runner's in-flight jobs ("draining — 2 left").
func (s *Store) RunnerRunningJobs(ctx context.Context, id string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status='running' AND runner_id=$1`, id).Scan(&n)
	return n, err
}

// ---- rolling upgrade ----

// Rollout is an "upgrade all runners" in progress: one outdated runner at a
// time is drained, told to update, and waited for.
type Rollout struct {
	Active    bool      `json:"active"`
	Target    string    `json:"target"` // server version the runners should reach
	Current   string    `json:"current,omitempty"`
	Done      []string  `json:"done"`
	Skipped   []string  `json:"skipped"` // offline or not self-updating
	StartedAt time.Time `json:"started_at,omitempty"`
	By        string    `json:"by,omitempty"`
	Note      string    `json:"note,omitempty"`
}

const rolloutKey = "runner_rollout"

func (s *Store) Rollout(ctx context.Context) (Rollout, error) {
	var raw []byte
	var r Rollout
	err := s.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key=$1`, rolloutKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(raw, &r)
}

func (s *Store) saveRollout(ctx context.Context, r Rollout) error {
	raw, _ := json.Marshal(r)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, rolloutKey, raw)
	return err
}

// StartRollout begins upgrading every outdated runner to target.
func (s *Store) StartRollout(ctx context.Context, target, by string) (Rollout, error) {
	r := Rollout{Active: true, Target: target, Done: []string{}, Skipped: []string{}, StartedAt: time.Now(), By: by}
	return r, s.saveRollout(ctx, r)
}

// StopRollout ends a rollout; the runner being upgraded keeps its request.
func (s *Store) StopRollout(ctx context.Context) error {
	r, err := s.Rollout(ctx)
	if err != nil {
		return err
	}
	r.Active = false
	r.Note = "stopped"
	return s.saveRollout(ctx, r)
}

// AdvanceRollout moves a rollout one step: if the current runner has
// reached the target (or vanished), mark it done and pick the next online
// runner whose version differs. Returns the runner now being upgraded.
func (s *Store) AdvanceRollout(ctx context.Context, staleAfter time.Duration) (string, error) {
	r, err := s.Rollout(ctx)
	if err != nil || !r.Active {
		return "", err
	}
	if r.Current != "" {
		var version string
		var online bool
		err := s.pool.QueryRow(ctx, `SELECT version, last_contact_at > now() - $2::interval FROM runners WHERE id=$1`, r.Current, staleAfter.String()).Scan(&version, &online)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			r.Skipped = append(r.Skipped, r.Current)
			r.Current = ""
		case err != nil:
			return "", err
		case version == r.Target:
			r.Done = append(r.Done, r.Current)
			r.Current = ""
		case !online:
			// Gone quiet mid-update (restarting, or dead): give it time.
			var requested time.Time
			_ = s.pool.QueryRow(ctx, `SELECT COALESCE(update_requested_at, now()) FROM runners WHERE id=$1`, r.Current).Scan(&requested)
			if time.Since(requested) > 10*time.Minute {
				r.Skipped = append(r.Skipped, r.Current)
				r.Current = ""
			}
		default:
			return r.Current, nil // still draining / updating
		}
	}
	if r.Current == "" {
		var next string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM runners
			 WHERE version <> $1 AND NOT paused AND last_contact_at > now() - $2::interval
			   AND NOT (id = ANY($3)) AND NOT (id = ANY($4))
			 ORDER BY last_contact_at DESC LIMIT 1`, r.Target, staleAfter.String(), r.Done, r.Skipped).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			r.Active = false
			r.Note = "complete"
			return "", s.saveRollout(ctx, r)
		}
		if err != nil {
			return "", err
		}
		if err := s.RequestRunnerUpdate(ctx, next); err != nil {
			return "", err
		}
		r.Current = next
	}
	return r.Current, s.saveRollout(ctx, r)
}

// RunnerState reads a runner's flags without touching last_contact_at.
func (s *Store) RunnerState(ctx context.Context, id string) (RunnerState, error) {
	var st RunnerState
	err := s.pool.QueryRow(ctx, `SELECT paused, draining, update_requested_at IS NOT NULL FROM runners WHERE id=$1`, id).
		Scan(&st.Paused, &st.Draining, &st.UpdateRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}
