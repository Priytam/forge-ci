package store

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApproverGroup is a named list of people who can be required on an
// approval rule ("at least 1 approval from release-managers").
type ApproverGroup struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Members     []string  `json:"members"`
	UpdatedAt   time.Time `json:"updated_at"`
}

var groupNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ErrGroupInUse: a group referenced by an approval rule can't be deleted.
var ErrGroupInUse = errors.New("group is required by an approval rule")

// NormalizeGroup lowercases and de-duplicates members and validates the name.
func NormalizeGroup(g *ApproverGroup) error {
	g.Name = strings.ToLower(strings.TrimSpace(g.Name))
	if !groupNameRe.MatchString(g.Name) {
		return errors.New("group name must be lowercase letters, digits, . _ - (max 63)")
	}
	seen := map[string]bool{}
	members := []string{}
	for _, m := range g.Members {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" && !seen[m] {
			seen[m] = true
			members = append(members, m)
		}
	}
	sort.Strings(members)
	g.Members = members
	return nil
}

func (s *Store) ListApproverGroups(ctx context.Context) ([]ApproverGroup, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, description, members, updated_at FROM approver_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApproverGroup{}
	for rows.Next() {
		var g ApproverGroup
		if err := rows.Scan(&g.Name, &g.Description, &g.Members, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetApproverGroup returns one group, or ErrNotFound.
func (s *Store) GetApproverGroup(ctx context.Context, name string) (*ApproverGroup, error) {
	var g ApproverGroup
	err := s.pool.QueryRow(ctx,
		`SELECT name, description, members, updated_at FROM approver_groups WHERE name = $1`,
		strings.ToLower(name)).Scan(&g.Name, &g.Description, &g.Members, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &g, err
}

// UpsertApproverGroup creates or replaces a group.
func (s *Store) UpsertApproverGroup(ctx context.Context, g ApproverGroup) (*ApproverGroup, error) {
	if err := NormalizeGroup(&g); err != nil {
		return nil, err
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO approver_groups (name, description, members) VALUES ($1,$2,$3)
		 ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description,
		     members = EXCLUDED.members, updated_at = now()
		 RETURNING updated_at`, g.Name, g.Description, g.Members).Scan(&g.UpdatedAt)
	return &g, err
}

// DeleteApproverGroup removes a group that no approval rule requires.
func (s *Store) DeleteApproverGroup(ctx context.Context, name string) error {
	var used bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM protected_environments WHERE required_group = $1)`,
		strings.ToLower(name)).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrGroupInUse
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM approver_groups WHERE name = $1`, strings.ToLower(name))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// groupMembers returns the lowercased members of a group (empty when the
// group is unset or missing).
func (s *Store) groupMembers(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) (map[string]bool, error) {
	out := map[string]bool{}
	if name == "" {
		return out, nil
	}
	var members []string
	err := q.QueryRow(ctx, `SELECT members FROM approver_groups WHERE name = $1`, name).Scan(&members)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		out[strings.ToLower(m)] = true
	}
	return out, nil
}

// ApproverGroupMembers returns every group's members by group name (emails
// lowercased), for the code owners rule.
func (s *Store) ApproverGroupMembers(ctx context.Context) (map[string]map[string]bool, error) {
	gs, err := s.ListApproverGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(gs))
	for _, g := range gs {
		m := make(map[string]bool, len(g.Members))
		for _, e := range g.Members {
			m[strings.ToLower(e)] = true
		}
		out[strings.ToLower(g.Name)] = m
	}
	return out, nil
}
