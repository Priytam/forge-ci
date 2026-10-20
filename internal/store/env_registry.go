package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RegisteredEnv is one admin-defined environment.
type RegisteredEnv struct {
	Name        string   `json:"name"`
	Tier        string   `json:"tier"` // dev | staging | prod | other
	Aliases     []string `json:"aliases"`
	AllowedRefs []string `json:"allowed_refs"` // glob patterns; empty = any branch
	Description string   `json:"description"`
	// CancelSuperseded: a deploy to this environment that is still waiting
	// (queued, gated on approval or a freeze) is canceled once a deploy from a
	// NEWER pipeline has started or succeeded here — an old build never lands
	// on top of a newer one.
	CancelSuperseded bool `json:"cancel_superseded"`
}

// EnvPolicy is the whole environment configuration of a server: the ordered
// environment list plus what to do with names that aren't on it.
type EnvPolicy struct {
	Environments []RegisteredEnv `json:"environments"`
	// Unknown is "allow" (use the name as written) or "deny" (the job fails
	// without running). Only applies when Environments is non-empty.
	Unknown string `json:"unknown"`
}

var envTiers = map[string]bool{"dev": true, "staging": true, "prod": true, "other": true}

var envNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

const envPolicyKey = "environment_policy"

// ErrEnvConflict reports data that can't be renamed to a canonical name
// without clobbering a row that already uses it.
var ErrEnvConflict = errors.New("environment rename conflict")

// Validate normalises and checks a policy: names and aliases unique across
// the whole list (case-insensitive), sane characters, known tiers.
func (p *EnvPolicy) Validate() error {
	if p.Unknown == "" {
		p.Unknown = "allow"
	}
	if p.Unknown != "allow" && p.Unknown != "deny" {
		return errors.New(`unknown must be "allow" or "deny"`)
	}
	seen := map[string]string{}
	claim := func(name, owner string) error {
		k := strings.ToLower(name)
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("%q is used by both %q and %q", name, prev, owner)
		}
		seen[k] = owner
		return nil
	}
	for i := range p.Environments {
		e := &p.Environments[i]
		e.Name = strings.TrimSpace(e.Name)
		if !envNameRe.MatchString(e.Name) {
			return fmt.Errorf("invalid environment name %q", e.Name)
		}
		if e.Tier == "" {
			e.Tier = "other"
		}
		if !envTiers[e.Tier] {
			return fmt.Errorf("%s: tier must be dev, staging, prod or other", e.Name)
		}
		if err := claim(e.Name, e.Name); err != nil {
			return err
		}
		aliases := []string{}
		for _, a := range e.Aliases {
			if a = strings.TrimSpace(a); a == "" {
				continue
			}
			if !envNameRe.MatchString(a) {
				return fmt.Errorf("%s: invalid alias %q", e.Name, a)
			}
			if err := claim(a, e.Name); err != nil {
				return err
			}
			aliases = append(aliases, a)
		}
		e.Aliases = aliases
		refs := []string{}
		for _, r := range e.AllowedRefs {
			if r = strings.TrimSpace(r); r != "" {
				refs = append(refs, r)
			}
		}
		e.AllowedRefs = refs
	}
	return nil
}

// Resolve maps a name a pipeline wrote to its registered environment
// (matching the name or an alias, case-insensitively). nil when not
// registered.
func (p *EnvPolicy) Resolve(name string) *RegisteredEnv {
	for i := range p.Environments {
		e := &p.Environments[i]
		if strings.EqualFold(e.Name, name) {
			return e
		}
		for _, a := range e.Aliases {
			if strings.EqualFold(a, name) {
				return e
			}
		}
	}
	return nil
}

// Canonical returns the registered name for name (an alias resolves to its
// environment), or name itself when it isn't registered.
func (p *EnvPolicy) Canonical(name string) string {
	if e := p.Resolve(name); e != nil {
		return e.Name
	}
	return name
}

// AllowsRef reports whether ref may deploy to e. A ref of the form
// refs/heads/x is matched as x. '*' in a pattern matches any characters,
// including '/', so release/* covers release/1.2 and release/2024/q1.
func (e *RegisteredEnv) AllowsRef(ref string) bool {
	if len(e.AllowedRefs) == 0 {
		return true
	}
	ref = strings.TrimPrefix(ref, "refs/heads/")
	for _, pat := range e.AllowedRefs {
		if globMatch(pat, ref) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, err := regexp.MatchString(re, s)
	return err == nil && ok
}

// CheckJob applies the policy to a job targeting env on ref: it returns the
// canonical environment name and, when the job must not run, why.
func (p *EnvPolicy) CheckJob(env, ref string) (canonical, denied string) {
	if env == "" || len(p.Environments) == 0 {
		return env, ""
	}
	e := p.Resolve(env)
	if e == nil {
		if p.Unknown == "deny" {
			names := make([]string, len(p.Environments))
			for i, x := range p.Environments {
				names[i] = x.Name
			}
			return env, fmt.Sprintf("environment %q is not registered on this server (known: %s)",
				env, strings.Join(names, ", "))
		}
		return env, ""
	}
	if !e.AllowsRef(ref) {
		return e.Name, fmt.Sprintf("branch %q may not deploy to %s (allowed: %s)",
			strings.TrimPrefix(ref, "refs/heads/"), e.Name, strings.Join(e.AllowedRefs, ", "))
	}
	return e.Name, ""
}

// GetEnvPolicy returns the server's environment policy (empty when unset).
func (s *Store) GetEnvPolicy(ctx context.Context) (*EnvPolicy, error) {
	p := &EnvPolicy{Environments: []RegisteredEnv{}, Unknown: "allow"}
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM platform_settings WHERE key=$1`, envPolicyKey).Scan(&raw)
	if err == nil {
		var settings struct {
			Unknown string `json:"unknown"`
		}
		if json.Unmarshal(raw, &settings) == nil && settings.Unknown != "" {
			p.Unknown = settings.Unknown
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT name, tier, aliases, allowed_refs, description, cancel_superseded
		 FROM environment_registry ORDER BY position, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e RegisteredEnv
		if err := rows.Scan(&e.Name, &e.Tier, &e.Aliases, &e.AllowedRefs, &e.Description, &e.CancelSuperseded); err != nil {
			return nil, err
		}
		p.Environments = append(p.Environments, e)
	}
	return p, rows.Err()
}

// renameTargets are the places an environment name is stored. Saving the
// registry renames alias values to the canonical name in each, so every
// existing exact-match lookup (approval rules, freezes, scoped variables, the
// scheduler, history) keeps working with one name per environment.
var renameTargets = []struct {
	table, column, unique string // unique: other columns that, with column, must stay unique
}{
	{"jobs", "environment", ""},
	{"deployments", "environment", ""},
	{"deploy_freezes", "environment", ""},
	{"protected_environments", "name", "repo"},
	{"repo_variables", "environment_scope", "repo, key"},
}

// SaveEnvPolicy replaces the registry and renames stored alias values to
// their canonical names, all in one transaction. When a rename would collide
// with an existing row (e.g. a repo has approval rules for both "prod" and
// "production"), nothing is changed and an ErrEnvConflict-wrapped error
// names the collision.
func (s *Store) SaveEnvPolicy(ctx context.Context, p EnvPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// Approval rules saved under an alias or case variant of a registered
	// name are folded into the canonical rule first, so the rename below
	// never meets two rules for one environment.
	if _, err := s.foldProtectedEnvRules(ctx, &p); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, e := range p.Environments {
		if len(e.Aliases) == 0 {
			continue
		}
		// Case variants of the canonical name are aliases too.
		names := append([]string{e.Name}, e.Aliases...)
		for _, t := range renameTargets {
			if t.unique != "" {
				var clash string
				err := tx.QueryRow(ctx, fmt.Sprintf(
					`SELECT concat_ws(' / ', %[3]s, string_agg(DISTINCT %[2]s, ', '))
					 FROM %[1]s WHERE lower(%[2]s) = ANY($1)
					 GROUP BY %[3]s HAVING count(DISTINCT %[2]s) > 1 LIMIT 1`,
					t.table, t.column, t.unique), lowerAll(names)).Scan(&clash)
				if err == nil {
					return fmt.Errorf("%w: %s has rows for %s — keep one, then save again",
						ErrEnvConflict, t.table, clash)
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				`UPDATE %[1]s SET %[2]s = $1 WHERE lower(%[2]s) = ANY($2) AND %[2]s <> $1`,
				t.table, t.column), e.Name, lowerAll(names)); err != nil {
				return err
			}
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM environment_registry`); err != nil {
		return err
	}
	for i, e := range p.Environments {
		if _, err := tx.Exec(ctx,
			`INSERT INTO environment_registry (name, position, tier, aliases, allowed_refs, description, cancel_superseded)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			e.Name, i, e.Tier, e.Aliases, e.AllowedRefs, e.Description, e.CancelSuperseded); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(map[string]string{"unknown": p.Unknown})
	if _, err := tx.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		envPolicyKey, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
