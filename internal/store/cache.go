package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RepoForJob returns the repo that owns a job (via its pipeline). The cache
// endpoints use this to scope a cache to its repo server-side, so the runner
// never controls the repo dimension of a cache key.
func (s *Store) RepoForJob(ctx context.Context, jobID int64) (string, error) {
	var repo string
	err := s.pool.QueryRow(ctx,
		`SELECT p.repo FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id WHERE j.id=$1`,
		jobID).Scan(&repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return repo, err
}

// LookupCache resolves the blob key of the cache for repo under the first of
// keys that exists (the fallback chain: exact key, then prefix/default). It
// returns the matched key too so the caller can log which key hit. ErrNotFound
// when none of the keys has a cache entry.
func (s *Store) LookupCache(ctx context.Context, repo string, keys []string) (blobKey, matchedKey string, err error) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		err = s.pool.QueryRow(ctx,
			`SELECT blob_key FROM cache_entries WHERE repo=$1 AND cache_key=$2`,
			repo, k).Scan(&blobKey)
		if err == nil {
			return blobKey, k, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", "", err
		}
	}
	return "", "", ErrNotFound
}

// SaveCache upserts the cache metadata row for (repo, key), pointing it at
// blobKey with the given size. A repeat save for the same key overwrites the
// row (the blob key is deterministic, so the same object is overwritten).
func (s *Store) SaveCache(ctx context.Context, repo, key, blobKey string, size int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO cache_entries (repo, cache_key, blob_key, size_bytes, updated_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (repo, cache_key)
		 DO UPDATE SET blob_key=EXCLUDED.blob_key, size_bytes=EXCLUDED.size_bytes, updated_at=now()`,
		repo, key, blobKey, size)
	return err
}

// ExpiredCacheBlobKeys returns the blob keys of cache entries not updated within
// the retention window, so the caller can delete them from the blob store before
// removing the rows. Cache retention is age-based and independent of pipelines
// (a cache is shared across pipelines).
func (s *Store) ExpiredCacheBlobKeys(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT blob_key FROM cache_entries WHERE updated_at < now() - $1::interval`,
		olderThan.String())
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

// DeleteExpiredCache removes cache metadata rows not updated within the
// retention window. Call ExpiredCacheBlobKeys first and delete the blobs, then
// this to drop the rows.
func (s *Store) DeleteExpiredCache(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cache_entries WHERE updated_at < now() - $1::interval`,
		olderThan.String())
	return tag.RowsAffected(), err
}
