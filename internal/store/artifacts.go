package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// SaveArtifact records artifact metadata; the blob was already written to
// disk by the API layer at path.
func (s *Store) SaveArtifact(ctx context.Context, jobID int64, name, path string, size int64) (*proto.ArtifactInfo, error) {
	var a proto.ArtifactInfo
	err := s.pool.QueryRow(ctx,
		`INSERT INTO artifacts (job_id, name, size_bytes, path)
		 VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
		jobID, name, size, path).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	a.JobID, a.Name, a.SizeBytes = jobID, name, size
	return &a, nil
}

// ListArtifacts returns artifact metadata, optionally filtered by repo or job.
func (s *Store) ListArtifacts(ctx context.Context, repo string, jobID int64) ([]proto.ArtifactInfo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.job_id, j.name, j.pipeline_id, p.repo, a.name, a.size_bytes, a.created_at
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
			&a.Name, &a.SizeBytes, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
