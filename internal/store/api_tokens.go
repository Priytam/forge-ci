package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// APITokenPrefix marks a personal API token, so the server can tell one apart
// from a runner token (plain hex) on the same Authorization header.
const APITokenPrefix = "fci_"

// APIToken is a personal API token. Token carries the raw secret only in the
// response to creation; lists expose TokenSuffix (last 4 chars) instead.
// ExpiresAt nil means the token never expires.
type APIToken struct {
	ID          int64      `json:"id"`
	Token       string     `json:"token,omitempty"`
	TokenSuffix string     `json:"token_suffix"`
	Owner       string     `json:"owner"`
	OwnerName   string     `json:"owner_name"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Revoked     bool       `json:"revoked"`
	// Status is derived: active | expired | revoked | disabled (by policy).
	Status string `json:"status"`
	// ServiceAccountID is set for a service account's token; nil for personal.
	ServiceAccountID *int64 `json:"service_account_id,omitempty"`
	// ServiceAdmin is the service account's admin flag (lookup only).
	ServiceAdmin bool `json:"-"`
}

// TokenPolicy is the admin-controlled rule for personal API tokens.
// MaxLifetimeDays 0 means no cap on expiring tokens.
type TokenPolicy struct {
	AllowNoExpiry   bool `json:"allow_no_expiry"`
	MaxLifetimeDays int  `json:"max_lifetime_days"`
}

// DefaultTokenPolicy applies until an admin saves one.
var DefaultTokenPolicy = TokenPolicy{AllowNoExpiry: true, MaxLifetimeDays: 0}

const tokenPolicyKey = "api_token_policy"

func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GetTokenPolicy returns the saved policy, or DefaultTokenPolicy.
func (s *Store) GetTokenPolicy(ctx context.Context) (TokenPolicy, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key=$1`, tokenPolicyKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultTokenPolicy, nil
	}
	if err != nil {
		return DefaultTokenPolicy, err
	}
	p := DefaultTokenPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return DefaultTokenPolicy, err
	}
	return p, nil
}

// SetTokenPolicy saves the policy.
func (s *Store) SetTokenPolicy(ctx context.Context, p TokenPolicy) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		tokenPolicyKey, raw)
	return err
}

// CreateAPIToken mints a token for owner. expiresAt nil = never expires; the
// caller has already checked that against the policy.
func (s *Store) CreateAPIToken(ctx context.Context, owner, ownerName, description string, expiresAt *time.Time) (*APIToken, error) {
	return s.createAPIToken(ctx, owner, ownerName, description, expiresAt, nil)
}

func (s *Store) createAPIToken(ctx context.Context, owner, ownerName, description string, expiresAt *time.Time, serviceAccountID *int64) (*APIToken, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	raw := APITokenPrefix + hex.EncodeToString(buf)
	t := APIToken{
		Token: raw, TokenSuffix: raw[len(raw)-4:], Owner: owner, OwnerName: ownerName,
		Description: description, ExpiresAt: expiresAt, Status: "active",
		ServiceAccountID: serviceAccountID,
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO api_tokens (token_hash, token_suffix, owner, owner_name, description, expires_at, service_account_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
		hashAPIToken(raw), t.TokenSuffix, owner, ownerName, description, expiresAt, serviceAccountID).
		Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListAPITokens returns owner's tokens, or every token when owner is "".
// Status is derived against policy so the list shows why a token stopped
// working.
func (s *Store) ListAPITokens(ctx context.Context, owner string, policy TokenPolicy) ([]APIToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, token_suffix, owner, owner_name, description, created_at,
		        expires_at, last_used_at, revoked, service_account_id
		 FROM api_tokens WHERE ($1 = '' OR lower(owner) = lower($1))
		 ORDER BY id DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.TokenSuffix, &t.Owner, &t.OwnerName, &t.Description,
			&t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.Revoked, &t.ServiceAccountID); err != nil {
			return nil, err
		}
		t.Status = tokenStatus(t, policy, now)
		out = append(out, t)
	}
	return out, rows.Err()
}

func tokenStatus(t APIToken, p TokenPolicy, now time.Time) string {
	switch {
	case t.Revoked:
		return "revoked"
	case t.ExpiresAt != nil && !t.ExpiresAt.After(now):
		return "expired"
	case t.ExpiresAt == nil && !p.AllowNoExpiry:
		return "disabled"
	case p.MaxLifetimeDays > 0 && !t.CreatedAt.AddDate(0, 0, p.MaxLifetimeDays).After(now):
		return "disabled"
	}
	return "active"
}

// LookupAPIToken resolves a raw token to its owner if it is usable right now
// under policy: not revoked, not expired, and allowed by the current policy.
// It stamps last_used_at at most once a minute to keep reads cheap. Returns
// ErrNotFound for any unusable token.
func (s *Store) LookupAPIToken(ctx context.Context, raw string, policy TokenPolicy) (*APIToken, error) {
	if !strings.HasPrefix(raw, APITokenPrefix) {
		return nil, ErrNotFound
	}
	var t APIToken
	err := s.pool.QueryRow(ctx,
		`SELECT t.id, t.owner, t.owner_name, t.created_at, t.expires_at, t.service_account_id,
		        COALESCE(sa.is_admin, FALSE)
		 FROM api_tokens t LEFT JOIN service_accounts sa ON sa.id = t.service_account_id
		 WHERE t.token_hash=$1 AND NOT t.revoked
		   AND (t.service_account_id IS NULL OR NOT sa.disabled)
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   AND ($2 OR t.expires_at IS NOT NULL)
		   AND ($3 = 0 OR t.created_at + make_interval(days => $3) > now())`,
		hashAPIToken(raw), policy.AllowNoExpiry, policy.MaxLifetimeDays).
		Scan(&t.ID, &t.Owner, &t.OwnerName, &t.CreatedAt, &t.ExpiresAt, &t.ServiceAccountID, &t.ServiceAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.pool.Exec(ctx,
		`UPDATE api_tokens SET last_used_at = now()
		 WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, t.ID)
	return &t, nil
}

// RevokeAPIToken revokes token id. When owner is non-empty only that owner's
// token matches (an admin passes "" to revoke anyone's).
func (s *Store) RevokeAPIToken(ctx context.Context, id int64, owner string) (*APIToken, error) {
	var t APIToken
	err := s.pool.QueryRow(ctx,
		`UPDATE api_tokens SET revoked = TRUE
		 WHERE id=$1 AND ($2 = '' OR lower(owner) = lower($2))
		 RETURNING id, token_suffix, owner, description`, id, owner).
		Scan(&t.ID, &t.TokenSuffix, &t.Owner, &t.Description)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}
