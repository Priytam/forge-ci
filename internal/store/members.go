package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
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

// ---- include: templates ----

// UpsertRepoTemplate registers (or replaces) a named pipeline fragment for a
// repo. These fragments are what top-level include: [{template: name}] resolves
// against — Forge has no repo file tree, so reusable YAML lives here.
func (s *Store) UpsertRepoTemplate(ctx context.Context, repo, name, yml string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repo_templates (repo, name, yaml, updated_at)
		 VALUES ($1,$2,$3, now())
		 ON CONFLICT (repo, name) DO UPDATE SET yaml=EXCLUDED.yaml, updated_at=now()`,
		repo, name, yml)
	return err
}

// GetRepoTemplate returns one registered template's YAML. ErrNotFound when the
// repo has no template by that name.
func (s *Store) GetRepoTemplate(ctx context.Context, repo, name string) (string, error) {
	var yml string
	err := s.pool.QueryRow(ctx,
		`SELECT yaml FROM repo_templates WHERE repo=$1 AND name=$2`, repo, name).Scan(&yml)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return yml, err
}

// ListRepoTemplates returns the names of a repo's registered templates.
func (s *Store) ListRepoTemplates(ctx context.Context, repo string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name FROM repo_templates WHERE repo=$1 ORDER BY name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TemplateResolver builds a compiler.TemplateFunc bound to one repo, so the
// compiler can resolve include: templates during Compile. ErrNotFound is
// translated to (found=false) so the compiler can report a clear "not
// registered" error.
func (s *Store) TemplateResolver(ctx context.Context, repo string) compiler.TemplateFunc {
	return func(name string) (string, bool, error) {
		yml, err := s.GetRepoTemplate(ctx, repo, name)
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return yml, true, nil
	}
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
// It includes roles granted through the user's IdP groups.
func (s *Store) memberRole(ctx context.Context, repo, username string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM repo_members WHERE repo=$1 AND lower(username)=lower($2)`, repo, username).Scan(&role)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	g, err := s.groupRole(ctx, repo, username)
	if err != nil {
		return "", err
	}
	return higherRole(role, g), nil
}

// repoHasMembers reports whether any membership exists for the repo. Repos
// with no members run in bootstrap mode: anyone may approve (documented).
func (s *Store) repoHasMembers(ctx context.Context, repo string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM repo_members WHERE repo=$1)`, repo).Scan(&exists)
	if err != nil || exists {
		return exists, err
	}
	return s.repoHasGroupGrants(ctx, repo)
}

// resolveProtectedEnv returns the approval rule for (repo, env): the
// repo-specific row if present, else the global (repo=”) default, else nil.
// The job's environment is canonicalised the same way saving a rule is
// (registry alias or case variant → registered name), so a job stored under
// an old spelling never matches a weaker leftover rule.
func (s *Store) resolveProtectedEnv(ctx context.Context, repo, name string) (*proto.ProtectedEnvironment, error) {
	if policy, err := s.GetEnvPolicy(ctx); err == nil && len(policy.Environments) > 0 {
		name = policy.Canonical(name)
	}
	var pe proto.ProtectedEnvironment
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, name, required_approvals, approval_timeout_hours,
		        approver_roles, allow_self_approval, required_group, required_group_approvals
		 FROM protected_environments
		 WHERE lower(name)=lower($2) AND repo IN ('', $1)
		 ORDER BY repo DESC, (name = $2) DESC LIMIT 1`, repo, name).
		Scan(&pe.ID, &pe.Repo, &pe.Name, &pe.RequiredApprovals, &pe.ApprovalTimeoutHours,
			&pe.ApproverRoles, &pe.AllowSelfApproval, &pe.RequiredGroup, &pe.RequiredGroupApprovals)
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
		        approver_roles, allow_self_approval, required_group, required_group_approvals
		 FROM protected_environments WHERE repo IN ('', $1) ORDER BY repo, name`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.ProtectedEnvironment{}
	for rows.Next() {
		var pe proto.ProtectedEnvironment
		if err := rows.Scan(&pe.ID, &pe.Repo, &pe.Name, &pe.RequiredApprovals,
			&pe.ApprovalTimeoutHours, &pe.ApproverRoles, &pe.AllowSelfApproval,
			&pe.RequiredGroup, &pe.RequiredGroupApprovals); err != nil {
			return nil, err
		}
		out = append(out, pe)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// With a registry in use, a rule whose name is no registered environment
	// (nor an alias of one) can never match a job: say so.
	if policy, perr := s.GetEnvPolicy(ctx); perr == nil && len(policy.Environments) > 0 {
		for i := range out {
			out[i].Inactive = policy.Resolve(out[i].Name) == nil
		}
	}
	return out, nil
}

// FoldProtectedEnvRules merges approval rules saved under an alias or case
// variant of a registered environment into the canonical rule, keeping the
// stricter settings (more approvals, shorter timeout, self-approval off,
// the group requirement), so a job can never fall through to a weaker
// leftover. Runs at start and when the registry is saved. Returns how many
// rules were folded.
func (s *Store) FoldProtectedEnvRules(ctx context.Context) (int, error) {
	policy, err := s.GetEnvPolicy(ctx)
	if err != nil {
		return 0, err
	}
	return s.foldProtectedEnvRules(ctx, policy)
}

func (s *Store) foldProtectedEnvRules(ctx context.Context, policy *EnvPolicy) (int, error) {
	if policy == nil || len(policy.Environments) == 0 {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, name, required_approvals, approval_timeout_hours, approver_roles,
		        allow_self_approval, required_group, required_group_approvals
		 FROM protected_environments ORDER BY repo, id`)
	if err != nil {
		return 0, err
	}
	var all []proto.ProtectedEnvironment
	for rows.Next() {
		var pe proto.ProtectedEnvironment
		if err := rows.Scan(&pe.ID, &pe.Repo, &pe.Name, &pe.RequiredApprovals, &pe.ApprovalTimeoutHours,
			&pe.ApproverRoles, &pe.AllowSelfApproval, &pe.RequiredGroup, &pe.RequiredGroupApprovals); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, pe)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	byKey := map[string]*proto.ProtectedEnvironment{} // repo|canonical name → the rule already using the canonical name
	for i := range all {
		if all[i].Name == policy.Canonical(all[i].Name) {
			byKey[all[i].Repo+"|"+all[i].Name] = &all[i]
		}
	}
	folded := 0
	for i := range all {
		pe := all[i]
		canonical := policy.Canonical(pe.Name)
		if canonical == pe.Name {
			continue
		}
		target, exists := byKey[pe.Repo+"|"+canonical]
		if !exists {
			if _, err := s.pool.Exec(ctx, `UPDATE protected_environments SET name=$2 WHERE id=$1`, pe.ID, canonical); err != nil {
				return folded, err
			}
			all[i].Name = canonical
			byKey[pe.Repo+"|"+canonical] = &all[i]
			folded++
			continue
		}
		// Merge into the canonical rule: strictest of each setting.
		if pe.RequiredApprovals > target.RequiredApprovals {
			target.RequiredApprovals = pe.RequiredApprovals
		}
		if pe.ApprovalTimeoutHours > 0 && (target.ApprovalTimeoutHours == 0 || pe.ApprovalTimeoutHours < target.ApprovalTimeoutHours) {
			target.ApprovalTimeoutHours = pe.ApprovalTimeoutHours
		}
		target.AllowSelfApproval = target.AllowSelfApproval && pe.AllowSelfApproval
		if target.RequiredGroup == "" && pe.RequiredGroup != "" {
			target.RequiredGroup, target.RequiredGroupApprovals = pe.RequiredGroup, pe.RequiredGroupApprovals
		} else if pe.RequiredGroup == target.RequiredGroup && pe.RequiredGroupApprovals > target.RequiredGroupApprovals {
			target.RequiredGroupApprovals = pe.RequiredGroupApprovals
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE protected_environments SET required_approvals=$2, approval_timeout_hours=$3,
			        allow_self_approval=$4, required_group=$5, required_group_approvals=$6 WHERE id=$1`,
			target.ID, target.RequiredApprovals, target.ApprovalTimeoutHours, target.AllowSelfApproval,
			target.RequiredGroup, target.RequiredGroupApprovals); err != nil {
			return folded, err
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM protected_environments WHERE id=$1`, pe.ID); err != nil {
			return folded, err
		}
		folded++
	}
	return folded, nil
}

// UpsertProtectedEnv creates or updates a repo-scoped approval rule.
func (s *Store) UpsertProtectedEnv(ctx context.Context, pe proto.ProtectedEnvironment) (*proto.ProtectedEnvironment, error) {
	if pe.ApproverRoles == nil {
		pe.ApproverRoles = []string{"admin", "owner"}
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO protected_environments
		     (repo, name, required_approvals, approval_timeout_hours, approver_roles, allow_self_approval,
		      required_group, required_group_approvals)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (repo, name) DO UPDATE SET
		     required_approvals = EXCLUDED.required_approvals,
		     approval_timeout_hours = EXCLUDED.approval_timeout_hours,
		     approver_roles = EXCLUDED.approver_roles,
		     allow_self_approval = EXCLUDED.allow_self_approval,
		     required_group = EXCLUDED.required_group,
		     required_group_approvals = EXCLUDED.required_group_approvals
		 RETURNING id`,
		pe.Repo, pe.Name, pe.RequiredApprovals, pe.ApprovalTimeoutHours,
		pe.ApproverRoles, pe.AllowSelfApproval, pe.RequiredGroup, pe.RequiredGroupApprovals).Scan(&pe.ID)
	return &pe, err
}

// RepoAccess returns the caller's role on repo ("" when not a member) and
// whether the repo has any members at all. Usernames are matched
// case-insensitively because SSO identities are emails, and members are
// usually added by hand.
func (s *Store) RepoAccess(ctx context.Context, repo, username string) (role string, hasMembers bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT role FROM repo_members
		                  WHERE repo=$1 AND lower(username)=lower($2) LIMIT 1), ''),
		        EXISTS (SELECT 1 FROM repo_members WHERE repo=$1)`,
		repo, username).Scan(&role, &hasMembers)
	if err != nil {
		return "", false, err
	}
	// Roles granted through IdP group mappings count like direct membership.
	g, err := s.groupRole(ctx, repo, username)
	if err != nil {
		return "", false, err
	}
	role = higherRole(role, g)
	if !hasMembers {
		if hasMembers, err = s.repoHasGroupGrants(ctx, repo); err != nil {
			return "", false, err
		}
	}
	return role, hasMembers, nil
}

// JobRepo returns the repo of the pipeline a job belongs to.
func (s *Store) JobRepo(ctx context.Context, jobID int64) (string, error) {
	var repo string
	err := s.pool.QueryRow(ctx,
		`SELECT p.repo FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id WHERE j.id=$1`,
		jobID).Scan(&repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return repo, err
}

// PipelineRepo returns the repo a pipeline ran for.
func (s *Store) PipelineRepo(ctx context.Context, pipelineID int64) (string, error) {
	var repo string
	err := s.pool.QueryRow(ctx, `SELECT repo FROM pipelines WHERE id=$1`, pipelineID).Scan(&repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return repo, err
}

// DeleteProtectedEnv removes an approval rule. Deleting the global default
// production rule is remembered, so startup doesn't re-create it.
func (s *Store) DeleteProtectedEnv(ctx context.Context, id int64) (*proto.ProtectedEnvironment, error) {
	var pe proto.ProtectedEnvironment
	err := s.pool.QueryRow(ctx,
		`DELETE FROM protected_environments WHERE id=$1 RETURNING id, repo, name`, id).Scan(&pe.ID, &pe.Repo, &pe.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if pe.Repo == "" && pe.Name == "production" {
		_, err = s.pool.Exec(ctx,
			`INSERT INTO platform_settings (key, value) VALUES ('default_production_rule_removed', 'true')
			 ON CONFLICT (key) DO NOTHING`)
	}
	return &pe, err
}
