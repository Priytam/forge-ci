package store

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/oidc"
)

// loadOrCreateOIDCSigner resolves the RSA key that signs per-job OIDC ID tokens,
// in priority order:
//
//  1. OIDC_PRIVATE_KEY (PEM) from the env — when set it always wins and nothing
//     is persisted (operator-managed key, e.g. mounted from a secret manager).
//  2. the single persisted key in oidc_keys (decrypted with FORGE_SECRET_KEY).
//  3. otherwise: generate an RSA-2048 key once, persist it (encrypted), and use
//     it — so the published JWKS/kid stay stable across restarts.
//
// The generated key is written under an advisory-locked read-then-insert so two
// server replicas starting on a fresh DB converge on one key rather than each
// minting its own.
func (s *Store) loadOrCreateOIDCSigner(ctx context.Context) (*oidc.Signer, error) {
	if pem := strings.TrimSpace(os.Getenv("OIDC_PRIVATE_KEY")); pem != "" {
		key, err := oidc.ParsePrivateKeyPEM(pem)
		if err != nil {
			return nil, err
		}
		slog.Info("oidc: signing key loaded from OIDC_PRIVATE_KEY env")
		return oidc.NewSigner(key), nil
	}

	if key, ok, err := s.loadPersistedOIDCKey(ctx); err != nil {
		return nil, err
	} else if ok {
		return oidc.NewSigner(key), nil
	}

	// None found — generate and persist under an advisory lock so concurrent
	// replicas don't each insert a different key.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Reuse the migration advisory-lock namespace with a distinct sub-key by
	// locking on a fixed constant; any stable constant serializes the section.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", oidcKeyLockKey); err != nil {
		return nil, err
	}
	// Re-check inside the lock: another replica may have inserted meanwhile.
	if key, ok, err := txLoadPersistedOIDCKey(ctx, tx, s.cipher.Decrypt); err != nil {
		return nil, err
	} else if ok {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return oidc.NewSigner(key), nil
	}

	key, err := oidc.GenerateKey()
	if err != nil {
		return nil, err
	}
	pem, err := oidc.MarshalPrivateKeyPEM(key)
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(pem)
	if err != nil {
		return nil, err
	}
	kid := oidc.KeyID(&key.PublicKey)
	if _, err := tx.Exec(ctx,
		`INSERT INTO oidc_keys (kid, private_key_pem_enc) VALUES ($1,$2)
		 ON CONFLICT (kid) DO NOTHING`, kid, enc); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	slog.Info("oidc: generated and persisted a new RSA-2048 signing key", "kid", kid,
		"encrypted_at_rest", s.cipher.HasKey())
	return oidc.NewSigner(key), nil
}

// oidcKeyLockKey is a stable advisory-lock key for the generate-and-persist
// section ("OidC" as ASCII bytes).
const oidcKeyLockKey int64 = 0x4f696443

// loadPersistedOIDCKey reads the active persisted key (oldest by created_at).
func (s *Store) loadPersistedOIDCKey(ctx context.Context) (*rsa.PrivateKey, bool, error) {
	return txLoadPersistedOIDCKey(ctx, s.pool, s.cipher.Decrypt)
}

// querier is the subset of pgxpool.Pool / pgx.Tx used to read the key.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func txLoadPersistedOIDCKey(ctx context.Context, q querier, decrypt func(string) (string, error)) (*rsa.PrivateKey, bool, error) {
	var enc string
	err := q.QueryRow(ctx,
		`SELECT private_key_pem_enc FROM oidc_keys ORDER BY created_at, kid LIMIT 1`).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	pem, err := decrypt(enc)
	if err != nil {
		return nil, false, err
	}
	key, err := oidc.ParsePrivateKeyPEM(pem)
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// oidcIssuer is the token issuer — the server's public origin. It MUST match the
// issuer in the discovery document and be reachable by AWS/GCP for JWKS fetch.
// Mirrors internal/api.externalURL so the mint path and the discovery endpoint
// agree without a cross-package dependency.
func oidcIssuer() string {
	if v := strings.TrimSpace(os.Getenv("EXTERNAL_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://localhost:8080"
}

// mintJobOIDCToken mints the per-job ID token injected into the job env. Never
// fatal to job acquisition: on any error it returns "" and the caller simply
// omits the token (a job that doesn't use keyless cloud auth is unaffected).
func (s *Store) mintJobOIDCToken(repo, ref, sha, environment string, pipelineID, jobID int64) string {
	if s.oidcSigner == nil {
		return ""
	}
	tok, err := s.oidcSigner.Mint(oidcIssuer(), oidc.Audience(), oidc.Claims{
		Repo:        repo,
		Ref:         ref,
		SHA:         sha,
		PipelineID:  pipelineID,
		JobID:       jobID,
		Environment: environment,
	})
	if err != nil {
		slog.Error("oidc: mint job token failed", "err", err, "job", jobID)
		return ""
	}
	return tok
}
