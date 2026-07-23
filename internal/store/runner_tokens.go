package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunnerToken is a runner authentication token. Token carries the raw secret
// only in the response to creation; list responses leave it empty and expose
// TokenSuffix (last 4 chars) instead.
type RunnerToken struct {
	ID          int64      `json:"id"`
	Token       string     `json:"token,omitempty"`
	TokenSuffix string     `json:"token_suffix"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Revoked     bool       `json:"revoked"`
}

// CreateRunnerToken generates a new random token and stores it. The returned
// value carries the raw Token — the only time it is ever exposed.
func (s *Store) CreateRunnerToken(ctx context.Context, description string) (*RunnerToken, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tok := hex.EncodeToString(raw)
	var rt RunnerToken
	err := s.pool.QueryRow(ctx,
		`INSERT INTO runner_tokens (token, description) VALUES ($1,$2)
		 RETURNING id, created_at`, tok, description).Scan(&rt.ID, &rt.CreatedAt)
	if err != nil {
		return nil, err
	}
	rt.Token = tok
	rt.TokenSuffix = tok[len(tok)-4:]
	rt.Description = description
	return &rt, nil
}

// ListRunnerTokens returns all tokens with the raw secret masked.
func (s *Store) ListRunnerTokens(ctx context.Context) ([]RunnerToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, right(token, 4), description, created_at, last_used_at, revoked
		 FROM runner_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunnerToken{}
	for rows.Next() {
		var rt RunnerToken
		if err := rows.Scan(&rt.ID, &rt.TokenSuffix, &rt.Description,
			&rt.CreatedAt, &rt.LastUsedAt, &rt.Revoked); err != nil {
			return nil, err
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}

// ValidateRunnerToken reports whether token exists and is not revoked, and
// stamps last_used_at when it is valid.
func (s *Store) ValidateRunnerToken(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runner_tokens SET last_used_at = now() WHERE token=$1 AND NOT revoked`, token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RevokeRunnerToken revokes a token addressed by either its numeric id or its
// raw token string.
func (s *Store) RevokeRunnerToken(ctx context.Context, idOrToken string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runner_tokens SET revoked=TRUE
		 WHERE token=$1 OR id::text=$1`, idOrToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountRunnerTokens returns the number of tokens (revoked or not). Used at
// startup to decide whether to auto-generate a bootstrap token.
func (s *Store) CountRunnerTokens(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runner_tokens`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
