package store

import (
	"context"
	"strings"
	"time"
)

// Person is one row of Admin → People: someone who has signed in, or who is
// a platform admin without having signed in yet.
type Person struct {
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	Provider     string     `json:"provider"`
	LastSignInAt *time.Time `json:"last_sign_in_at"` // nil: never signed in
	// AdminSource is why the person is a platform admin: "env" (ADMIN_EMAILS),
	// "ui" (granted on the People page), "group" (an IdP group mapped to
	// platform admin), or "" when they aren't one. Earlier sources win.
	AdminSource string     `json:"admin_source"`
	AdminGroups []string   `json:"admin_groups"` // IdP groups that make them admin
	RepoRoles   []RepoRole `json:"repo_roles"`   // direct repo memberships
}

// RepoRole is a person's membership role on one repo.
type RepoRole struct {
	Repo string `json:"repo"`
	Role string `json:"role"`
}

// PeopleQuery selects a page of people. Search matches email or name
// (case-insensitive substring); AdminsOnly keeps platform admins. Limit 0
// returns every match.
type PeopleQuery struct {
	Search     string
	AdminsOnly bool
	Limit      int
	Offset     int
}

// RecordSignIn adds or refreshes a person in the directory. Called on every
// sign-in; an empty name keeps the one already on file.
func (s *Store) RecordSignIn(ctx context.Context, email, name, provider string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (email, name, provider) VALUES (lower($1), $2, $3)
		 ON CONFLICT (email) DO UPDATE SET
		     name = COALESCE(NULLIF(EXCLUDED.name, ''), users.name),
		     provider = EXCLUDED.provider,
		     last_sign_in_at = now()`,
		strings.TrimSpace(email), strings.TrimSpace(name), provider)
	return err
}

// ListPeople returns one page of people, most recently signed in first
// (people who never signed in last), plus the number of matches. envAdmins
// are the ADMIN_EMAILS addresses, lower-cased.
func (s *Store) ListPeople(ctx context.Context, q PeopleQuery, envAdmins []string) ([]Person, int, error) {
	if envAdmins == nil {
		envAdmins = []string{}
	}
	pattern := ""
	if q.Search = strings.TrimSpace(q.Search); q.Search != "" {
		pattern = "%" + likeEscaper.Replace(q.Search) + "%"
	}
	// Everyone who signed in, plus admins who haven't yet.
	const rows = `WITH people AS (
		    SELECT email FROM users
		    UNION SELECT email FROM platform_admins
		    UNION SELECT unnest($2::text[])
		), r AS (
		    SELECT p.email, COALESCE(u.name, '') AS name, COALESCE(u.provider, '') AS provider,
		           u.last_sign_in_at,
		           p.email = ANY($2::text[]) AS env_admin,
		           pa.email IS NOT NULL AS ui_admin,
		           ARRAY(SELECT DISTINCT m.idp_group
		                 FROM user_groups g JOIN idp_group_mappings m
		                   ON m.platform_admin AND (m.provider = '' OR m.provider = g.provider)
		                  AND lower(m.idp_group) = ANY (SELECT lower(x) FROM unnest(g.groups) x)
		                 WHERE g.email = p.email ORDER BY 1) AS admin_groups
		    FROM people p
		    LEFT JOIN users u ON u.email = p.email
		    LEFT JOIN platform_admins pa ON pa.email = p.email
		), hits AS (
		    SELECT * FROM r
		    WHERE ($1 = '' OR r.email ILIKE $1 OR r.name ILIKE $1)
		      AND (NOT $3 OR r.env_admin OR r.ui_admin OR cardinality(r.admin_groups) > 0)
		)`
	var total int
	if err := s.pool.QueryRow(ctx, rows+` SELECT count(*) FROM hits`,
		pattern, envAdmins, q.AdminsOnly).Scan(&total); err != nil {
		return nil, 0, err
	}
	var limit any // NULL: no limit
	if q.Limit > 0 {
		limit = q.Limit
	}
	res, err := s.pool.Query(ctx, rows+`
		SELECT h.email, h.name, h.provider, h.last_sign_in_at, h.env_admin, h.ui_admin, h.admin_groups,
		       ARRAY(SELECT rm.repo || '|' || rm.role FROM repo_members rm
		             WHERE lower(rm.username) = h.email ORDER BY rm.repo)
		FROM hits h
		ORDER BY h.last_sign_in_at DESC NULLS LAST, h.email
		LIMIT $4 OFFSET $5`,
		pattern, envAdmins, q.AdminsOnly, limit, q.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer res.Close()
	out := []Person{}
	for res.Next() {
		var p Person
		var envAdmin, uiAdmin bool
		var roles []string
		if err := res.Scan(&p.Email, &p.Name, &p.Provider, &p.LastSignInAt, &envAdmin, &uiAdmin,
			&p.AdminGroups, &roles); err != nil {
			return nil, 0, err
		}
		switch {
		case envAdmin:
			p.AdminSource = "env"
		case uiAdmin:
			p.AdminSource = "ui"
		case len(p.AdminGroups) > 0:
			p.AdminSource = "group"
		}
		p.RepoRoles = make([]RepoRole, 0, len(roles))
		for _, r := range roles {
			repo, role, _ := strings.Cut(r, "|")
			p.RepoRoles = append(p.RepoRoles, RepoRole{Repo: repo, Role: role})
		}
		out = append(out, p)
	}
	return out, total, res.Err()
}
