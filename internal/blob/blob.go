// Package blob abstracts artifact blob storage. Two backends:
//
//   - local: files under ARTIFACTS_DIR (default ./data/artifacts). Dev default.
//   - s3: any S3-compatible object store — AWS S3, MinIO, or GCS in
//     interoperability (HMAC) mode — via the MinIO client.
//
// Selection and configuration are environment-driven; see
// docs/artifact-storage.md for step-by-step provider setup.
package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store interface {
	// Put streams r into the store under key and returns bytes written.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Get opens the blob at key for reading.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the blob at key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Kind names the backend ("local" or "s3") for logs/UI.
	Kind() string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// FromEnv builds the configured store. ARTIFACT_STORE=s3 selects the
// S3-compatible backend; anything else falls back to local disk.
func FromEnv() (Store, error) {
	if envOr("ARTIFACT_STORE", "local") != "s3" {
		return NewLocal(envOr("ARTIFACTS_DIR", "data/artifacts")), nil
	}
	endpoint := os.Getenv("S3_ENDPOINT") // e.g. s3.amazonaws.com, storage.googleapis.com, minio:9000
	bucket := os.Getenv("S3_BUCKET")
	access := os.Getenv("S3_ACCESS_KEY")
	secret := os.Getenv("S3_SECRET_KEY")
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		return nil, fmt.Errorf("ARTIFACT_STORE=s3 requires S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY")
	}
	useSSL := envOr("S3_USE_SSL", "true") == "true"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: useSSL,
		Region: os.Getenv("S3_REGION"),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client: %w", err)
	}
	return &s3Store{client: client, bucket: bucket}, nil
}

// ---- local ----

type localStore struct{ dir string }

func NewLocal(dir string) Store { return &localStore{dir: dir} }

func (l *localStore) Kind() string { return "local" }

func (l *localStore) path(key string) (string, error) {
	clean := filepath.Clean(key)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	return filepath.Join(l.dir, clean), nil
}

func (l *localStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(f, r)
}

func (l *localStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (l *localStore) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Best-effort prune of the now-empty per-job directory.
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// ---- s3-compatible ----

type s3Store struct {
	client *minio.Client
	bucket string
}

func (s *s3Store) Kind() string { return "s3" }

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1,
		minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy; surface missing-object errors now.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
