package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// envRank orders well-known environment names along the usual promotion path
// (dev → staging → prod) so every view lists them the same way; unknown names
// sort after these, alphabetically.
var envRank = map[string]int{
	"local": 0, "dev": 1, "development": 1, "test": 2, "testing": 2, "qa": 3,
	"integration": 3, "staging": 4, "stage": 4, "uat": 5, "preprod": 6,
	"pre-prod": 6, "canary": 7, "prod": 8, "production": 8,
}

// SortEnvironments sorts names in promotion order (see envRank), in place.
func SortEnvironments(names []string) { SortEnvironmentsBy(names, nil) }

// SortEnvironmentsBy sorts names in place: first the admin-registered order
// (registered, by position), then well-known names in promotion order, then
// the rest alphabetically.
func SortEnvironmentsBy(names, registered []string) {
	pos := map[string]int{}
	for i, n := range registered {
		pos[strings.ToLower(n)] = i
	}
	rank := func(n string) (int, bool) {
		if p, ok := pos[strings.ToLower(n)]; ok {
			return p, true
		}
		if r, ok := envRank[strings.ToLower(n)]; ok {
			return len(registered) + r, true
		}
		return 0, false
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri, iok := rank(names[i])
		rj, jok := rank(names[j])
		switch {
		case iok && jok && ri != rj:
			return ri < rj
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})
}

// sortEnvs orders names using this server's registry.
func (s *Store) sortEnvs(ctx context.Context, names []string) error {
	p, err := s.GetEnvPolicy(ctx)
	if err != nil {
		return err
	}
	reg := make([]string, len(p.Environments))
	for i, e := range p.Environments {
		reg[i] = e.Name
	}
	SortEnvironmentsBy(names, reg)
	return nil
}

// recentEnvJobsWindow bounds the scan for environments and repos that have
// environment jobs but no successful deployment yet.
const recentEnvJobsWindow = 20000

// attemptLookback bounds how far back the board looks for the latest deploy
// attempt (running, waiting for approval, failed) per repo and environment.
// The current deployment comes from the deployments table and is not bounded.
const attemptLookback = 30 * 24 * time.Hour

// OrgEnvironments returns every environment name in use, in promotion order:
// ones deployed to, ones with an approval rule, and ones targeted by recent
// jobs.
func (s *Store) OrgEnvironments(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`SELECT DISTINCT environment FROM deployments
		 UNION SELECT name FROM environment_registry
		 UNION SELECT name FROM protected_environments
		 UNION SELECT environment FROM (
		     SELECT environment FROM jobs WHERE environment IS NOT NULL
		     ORDER BY id DESC LIMIT %d) j
		 WHERE environment <> ''`, recentEnvJobsWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		if e != "" {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.sortEnvs(ctx, out)
}

// EnvAttempt is the latest job that targeted an environment, whatever its
// outcome — it shows a deploy in flight, waiting for approval, or failing.
type EnvAttempt struct {
	JobID      int64     `json:"job_id"`
	JobName    string    `json:"job_name"`
	PipelineID int64     `json:"pipeline_id"`
	Status     string    `json:"status"`
	Reason     string    `json:"status_reason,omitempty"`
	Manual     bool      `json:"manual"`
	Ref        string    `json:"ref"`
	SHA        string    `json:"sha"`
	CreatedAt  time.Time `json:"created_at"`
}

// EnvCell is one repo × environment cell of the board.
type EnvCell struct {
	Current *proto.Deployment `json:"current"`
	Latest  *EnvAttempt       `json:"latest"`
	Frozen  bool              `json:"frozen"`
}

// EnvMatrixRow is one repo's row: environment name → cell (absent = never
// deployed or attempted).
type EnvMatrixRow struct {
	Repo  string              `json:"repo"`
	Cells map[string]*EnvCell `json:"cells"`
}

// EnvMatrixFilter narrows the board. Env keeps only repos with activity in
// that environment; Q matches repo names case-insensitively; Attention keeps
// only repos whose latest attempt is failed, blocked or canceled (in Env when
// set, else in any environment).
type EnvMatrixFilter struct {
	Q         string
	Env       string
	Attention bool
}

// EnvMatrix returns one page of the org-wide environment board, repos sorted
// by name, plus the total matching repo count. Only repos that have deployed,
// or recently tried to deploy, to some environment are listed.
func (s *Store) EnvMatrix(ctx context.Context, f EnvMatrixFilter, limit, offset int) ([]EnvMatrixRow, int, error) {
	since := time.Now().Add(-attemptLookback)
	// Latest attempt per (repo, env) within the lookback, over pipelines by
	// creation time (pipelines_created_idx) and their environment jobs.
	const attemptsCTE = `attempts AS (
	    SELECT DISTINCT ON (p.repo, j.environment)
	           p.repo, j.environment, j.id AS job_id, j.name, p.id AS pipeline_id,
	           j.status, j.status_reason, j.manual, p.ref, p.sha, p.created_at
	    FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id
	    WHERE p.created_at > $1 AND j.environment IS NOT NULL AND j.environment <> ''
	    ORDER BY p.repo, j.environment, j.id DESC)`
	conds := []string{"TRUE"}
	args := []any{since}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "$?", fmt.Sprintf("$%d", len(args))))
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		add(`r.repo ILIKE '%' || $? || '%'`, escapeLike(q))
	}
	if f.Env != "" {
		add(`(EXISTS (SELECT 1 FROM deployments d WHERE d.repo = r.repo AND d.environment = $?)
		      OR EXISTS (SELECT 1 FROM attempts a WHERE a.repo = r.repo AND a.environment = $?))`, f.Env)
	}
	if f.Attention {
		const needs = `EXISTS (SELECT 1 FROM attempts a WHERE a.repo = r.repo
		    AND a.status IN ('failed','blocked','canceled')`
		if f.Env != "" {
			add(needs+` AND a.environment = $?)`, f.Env)
		} else {
			conds = append(conds, needs+`)`)
		}
	}
	reposSQL := `WITH ` + attemptsCTE + `,
	    repos AS (SELECT DISTINCT repo FROM deployments UNION SELECT repo FROM attempts)
	    SELECT r.repo FROM repos r WHERE ` + strings.Join(conds, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (`+reposSQL+`) x`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(args)
	rows, err := s.pool.Query(ctx, reposSQL+fmt.Sprintf(` ORDER BY r.repo LIMIT $%d OFFSET $%d`, n+1, n+2),
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	var repos []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, 0, err
		}
		repos = append(repos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	out := make([]EnvMatrixRow, len(repos))
	byRepo := map[string]*EnvMatrixRow{}
	for i, r := range repos {
		out[i] = EnvMatrixRow{Repo: r, Cells: map[string]*EnvCell{}}
		byRepo[r] = &out[i]
	}
	if len(repos) == 0 {
		return out, total, nil
	}
	cell := func(repo, env string) *EnvCell {
		row := byRepo[repo]
		c, ok := row.Cells[env]
		if !ok {
			c = &EnvCell{}
			row.Cells[env] = c
		}
		return c
	}

	// Current (latest successful) deployment per repo × env.
	drows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (repo, environment)
		        id, repo, environment, sha, ref, pipeline_id, job_id, deployed_by, deployed_at, status, url, version, digest, promoted_from
		 FROM deployments WHERE repo = ANY($1) AND status = 'success'
		 ORDER BY repo, environment, id DESC`, repos)
	if err != nil {
		return nil, 0, err
	}
	for drows.Next() {
		var d proto.Deployment
		if err := drows.Scan(&d.ID, &d.Repo, &d.Environment, &d.SHA, &d.Ref, &d.PipelineID,
			&d.JobID, &d.DeployedBy, &d.DeployedAt, &d.Status, &d.URL, &d.Version, &d.Digest, &d.PromotedFrom); err != nil {
			drows.Close()
			return nil, 0, err
		}
		cell(d.Repo, d.Environment).Current = &d
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return nil, 0, err
	}

	// Latest attempt per repo × env (same CTE, limited to this page's repos).
	arows, err := s.pool.Query(ctx, `WITH `+attemptsCTE+`
		SELECT repo, environment, job_id, name, pipeline_id, status, status_reason, manual, ref, sha, created_at
		FROM attempts WHERE repo = ANY($2)`, since, repos)
	if err != nil {
		return nil, 0, err
	}
	for arows.Next() {
		var repo, env string
		var a EnvAttempt
		if err := arows.Scan(&repo, &env, &a.JobID, &a.JobName, &a.PipelineID, &a.Status,
			&a.Reason, &a.Manual, &a.Ref, &a.SHA, &a.CreatedAt); err != nil {
			arows.Close()
			return nil, 0, err
		}
		cell(repo, env).Latest = &a
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, 0, err
	}

	// Active freezes (repo-specific or global).
	frows, err := s.pool.Query(ctx,
		`SELECT repo, environment FROM deploy_freezes
		 WHERE now() >= starts_at AND now() < ends_at AND (repo = '' OR repo = ANY($1))`, repos)
	if err != nil {
		return nil, 0, err
	}
	for frows.Next() {
		var repo, env string
		if err := frows.Scan(&repo, &env); err != nil {
			frows.Close()
			return nil, 0, err
		}
		targets := []string{repo}
		if repo == "" {
			targets = repos
		}
		for _, r := range targets {
			cell(r, env).Frozen = true
		}
	}
	frows.Close()
	return out, total, frows.Err()
}

// escapeLike escapes LIKE wildcards so user input matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---- jobs across pipelines ----

// JobStatuses are the job statuses a filter may name.
var JobStatuses = map[string]bool{
	"created": true, "blocked": true, "pending": true, "running": true,
	"success": true, "failed": true, "canceled": true,
}

// JobFilter narrows the cross-pipeline job list. Empty fields don't filter.
type JobFilter struct {
	Repos       []string
	Ref         string
	Statuses    []string
	Runner      string
	Environment string
	Name        string // case-insensitive substring of the job name
	// StartedFrom / StartedTo bound started_at (history drill-down).
	StartedFrom, StartedTo *time.Time
}

// Validate rejects unknown statuses.
func (f JobFilter) Validate() error {
	for _, st := range f.Statuses {
		if !JobStatuses[st] {
			return fmt.Errorf("unknown job status %q", st)
		}
	}
	return nil
}

// JobRow is one job in the cross-pipeline list, with its pipeline context.
type JobRow struct {
	ID           int64      `json:"id"`
	PipelineID   int64      `json:"pipeline_id"`
	Repo         string     `json:"repo"`
	Ref          string     `json:"ref"`
	SHA          string     `json:"sha"`
	Name         string     `json:"name"`
	Stage        string     `json:"stage"`
	Environment  *string    `json:"environment"`
	Status       string     `json:"status"`
	Manual       bool       `json:"manual"`
	AllowFailure bool       `json:"allow_failure"`
	RunnerID     *string    `json:"runner_id"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	StatusReason string     `json:"status_reason,omitempty"`
}

// ListJobsFiltered returns one page of jobs across all pipelines, newest
// first, plus the total match count.
func (s *Store) ListJobsFiltered(ctx context.Context, f JobFilter, limit, offset int) ([]JobRow, int, error) {
	conds := []string{"TRUE"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "$?", fmt.Sprintf("$%d", len(args))))
	}
	if len(f.Repos) > 0 {
		add("p.repo = ANY($?)", f.Repos)
	}
	if f.Ref != "" {
		add("p.ref = $?", f.Ref)
	}
	if len(f.Statuses) > 0 {
		add("j.status = ANY($?)", f.Statuses)
	}
	if f.Runner != "" {
		add("j.runner_id = $?", f.Runner)
	}
	if f.Environment != "" {
		add("j.environment = $?", f.Environment)
	}
	if f.Name != "" {
		add(`j.name ILIKE '%' || $? || '%'`, escapeLike(f.Name))
	}
	if f.StartedFrom != nil {
		add("j.started_at >= $?", *f.StartedFrom)
	}
	if f.StartedTo != nil {
		add("j.started_at < $?", *f.StartedTo)
	}
	where := strings.Join(conds, " AND ")
	const from = ` FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id WHERE `
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(args)
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.pipeline_id, p.repo, p.ref, p.sha, j.name, j.stage, j.environment,
		        j.status, j.manual, j.allow_failure, j.runner_id, p.created_at,
		        j.started_at, j.finished_at, j.status_reason`+from+where+
			fmt.Sprintf(` ORDER BY j.id DESC LIMIT $%d OFFSET $%d`, n+1, n+2),
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []JobRow{}
	for rows.Next() {
		var r JobRow
		if err := rows.Scan(&r.ID, &r.PipelineID, &r.Repo, &r.Ref, &r.SHA, &r.Name, &r.Stage,
			&r.Environment, &r.Status, &r.Manual, &r.AllowFailure, &r.RunnerID, &r.CreatedAt,
			&r.StartedAt, &r.FinishedAt, &r.StatusReason); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
