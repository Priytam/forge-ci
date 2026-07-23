package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SSOProvider is one provider's configuration. ClientSecret is write-only:
// list/read paths return HasSecret instead.
type SSOProvider struct {
	Provider      string `json:"provider"`
	Enabled       bool   `json:"enabled"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret,omitempty"`
	HasSecret     bool   `json:"has_secret"`
	Tenant        string `json:"tenant"`
	AllowedDomain string `json:"allowed_domain"`
}

// UpsertSSOProvider saves provider config; empty secret keeps the stored one.
func (s *Store) UpsertSSOProvider(ctx context.Context, p SSOProvider) error {
	if p.Tenant == "" {
		p.Tenant = "common"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sso_providers (provider, enabled, client_id, client_secret, tenant, allowed_domain)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (provider) DO UPDATE SET
		   enabled = EXCLUDED.enabled,
		   client_id = EXCLUDED.client_id,
		   client_secret = CASE WHEN EXCLUDED.client_secret = ''
		                        THEN sso_providers.client_secret
		                        ELSE EXCLUDED.client_secret END,
		   tenant = EXCLUDED.tenant,
		   allowed_domain = EXCLUDED.allowed_domain,
		   updated_at = now()`,
		p.Provider, p.Enabled, p.ClientID, p.ClientSecret, p.Tenant, p.AllowedDomain)
	return err
}

// ListSSOProviders returns all provider configs with secrets masked.
func (s *Store) ListSSOProviders(ctx context.Context) ([]SSOProvider, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT provider, enabled, client_id, client_secret <> '', tenant, allowed_domain
		 FROM sso_providers ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SSOProvider{}
	for rows.Next() {
		var p SSOProvider
		if err := rows.Scan(&p.Provider, &p.Enabled, &p.ClientID, &p.HasSecret,
			&p.Tenant, &p.AllowedDomain); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetEnabledSSOProvider returns full config (incl. secret) for the auth flow.
func (s *Store) GetEnabledSSOProvider(ctx context.Context, provider string) (*SSOProvider, error) {
	var p SSOProvider
	err := s.pool.QueryRow(ctx,
		`SELECT provider, enabled, client_id, client_secret, tenant, allowed_domain
		 FROM sso_providers WHERE provider=$1 AND enabled`, provider).
		Scan(&p.Provider, &p.Enabled, &p.ClientID, &p.ClientSecret, &p.Tenant, &p.AllowedDomain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.HasSecret = p.ClientSecret != ""
	return &p, nil
}

// AnySSOEnabled reports whether session enforcement should be active.
func (s *Store) AnySSOEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sso_providers WHERE enabled)`).Scan(&enabled)
	return enabled, err
}

// Session is an authenticated login.
type Session struct {
	Token    string    `json:"-"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	Provider string    `json:"provider"`
	Expires  time.Time `json:"expires_at"`
}

func (s *Store) CreateSession(ctx context.Context, email, name, provider string, ttl time.Duration) (*Session, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	sess := &Session{
		Token:    hex.EncodeToString(raw),
		Email:    email,
		Name:     name,
		Provider: provider,
		Expires:  time.Now().Add(ttl),
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token, email, name, provider, expires_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		sess.Token, email, name, provider, sess.Expires)
	return sess, err
}

func (s *Store) GetSession(ctx context.Context, token string) (*Session, error) {
	var sess Session
	sess.Token = token
	err := s.pool.QueryRow(ctx,
		`SELECT email, name, provider, expires_at FROM sessions
		 WHERE token=$1 AND expires_at > now()`, token).
		Scan(&sess.Email, &sess.Name, &sess.Provider, &sess.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sess, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token=$1`, token)
	return err
}
