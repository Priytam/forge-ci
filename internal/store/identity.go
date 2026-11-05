package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Identity beyond direct repo membership: the groups an identity provider
// reports for a user, and the mappings that turn those groups into Forge
// roles. Effective role on a repo = the highest of the user's direct
// membership and every mapping that matches one of their groups and the repo.

func settingsJSON(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

var roleRank = map[string]int{"developer": 1, "owner": 2, "admin": 3}

// higherRole returns the stronger of two repo roles ("" = none).
func higherRole(a, b string) string {
	if roleRank[b] > roleRank[a] {
		return b
	}
	return a
}

// RecordUserGroups stores the groups a provider reported at sign-in. An
// empty list is stored too: leaving a group in the IdP must remove its roles.
func (s *Store) RecordUserGroups(ctx context.Context, email, provider string, groups []string) error {
	if groups == nil {
		groups = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_groups (email, provider, groups) VALUES (lower($1), $2, $3)
		 ON CONFLICT (email) DO UPDATE SET provider = EXCLUDED.provider, groups = EXCLUDED.groups, updated_at = now()`,
		email, provider, groups)
	return err
}

// UserGroups returns a user's last-reported groups and their provider.
func (s *Store) UserGroups(ctx context.Context, email string) (provider string, groups []string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT provider, groups FROM user_groups WHERE email = lower($1)`, email).
		Scan(&provider, &groups)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return provider, groups, err
}

// GroupMapping maps an IdP group to platform admin or a role on repos.
type GroupMapping struct {
	ID            int64     `json:"id"`
	Provider      string    `json:"provider"` // "" = any provider
	Group         string    `json:"group"`
	PlatformAdmin bool      `json:"platform_admin"`
	RepoPattern   string    `json:"repo_pattern"` // "*", "acme/*", "acme/shop"
	Role          string    `json:"role"`         // developer | owner | admin
	CreatedAt     time.Time `json:"created_at"`
}

// ErrDuplicateMapping: the same group already has this grant.
var ErrDuplicateMapping = errors.New("this group already has that mapping")

func (m *GroupMapping) validate() error {
	m.Group = strings.TrimSpace(m.Group)
	m.RepoPattern = strings.TrimSpace(m.RepoPattern)
	if m.Group == "" || len(m.Group) > 512 {
		return errors.New("group is required")
	}
	if m.PlatformAdmin {
		m.RepoPattern, m.Role = "", ""
		return nil
	}
	if m.RepoPattern == "" {
		return errors.New(`repo_pattern is required ("*" for every repo, "acme/*" for a namespace)`)
	}
	if _, err := path.Match(m.RepoPattern, "x"); err != nil {
		return fmt.Errorf("invalid repo_pattern: %w", err)
	}
	if roleRank[m.Role] == 0 {
		return errors.New("role must be developer, owner or admin")
	}
	return nil
}

func (s *Store) ListGroupMappings(ctx context.Context) ([]GroupMapping, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, provider, idp_group, platform_admin, repo_pattern, role, created_at
		 FROM idp_group_mappings ORDER BY idp_group, platform_admin DESC, repo_pattern`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupMapping, error) {
		var m GroupMapping
		err := r.Scan(&m.ID, &m.Provider, &m.Group, &m.PlatformAdmin, &m.RepoPattern, &m.Role, &m.CreatedAt)
		return m, err
	})
}

func (s *Store) CreateGroupMapping(ctx context.Context, m GroupMapping) (*GroupMapping, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO idp_group_mappings (provider, idp_group, platform_admin, repo_pattern, role)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		m.Provider, m.Group, m.PlatformAdmin, m.RepoPattern, m.Role).Scan(&m.ID, &m.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicateMapping
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) DeleteGroupMapping(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idp_group_mappings WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// mappingsFor returns the mappings that apply to a user's current groups.
func (s *Store) mappingsFor(ctx context.Context, email string) ([]GroupMapping, error) {
	provider, groups, err := s.UserGroups(ctx, email)
	if err != nil || len(groups) == 0 {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, provider, idp_group, platform_admin, repo_pattern, role, created_at
		 FROM idp_group_mappings
		 WHERE (provider = '' OR provider = $1) AND lower(idp_group) = ANY($2)`,
		provider, lowerAll(groups))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupMapping, error) {
		var m GroupMapping
		err := r.Scan(&m.ID, &m.Provider, &m.Group, &m.PlatformAdmin, &m.RepoPattern, &m.Role, &m.CreatedAt)
		return m, err
	})
}

// IsGroupAdmin reports whether one of the user's IdP groups maps to
// platform admin.
func (s *Store) IsGroupAdmin(ctx context.Context, email string) (bool, error) {
	ms, err := s.mappingsFor(ctx, email)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.PlatformAdmin {
			return true, nil
		}
	}
	return false, nil
}

// repoPatternMatches matches a repo against a mapping pattern: "*" is every
// repo; otherwise path.Match, where "acme/*" is one namespace level and
// "acme/**" any depth below acme/.
func repoPatternMatches(pattern, repo string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(repo, strings.TrimSuffix(pattern, "**"))
	}
	ok, _ := path.Match(pattern, repo)
	return ok
}

// groupRole is the highest role the user's groups grant on repo ("" = none).
func (s *Store) groupRole(ctx context.Context, repo, email string) (string, error) {
	ms, err := s.mappingsFor(ctx, email)
	if err != nil {
		return "", err
	}
	role := ""
	for _, m := range ms {
		if !m.PlatformAdmin && repoPatternMatches(m.RepoPattern, repo) {
			role = higherRole(role, m.Role)
		}
	}
	return role, nil
}

// repoHasGroupGrants reports whether any mapping grants a role on repo — a
// repo governed by group mappings is no longer in open bootstrap mode.
func (s *Store) repoHasGroupGrants(ctx context.Context, repo string) (bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT repo_pattern FROM idp_group_mappings WHERE NOT platform_admin`)
	if err != nil {
		return false, err
	}
	pats, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, p := range pats {
		if repoPatternMatches(p, repo) {
			return true, nil
		}
	}
	return false, nil
}

// ---- session lifetime ----

const sessionTTLKey = "session_ttl_hours"

// Session lifetime bounds: at least an hour, at most 30 days.
const (
	MinSessionTTL     = time.Hour
	MaxSessionTTL     = 30 * 24 * time.Hour
	DefaultSessionTTL = 12 * time.Hour
)

// SessionTTL is how long a new sign-in lasts: the admin setting, else
// SESSION_TTL (a Go duration), else 12h.
func (s *Store) SessionTTL(ctx context.Context, envDefault time.Duration) time.Duration {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key=$1`, sessionTTLKey).Scan(&raw); err == nil {
		var v struct {
			Hours int `json:"hours"`
		}
		if json.Unmarshal(raw, &v) == nil && v.Hours > 0 {
			return clampTTL(time.Duration(v.Hours) * time.Hour)
		}
	}
	if envDefault > 0 {
		return clampTTL(envDefault)
	}
	return DefaultSessionTTL
}

func clampTTL(d time.Duration) time.Duration {
	if d < MinSessionTTL {
		return MinSessionTTL
	}
	if d > MaxSessionTTL {
		return MaxSessionTTL
	}
	return d
}

// SetSessionTTLHours saves the admin setting (0 clears it, back to the default).
func (s *Store) SetSessionTTLHours(ctx context.Context, hours int) error {
	if hours == 0 {
		_, err := s.pool.Exec(ctx, `DELETE FROM platform_settings WHERE key=$1`, sessionTTLKey)
		return err
	}
	if time.Duration(hours)*time.Hour < MinSessionTTL || time.Duration(hours)*time.Hour > MaxSessionTTL {
		return errors.New("session lifetime must be between 1 and " + strconv.Itoa(int(MaxSessionTTL.Hours())) + " hours")
	}
	raw, _ := json.Marshal(map[string]int{"hours": hours})
	_, err := s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, sessionTTLKey, raw)
	return err
}

// usersWithGroupRole lists users (lower-cased emails) whose last-reported IdP
// groups grant one of roles on repo through a mapping.
func (s *Store) usersWithGroupRole(ctx context.Context, repo string, roles []string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ug.email, m.repo_pattern
		 FROM user_groups ug
		 JOIN idp_group_mappings m
		   ON NOT m.platform_admin AND m.role = ANY($1)
		  AND (m.provider = '' OR m.provider = ug.provider)
		  AND lower(m.idp_group) = ANY(SELECT lower(g) FROM unnest(ug.groups) g)`, roles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var email, pattern string
		if err := rows.Scan(&email, &pattern); err != nil {
			return nil, err
		}
		if !seen[email] && repoPatternMatches(pattern, repo) {
			seen[email] = true
			out = append(out, email)
		}
	}
	return out, rows.Err()
}
