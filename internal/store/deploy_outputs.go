package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
)

// Job outputs: dotenv reports, coverage, and the deployment record built
// from them. Nothing here knows how or where a job deployed — Kubernetes, a
// VM, a function, a CDN — only what the job said about it.

const (
	maxDotenvVars  = 100
	maxDotenvValue = 4096
)

var dotenvKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseDotenv parses KEY=VALUE lines (blank lines and # comments skipped;
// an optional "export " prefix and surrounding quotes are removed).
func ParseDotenv(body string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !dotenvKeyRe.MatchString(k) {
			return nil, fmt.Errorf("dotenv line %d: expected KEY=VALUE", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if len(v) > maxDotenvValue {
			return nil, fmt.Errorf("dotenv %s: value longer than %d bytes", k, maxDotenvValue)
		}
		if strings.HasPrefix(k, "CI_") || strings.HasPrefix(k, "FORGE_") {
			return nil, fmt.Errorf("dotenv %s: CI_ and FORGE_ variables are reserved", k)
		}
		out[k] = v
		if len(out) > maxDotenvVars {
			return nil, fmt.Errorf("dotenv: more than %d variables", maxDotenvVars)
		}
	}
	return out, sc.Err()
}

// SetJobDotenv stores a job's dotenv report (called by the runner before it
// reports completion, so the deployment record can use it).
func (s *Store) SetJobDotenv(ctx context.Context, jobID int64, vars map[string]string) error {
	raw, _ := json.Marshal(vars)
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET dotenv=$2 WHERE id=$1 AND status='running'`, jobID, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) neededDotenv(ctx context.Context, jobID int64) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT d.dotenv FROM job_needs n JOIN jobs d ON d.id = n.needs_job_id
		 WHERE n.job_id = $1 AND d.dotenv <> '{}'::jsonb ORDER BY d.id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		m := map[string]string{}
		_ = json.Unmarshal(raw, &m)
		for k, v := range m {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// deployFacts derives a deployment's URL, version and digest from what the
// job declared (environment: url:) and reported (its dotenv, then what it
// inherited from the jobs it needs). Version falls back to the short sha.
func deployFacts(envURL string, env map[string]string, sha string) (url, version, digest string) {
	url = env["DEPLOY_URL"]
	if url == "" {
		url = env["CI_ENVIRONMENT_URL"]
	}
	if url == "" && envURL != "" {
		url = os.Expand(envURL, func(k string) string { return env[k] })
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		url = "" // only web links are shown as links
	}
	version = env["DEPLOY_VERSION"]
	if version == "" && len(sha) >= 8 {
		version = sha[:8]
	} else if version == "" {
		version = sha
	}
	digest = env["DEPLOY_DIGEST"]
	return url, version, digest
}

// jobOutputEnv is the variable set a finished job's outputs are read from:
// its own YAML variables, the dotenv it inherited, then its own dotenv.
func (s *Store) jobOutputEnv(ctx context.Context, tx pgx.Tx, jobID int64) (envURL string, env map[string]string, err error) {
	var jobEnvRaw, dotenvRaw []byte
	if err := tx.QueryRow(ctx, `SELECT environment_url, env, dotenv FROM jobs WHERE id=$1`, jobID).
		Scan(&envURL, &jobEnvRaw, &dotenvRaw); err != nil {
		return "", nil, err
	}
	env = map[string]string{}
	_ = json.Unmarshal(jobEnvRaw, &env)
	inherited, _ := s.neededDotenv(ctx, jobID)
	for k, v := range inherited {
		env[k] = v
	}
	own := map[string]string{}
	_ = json.Unmarshal(dotenvRaw, &own)
	for k, v := range own {
		env[k] = v
	}
	return envURL, env, nil
}

// ---- coverage ----

var coberturaLineRate = regexp.MustCompile(`<coverage\b[^>]*\bline-rate="([0-9.]+)"`)

// ParseCobertura returns the overall line coverage (0-100) of a Cobertura
// report: the root <coverage line-rate="0.8312"> attribute.
func ParseCobertura(body string) (float64, error) {
	m := coberturaLineRate.FindStringSubmatch(body)
	if m == nil {
		return 0, errors.New("not a Cobertura report (no <coverage line-rate=...>)")
	}
	var rate float64
	if _, err := fmt.Sscanf(m[1], "%g", &rate); err != nil || rate < 0 || rate > 1 {
		return 0, fmt.Errorf("invalid line-rate %q", m[1])
	}
	return float64(int(rate*10000+0.5)) / 100, nil
}

// SetJobCoverage records a job's coverage percentage.
func (s *Store) SetJobCoverage(ctx context.Context, jobID int64, pct float64) error {
	_, err := s.pool.Exec(ctx, `UPDATE jobs SET coverage=$2 WHERE id=$1`, jobID, pct)
	return err
}

// CoverageFromLog fills in a job's coverage from its coverage: regex once the
// job has finished, unless a report already set it. The log is the masked one.
func (s *Store) CoverageFromLog(ctx context.Context, jobID int64, log string) error {
	var pattern string
	var have *float64
	if err := s.pool.QueryRow(ctx, `SELECT coverage_regex, coverage FROM jobs WHERE id=$1`, jobID).
		Scan(&pattern, &have); err != nil || pattern == "" || have != nil {
		return err
	}
	if pct, ok := compiler.ExtractCoverage(pattern, log); ok {
		return s.SetJobCoverage(ctx, jobID, pct)
	}
	return nil
}

// CoveragePoint is one pipeline's coverage, for a repo's trend.
type CoveragePoint struct {
	PipelineID int64   `json:"pipeline_id"`
	Ref        string  `json:"ref"`
	SHA        string  `json:"sha"`
	Coverage   float64 `json:"coverage"`
	CreatedAt  string  `json:"created_at"`
}

// CoverageTrend returns the latest pipelines' coverage for a repo (optionally
// one ref), oldest first.
func (s *Store) CoverageTrend(ctx context.Context, repo, ref string, limit int) ([]CoveragePoint, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT * FROM (
		   SELECT p.id, p.ref, p.sha, round(avg(j.coverage)::numeric, 2)::float8, to_char(p.created_at, 'YYYY-MM-DD"T"HH24:MI:SSOF')
		   FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id
		   WHERE p.repo = $1 AND ($2 = '' OR p.ref = $2) AND j.coverage IS NOT NULL AND NOT j.superseded
		   GROUP BY p.id ORDER BY p.id DESC LIMIT $3) t
		 ORDER BY 1`, repo, ref, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CoveragePoint, error) {
		var c CoveragePoint
		err := r.Scan(&c.PipelineID, &c.Ref, &c.SHA, &c.Coverage, &c.CreatedAt)
		return c, err
	})
}
