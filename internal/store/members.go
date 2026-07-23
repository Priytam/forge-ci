package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

var ErrDuplicateMember = errors.New("user is already a member of this repo")

// SetRepoConfig saves the pipeline YAML as a NEW immutable version and moves
// the current pointer. Returns the new version number. History is append-only.
func (s *Store) SetRepoConfig(ctx context.Context, repo, config, author, message string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var version int
	err = tx.QueryRow(ctx,
		`INSERT INTO repo_config_versions (repo, version, config_yaml, author, message)
		 VALUES ($1, COALESCE((SELECT max(version) FROM repo_config_versions WHERE repo=$1), 0) + 1, $2, $3, $4)
		 RETURNING version`,
		repo, config, author, message).Scan(&version)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO repo_configs (repo, config_yaml) VALUES ($1,$2)
		 ON CONFLICT (repo) DO UPDATE SET config_yaml = EXCLUDED.config_yaml, updated_at = now()`,
		repo, config); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// ConfigVersion is one row of a repo's config history (YAML omitted in lists).
type ConfigVersion struct {
	Version   int       `json:"version"`
	Author    string    `json:"author"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) ListConfigVersions(ctx context.Context, repo string) ([]ConfigVersion, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT version, author, message, created_at
		 FROM repo_config_versions WHERE repo=$1 ORDER BY version DESC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigVersion{}
	for rows.Next() {
		var v ConfigVersion
		if err := rows.Scan(&v.Version, &v.Author, &v.Message, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetConfigVersion returns one historical version's YAML.
func (s *Store) GetConfigVersion(ctx context.Context, repo string, version int) (string, error) {
	var config string
	err := s.pool.QueryRow(ctx,
		`SELECT config_yaml FROM repo_config_versions WHERE repo=$1 AND version=$2`,
		repo, version).Scan(&config)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return config, err
}

// CurrentConfigVersion returns the latest version number (0 = none).
func (s *Store) CurrentConfigVersion(ctx context.Context, repo string) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(max(version), 0) FROM repo_config_versions WHERE repo=$1`, repo).Scan(&v)
	return v, err
}

// SetRepoDefaultTags stores the repo's runner-group selection.
func (s *Store) SetRepoDefaultTags(ctx context.Context, repo string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repo_settings (repo, default_runner_tags) VALUES ($1,$2)
		 ON CONFLICT (repo) DO UPDATE
		   SET default_runner_tags = EXCLUDED.default_runner_tags, updated_at = now()`,
		repo, tags)
	return err
}

// GetRepoDefaultTags returns the repo's default runner tags ([] if unset).
func (s *Store) GetRepoDefaultTags(ctx context.Context, repo string) ([]string, error) {
	var tags []string
	err := s.pool.QueryRow(ctx,
		`SELECT default_runner_tags FROM repo_settings WHERE repo=$1`, repo).Scan(&tags)
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{}, nil
	}
	return tags, err
}

func (s *Store) GetRepoConfig(ctx context.Context, repo string) (string, error) {
	var config string
	err := s.pool.QueryRow(ctx,
		`SELECT config_yaml FROM repo_configs WHERE repo=$1`, repo).Scan(&config)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return config, err
}

func (s *Store) ListMembers(ctx context.Context, repo string) ([]proto.Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, username, role FROM repo_members WHERE repo=$1 ORDER BY username`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Member{}
	for rows.Next() {
		var m proto.Member
		if err := rows.Scan(&m.ID, &m.Repo, &m.Username, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) AddMember(ctx context.Context, repo, username, role string) (*proto.Member, error) {
	var m proto.Member
	err := s.pool.QueryRow(ctx,
		`INSERT INTO repo_members (repo, username, role) VALUES ($1,$2,$3)
		 ON CONFLICT (repo, username) DO UPDATE SET role = EXCLUDED.role
		 RETURNING id, repo, username, role`,
		repo, username, role).Scan(&m.ID, &m.Repo, &m.Username, &m.Role)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicateMember
	}
	return &m, err
}

func (s *Store) RemoveMember(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM repo_members WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// memberRole returns the user's role on a repo, or "" if not a member.
func (s *Store) memberRole(ctx context.Context, repo, username string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM repo_members WHERE repo=$1 AND username=$2`, repo, username).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// repoHasMembers reports whether any membership exists for the repo. Repos
// with no members run in bootstrap mode: anyone may approve (documented).
func (s *Store) repoHasMembers(ctx context.Context, repo string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM repo_members WHERE repo=$1)`, repo).Scan(&exists)
	return exists, err
}

// resolveProtectedEnv returns the approval rule for (repo, env): the
// repo-specific row if present, else the global (repo='') default, else nil.
func (s *Store) resolveProtectedEnv(ctx context.Context, repo, name string) (*proto.ProtectedEnvironment, error) {
	var pe proto.ProtectedEnvironment
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, name, required_approvals, approval_timeout_hours,
		        approver_roles, allow_self_approval
		 FROM protected_environments
		 WHERE name=$2 AND repo IN ('', $1)
		 ORDER BY repo DESC LIMIT 1`, repo, name).
		Scan(&pe.ID, &pe.Repo, &pe.Name, &pe.RequiredApprovals, &pe.ApprovalTimeoutHours,
			&pe.ApproverRoles, &pe.AllowSelfApproval)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pe, nil
}

func (s *Store) ListProtectedEnvs(ctx context.Context, repo string) ([]proto.ProtectedEnvironment, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, name, required_approvals, approval_timeout_hours,
		        approver_roles, allow_self_approval
		 FROM protected_environments WHERE repo IN ('', $1) ORDER BY repo, name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.ProtectedEnvironment{}
	for rows.Next() {
		var pe proto.ProtectedEnvironment
		if err := rows.Scan(&pe.ID, &pe.Repo, &pe.Name, &pe.RequiredApprovals,
			&pe.ApprovalTimeoutHours, &pe.ApproverRoles, &pe.AllowSelfApproval); err != nil {
			return nil, err
		}
		out = append(out, pe)
	}
	return out, rows.Err()
}

// UpsertProtectedEnv creates or updates a repo-scoped approval rule.
func (s *Store) UpsertProtectedEnv(ctx context.Context, pe proto.ProtectedEnvironment) (*proto.ProtectedEnvironment, error) {
	if pe.ApproverRoles == nil {
		pe.ApproverRoles = []string{"admin", "owner"}
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO protected_environments
		     (repo, name, required_approvals, approval_timeout_hours, approver_roles, allow_self_approval)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (repo, name) DO UPDATE SET
		     required_approvals = EXCLUDED.required_approvals,
		     approval_timeout_hours = EXCLUDED.approval_timeout_hours,
		     approver_roles = EXCLUDED.approver_roles,
		     allow_self_approval = EXCLUDED.allow_self_approval
		 RETURNING id`,
		pe.Repo, pe.Name, pe.RequiredApprovals, pe.ApprovalTimeoutHours,
		pe.ApproverRoles, pe.AllowSelfApproval).Scan(&pe.ID)
	return &pe, err
}
