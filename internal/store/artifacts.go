package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// SaveArtifact records artifact metadata; the blob was already written to
// disk by the API layer at path. The per-artifact expiry is computed
// SERVER-SIDE from the owning job's artifact_expire_seconds (set from the
// job's artifacts.expire_in at compile time): when > 0, expires_at is
// now()+that; otherwise it is NULL and the artifact relies on the
// RETENTION_DAYS sweep. Computing it here (not trusting the runner) keeps the
// TTL authoritative and clock-consistent with the GC sweep.
func (s *Store) SaveArtifact(ctx context.Context, jobID int64, name, path string, size int64) (*proto.ArtifactInfo, error) {
	var a proto.ArtifactInfo
	err := s.pool.QueryRow(ctx,
		`INSERT INTO artifacts (job_id, name, size_bytes, path, expires_at)
		 SELECT $1,$2,$3,$4,
		        CASE WHEN j.artifact_expire_seconds > 0
		             THEN now() + make_interval(secs => j.artifact_expire_seconds)
		             ELSE NULL END
		 FROM jobs j WHERE j.id = $1
		 RETURNING id, created_at, expires_at`,
		jobID, name, size, path).Scan(&a.ID, &a.CreatedAt, &a.ExpiresAt)
	if err != nil {
		return nil, err
	}
	a.JobID, a.Name, a.SizeBytes = jobID, name, size
	return &a, nil
}

// ListArtifacts returns artifact metadata, optionally filtered by repo or job.
func (s *Store) ListArtifacts(ctx context.Context, repo string, jobID int64) ([]proto.ArtifactInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.job_id, j.name, j.pipeline_id, p.repo, a.name, a.size_bytes, a.created_at, a.expires_at
		 FROM artifacts a
		 JOIN jobs j ON j.id = a.job_id
		 JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE ($1 = '' OR p.repo = $1) AND ($2 = 0 OR a.job_id = $2)
		 ORDER BY a.id DESC LIMIT 200`, repo, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.ArtifactInfo{}
	for rows.Next() {
		var a proto.ArtifactInfo
		if err := rows.Scan(&a.ID, &a.JobID, &a.JobName, &a.PipelineID, &a.Repo,
			&a.Name, &a.SizeBytes, &a.CreatedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ExpiredArtifactBlobKeys_ByExpiry returns the blob keys of artifacts whose
// per-artifact expires_at has passed. This is INDEPENDENT of RETENTION_DAYS:
// a short expire_in expires promptly even when the retention window is large or
// disabled. Artifacts with a NULL expires_at (no explicit expire_in) are never
// returned here — they rely on the RETENTION_DAYS pipeline sweep.
func (s *Store) ExpiredArtifactBlobKeysByExpiry(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT path FROM artifacts WHERE expires_at IS NOT NULL AND expires_at < now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteExpiredArtifactsByExpiry deletes artifact rows whose expires_at has
// passed and returns the number removed. Callers should delete the blobs
// (ExpiredArtifactBlobKeysByExpiry) first, then the rows here.
func (s *Store) DeleteExpiredArtifactsByExpiry(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM artifacts WHERE expires_at IS NOT NULL AND expires_at < now()`)
	return tag.RowsAffected(), err
}

// SaveJUnitReport upserts a job's parsed test summary. A re-run's re-upload
// (same job id) replaces the previous row.
func (s *Store) SaveJUnitReport(ctx context.Context, r proto.JUnitReport) error {
	failures := r.Failures
	if failures == nil {
		failures = []proto.JUnitFailure{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO job_reports (job_id, total, passed, failed, skipped, duration_seconds, failures)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (job_id) DO UPDATE SET
		   total=EXCLUDED.total, passed=EXCLUDED.passed, failed=EXCLUDED.failed,
		   skipped=EXCLUDED.skipped, duration_seconds=EXCLUDED.duration_seconds,
		   failures=EXCLUDED.failures, created_at=now()`,
		r.JobID, r.Total, r.Passed, r.Failed, r.Skipped, r.DurationSeconds, failures)
	return err
}

// GetJUnitReport returns a job's parsed test summary, or ErrNotFound when the
// job has no report (no reports.junit, or the XML was empty/malformed).
func (s *Store) GetJUnitReport(ctx context.Context, jobID int64) (*proto.JUnitReport, error) {
	var r proto.JUnitReport
	err := s.pool.QueryRow(ctx,
		`SELECT job_id, total, passed, failed, skipped, duration_seconds, failures, created_at
		 FROM job_reports WHERE job_id=$1`, jobID).
		Scan(&r.JobID, &r.Total, &r.Passed, &r.Failed, &r.Skipped, &r.DurationSeconds, &r.Failures, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.Failures == nil {
		r.Failures = []proto.JUnitFailure{}
	}
	return &r, nil
}

// GetArtifactPath returns the on-disk path for a download, addressed by job.
func (s *Store) GetArtifactPath(ctx context.Context, artifactID int64) (path, name string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT path, name FROM artifacts WHERE id=$1`, artifactID).Scan(&path, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return path, name, err
}
