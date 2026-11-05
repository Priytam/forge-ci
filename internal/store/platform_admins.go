package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PlatformAdmin is an admin granted from the Admin UI. ADMIN_EMAILS and IdP
// group mappings grant admin too; those live in config and idp_group_mappings.
type PlatformAdmin struct {
	Email     string    `json:"email"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrAlreadyAdmin: the email is already in platform_admins.
var ErrAlreadyAdmin = errors.New("already a platform admin")

// ListPlatformAdmins returns the UI-granted admins, by email.
func (s *Store) ListPlatformAdmins(ctx context.Context) ([]PlatformAdmin, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT email, added_by, created_at FROM platform_admins ORDER BY email`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PlatformAdmin, error) {
		var a PlatformAdmin
		err := r.Scan(&a.Email, &a.AddedBy, &a.CreatedAt)
		return a, err
	})
}

// AddPlatformAdmin grants platform admin to email (case-insensitive).
func (s *Store) AddPlatformAdmin(ctx context.Context, email, addedBy string) (*PlatformAdmin, error) {
	var a PlatformAdmin
	err := s.pool.QueryRow(ctx,
		`INSERT INTO platform_admins (email, added_by) VALUES (lower($1), $2)
		 ON CONFLICT (email) DO NOTHING
		 RETURNING email, added_by, created_at`,
		strings.TrimSpace(email), addedBy).Scan(&a.Email, &a.AddedBy, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlreadyAdmin
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// RemovePlatformAdmin revokes a UI-granted admin. ErrNotFound when the email
// isn't in the table (it may still be an admin through ADMIN_EMAILS or a group).
func (s *Store) RemovePlatformAdmin(ctx context.Context, email string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM platform_admins WHERE email = lower($1)`, strings.TrimSpace(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsPlatformAdmin reports whether email was granted admin from the UI.
func (s *Store) IsPlatformAdmin(ctx context.Context, email string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM platform_admins WHERE email = lower($1))`,
		strings.TrimSpace(email)).Scan(&ok)
	return ok, err
}

// HasGroupAdminMapping reports whether any IdP group maps to platform admin.
func (s *Store) HasGroupAdminMapping(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM idp_group_mappings WHERE platform_admin)`).Scan(&ok)
	return ok, err
}
