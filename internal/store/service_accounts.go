package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ServiceAccountPrefix starts every service account identity ("svc:deployer"),
// the string used for repo membership, audit actors and triggered_by.
const ServiceAccountPrefix = "svc:"

// ServiceAccount is a non-human identity for automation, owned by the org.
type ServiceAccount struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Identity     string    `json:"identity"`
	Description  string    `json:"description"`
	IsAdmin      bool      `json:"is_admin"`
	Disabled     bool      `json:"disabled"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	ActiveTokens int       `json:"active_tokens"`
}

var (
	serviceNameRe         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)
	ErrDuplicateService   = errors.New("a service account with this name already exists")
	ErrInvalidServiceName = errors.New("name must be 2–41 lowercase letters, digits or -")
)

func scanService(row pgx.Row) (*ServiceAccount, error) {
	var a ServiceAccount
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &a.IsAdmin, &a.Disabled, &a.CreatedBy,
		&a.CreatedAt, &a.ActiveTokens); err != nil {
		return nil, err
	}
	a.Identity = ServiceAccountPrefix + a.Name
	return &a, nil
}

const serviceCols = `sa.id, sa.name, sa.description, sa.is_admin, sa.disabled, sa.created_by, sa.created_at,
	(SELECT count(*) FROM api_tokens t WHERE t.service_account_id = sa.id AND NOT t.revoked
	   AND (t.expires_at IS NULL OR t.expires_at > now()))`

func (s *Store) ListServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceCols+` FROM service_accounts sa ORDER BY sa.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceAccount{}
	for rows.Next() {
		a, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) GetServiceAccount(ctx context.Context, id int64) (*ServiceAccount, error) {
	a, err := scanService(s.pool.QueryRow(ctx, `SELECT `+serviceCols+` FROM service_accounts sa WHERE sa.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *Store) CreateServiceAccount(ctx context.Context, name, description string, isAdmin bool, createdBy string) (*ServiceAccount, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !serviceNameRe.MatchString(name) {
		return nil, ErrInvalidServiceName
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO service_accounts (name, description, is_admin, created_by) VALUES ($1,$2,$3,$4) RETURNING id`,
		name, strings.TrimSpace(description), isAdmin, createdBy).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicateService
	}
	if err != nil {
		return nil, err
	}
	return s.GetServiceAccount(ctx, id)
}

// UpdateServiceAccount changes the description, admin flag or disabled state.
// Disabling stops every token of the account immediately (see LookupAPIToken).
func (s *Store) UpdateServiceAccount(ctx context.Context, id int64, description *string, isAdmin, disabled *bool) (*ServiceAccount, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE service_accounts SET
		     description = COALESCE($2, description),
		     is_admin = COALESCE($3, is_admin),
		     disabled = COALESCE($4, disabled)
		 WHERE id = $1`, id, description, isAdmin, disabled)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.GetServiceAccount(ctx, id)
}

// DeleteServiceAccount removes the account and (by cascade) its tokens.
func (s *Store) DeleteServiceAccount(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM service_accounts WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateServiceToken mints a token for a service account.
func (s *Store) CreateServiceToken(ctx context.Context, a *ServiceAccount, description string, expiresAt *time.Time) (*APIToken, error) {
	id := a.ID
	return s.createAPIToken(ctx, a.Identity, a.Name, description, expiresAt, &id)
}

// ListServiceTokens returns a service account's tokens (secrets masked).
func (s *Store) ListServiceTokens(ctx context.Context, a *ServiceAccount, policy TokenPolicy) ([]APIToken, error) {
	return s.ListAPITokens(ctx, a.Identity, policy)
}
