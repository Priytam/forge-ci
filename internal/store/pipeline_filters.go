package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// PipelineFilter narrows the pipeline list. Empty fields don't filter. All
// filtering happens in SQL so the list stays fast with many pipelines.
type PipelineFilter struct {
	Repos       []string `json:"repos,omitempty"`
	Ref         string   `json:"ref,omitempty"`
	Statuses    []string `json:"statuses,omitempty"`
	Runner      string   `json:"runner,omitempty"`
	Environment string   `json:"environment,omitempty"`
}

// PipelineStatuses are the derived pipeline statuses a filter may name.
var PipelineStatuses = map[string]bool{
	"created": true, "running": true, "blocked": true,
	"success": true, "failed": true, "canceled": true,
}

// Validate rejects unknown statuses so a typo filters nothing rather than
// silently matching nothing.
func (f PipelineFilter) Validate() error {
	for _, st := range f.Statuses {
		if !PipelineStatuses[st] {
			return fmt.Errorf("unknown status %q", st)
		}
	}
	return nil
}

// where builds the WHERE clause (over pipelines aliased p) and its args.
func (f PipelineFilter) where() (string, []any) {
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
	if f.Runner != "" {
		add("EXISTS (SELECT 1 FROM jobs j WHERE j.pipeline_id = p.id AND NOT j.superseded AND j.runner_id = $?)", f.Runner)
	}
	if f.Environment != "" {
		add("EXISTS (SELECT 1 FROM jobs j WHERE j.pipeline_id = p.id AND NOT j.superseded AND j.environment = $?)", f.Environment)
	}
	if len(f.Statuses) > 0 {
		add("p.status = ANY($?)", f.Statuses)
	}
	return strings.Join(conds, " AND "), args
}

// ListPipelinesFiltered returns one page of pipelines matching f, newest
// first, plus the total match count. limit/offset are clamped by the caller.
func (s *Store) ListPipelinesFiltered(ctx context.Context, f PipelineFilter, limit, offset int) ([]proto.Pipeline, int, error) {
	where, args := f.where()
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pipelines p WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(args)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`WITH page AS (
		     SELECT p.id FROM pipelines p WHERE %s
		     ORDER BY p.id DESC LIMIT $%d OFFSET $%d)
		 SELECT %s
		 FROM page JOIN pipelines p ON p.id = page.id
		 LEFT JOIN jobs j ON j.pipeline_id = p.id AND NOT j.superseded
		 GROUP BY p.id ORDER BY p.id DESC`, where, n+1, n+2, pipelineWithStagesCols), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanPipelines(rows)
	return out, total, err
}

// pipelineWithStagesCols selects a pipeline (aliased p) plus its jobs
// (aliased j, LEFT JOINed) folded into JSON for scanPipelines; the query must
// GROUP BY p.id.
const pipelineWithStagesCols = `p.id, p.repo, p.ref, p.sha, p.config_version, p.source, p.triggered_by,
	p.commit_author, p.commit_message, p.created_at,
	COALESCE(json_agg(json_build_object(
	    'stage', j.stage, 'idx', j.stage_idx, 'status', CASE WHEN j.status='failed' AND j.allow_failure THEN 'success' ELSE j.status END
	) ORDER BY j.stage_idx) FILTER (WHERE j.id IS NOT NULL), '[]')`

// scanPipelines reads rows selected with pipelineWithStagesCols, deriving
// each pipeline's overall and per-stage status.
func scanPipelines(rows pgx.Rows) ([]proto.Pipeline, error) {
	defer rows.Close()
	out := []proto.Pipeline{}
	for rows.Next() {
		var p proto.Pipeline
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.Source,
			&p.TriggeredBy, &p.CommitAuthor, &p.CommitMessage, &p.CreatedAt, &raw); err != nil {
			return nil, err
		}
		var sjs []stageJob
		if err := json.Unmarshal(raw, &sjs); err != nil {
			return nil, err
		}
		statuses := make([]string, len(sjs))
		for i, sj := range sjs {
			statuses[i] = sj.Status
		}
		p.Status = deriveStatus(statuses)
		p.Stages = deriveStages(sjs)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PipelineFacets are the values the filter dropdowns offer.
type PipelineFacets struct {
	Repos        []string `json:"repos"`
	Refs         []string `json:"refs"`
	Runners      []string `json:"runners"`
	Environments []string `json:"environments"`
}

// facetWindow bounds the job/pipeline scan behind the dropdowns: values seen
// in the most recent rows are what anyone filters by.
const facetWindow = 20000

// ListPipelineFacets returns the distinct repos, refs, runners and
// environments seen recently, optionally limited to repos.
func (s *Store) ListPipelineFacets(ctx context.Context, repos []string) (*PipelineFacets, error) {
	f := &PipelineFacets{}
	list := func(dst *[]string, q string, args ...any) error {
		rows, err := s.pool.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		*dst = []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			*dst = append(*dst, v)
		}
		return rows.Err()
	}
	if err := list(&f.Repos, `SELECT DISTINCT repo FROM pipelines ORDER BY repo`); err != nil {
		return nil, err
	}
	if repos == nil {
		repos = []string{}
	}
	recent := fmt.Sprintf(`SELECT id, repo, ref FROM pipelines
		WHERE (cardinality($1::text[]) = 0 OR repo = ANY($1))
		ORDER BY id DESC LIMIT %d`, facetWindow)
	if err := list(&f.Refs, `SELECT ref FROM (`+recent+`) r GROUP BY ref ORDER BY max(id) DESC LIMIT 100`, repos); err != nil {
		return nil, err
	}
	if err := list(&f.Runners, `SELECT DISTINCT j.runner_id FROM (`+recent+`) r
		JOIN jobs j ON j.pipeline_id = r.id WHERE j.runner_id IS NOT NULL
		UNION SELECT id FROM runners ORDER BY 1`, repos); err != nil {
		return nil, err
	}
	if err := list(&f.Environments, `SELECT DISTINCT j.environment FROM (`+recent+`) r
		JOIN jobs j ON j.pipeline_id = r.id WHERE j.environment IS NOT NULL AND j.environment <> ''
		ORDER BY 1`, repos); err != nil {
		return nil, err
	}
	return f, s.sortEnvs(ctx, f.Environments)
}

// PipelineView is a saved, named pipeline-list filter.
type PipelineView struct {
	ID        int64          `json:"id"`
	Owner     string         `json:"owner"`
	Name      string         `json:"name"`
	Filters   PipelineFilter `json:"filters"`
	Shared    bool           `json:"shared"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

var ErrDuplicateView = errors.New("you already have a view with this name")

const viewCols = `id, owner, name, filters, shared, created_at, updated_at`

func scanView(row pgx.Row) (*PipelineView, error) {
	var v PipelineView
	var raw []byte
	if err := row.Scan(&v.ID, &v.Owner, &v.Name, &raw, &v.Shared, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &v.Filters); err != nil {
		return nil, err
	}
	return &v, nil
}

// ListPipelineViews returns owner's views plus every shared view.
func (s *Store) ListPipelineViews(ctx context.Context, owner string) ([]PipelineView, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+viewCols+` FROM pipeline_views
		 WHERE lower(owner) = lower($1) OR shared
		 ORDER BY lower(name), id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PipelineView{}
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// GetPipelineView returns one view by id.
func (s *Store) GetPipelineView(ctx context.Context, id int64) (*PipelineView, error) {
	v, err := scanView(s.pool.QueryRow(ctx,
		`SELECT `+viewCols+` FROM pipeline_views WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func viewErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicateView
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// CreatePipelineView saves a new view for owner.
func (s *Store) CreatePipelineView(ctx context.Context, owner, name string, f PipelineFilter, shared bool) (*PipelineView, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	v, err := scanView(s.pool.QueryRow(ctx,
		`INSERT INTO pipeline_views (owner, name, filters, shared) VALUES ($1,$2,$3,$4)
		 RETURNING `+viewCols, owner, name, raw, shared))
	return v, viewErr(err)
}

// UpdatePipelineView replaces a view's name, filters and sharing.
func (s *Store) UpdatePipelineView(ctx context.Context, id int64, name string, f PipelineFilter, shared bool) (*PipelineView, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	v, err := scanView(s.pool.QueryRow(ctx,
		`UPDATE pipeline_views SET name=$2, filters=$3, shared=$4, updated_at=now()
		 WHERE id=$1 RETURNING `+viewCols, id, name, raw, shared))
	return v, viewErr(err)
}

// DeletePipelineView removes a view.
func (s *Store) DeletePipelineView(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pipeline_views WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
