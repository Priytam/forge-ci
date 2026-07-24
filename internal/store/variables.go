package store

import (
	"context"
	"errors"
	"path"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

var ErrDuplicateVariable = errors.New("variable already exists for this repo/key/scope")

// ListVariables returns a repo's variables. Masked values are redacted
// unless reveal is set (GitLab's "Reveal values").
func (s *Store) ListVariables(ctx context.Context, repo string, reveal bool) ([]proto.Variable, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, key, value, protected, masked, environment_scope, created_at
		 FROM repo_variables WHERE repo=$1 ORDER BY key, environment_scope`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.Variable{}
	for rows.Next() {
		var v proto.Variable
		if err := rows.Scan(&v.ID, &v.Repo, &v.Key, &v.Value, &v.Protected,
			&v.Masked, &v.EnvironmentScope, &v.CreatedAt); err != nil {
			return nil, err
		}
		plain, err := s.cipher.Decrypt(v.Value)
		if err != nil {
			return nil, err
		}
		v.Value = plain
		if v.Masked && !reveal {
			v.Value = ""
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CreateVariable(ctx context.Context, req proto.VariableRequest) (*proto.Variable, error) {
	encVal, err := s.cipher.Encrypt(req.Value)
	if err != nil {
		return nil, err
	}
	var v proto.Variable
	err = s.pool.QueryRow(ctx,
		`INSERT INTO repo_variables (repo, key, value, protected, masked, environment_scope)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id, repo, key, value, protected, masked, environment_scope, created_at`,
		req.Repo, req.Key, encVal, req.Protected, req.Masked, req.EnvironmentScope).
		Scan(&v.ID, &v.Repo, &v.Key, &v.Value, &v.Protected, &v.Masked, &v.EnvironmentScope, &v.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicateVariable
	}
	if err != nil {
		return nil, err
	}
	v.Value = req.Value // return plaintext to the caller
	return &v, nil
}

func (s *Store) UpdateVariable(ctx context.Context, id int64, req proto.VariableRequest) (*proto.Variable, error) {
	encVal, err := s.cipher.Encrypt(req.Value)
	if err != nil {
		return nil, err
	}
	var v proto.Variable
	err = s.pool.QueryRow(ctx,
		`UPDATE repo_variables
		 SET value=$2, protected=$3, masked=$4, environment_scope=$5
		 WHERE id=$1
		 RETURNING id, repo, key, value, protected, masked, environment_scope, created_at`,
		id, encVal, req.Protected, req.Masked, req.EnvironmentScope).
		Scan(&v.ID, &v.Repo, &v.Key, &v.Value, &v.Protected, &v.Masked, &v.EnvironmentScope, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.Value = req.Value // return plaintext to the caller
	return &v, nil
}

func (s *Store) DeleteVariable(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM repo_variables WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// refProtected reports whether a ref matches any protected-ref pattern for
// the repo (or the global "" repo).
func (s *Store) refProtected(ctx context.Context, repo, ref string) (bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT pattern FROM protected_refs WHERE repo IN ('', $1)`, repo)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return false, err
		}
		if pattern == ref {
			return true, nil
		}
		if ok, err := path.Match(pattern, ref); err == nil && ok {
			return true, nil
		}
	}
	return false, rows.Err()
}

func scopeMatches(scope, environment string) bool {
	if scope == "*" {
		return true
	}
	if environment == "" {
		return false
	}
	if scope == environment {
		return true
	}
	ok, err := path.Match(scope, environment)
	return err == nil && ok
}

// ResolveVariables computes the repo variables to inject into a job:
// protected variables only on protected refs, environment_scope matched
// against the job's environment.
func (s *Store) ResolveVariables(ctx context.Context, repo, ref, environment string) (map[string]string, error) {
	refIsProtected, err := s.refProtected(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key, value, protected, environment_scope
		 FROM repo_variables WHERE repo=$1 ORDER BY environment_scope`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key, value, scope string
		var protected bool
		if err := rows.Scan(&key, &value, &protected, &scope); err != nil {
			return nil, err
		}
		if protected && !refIsProtected {
			continue
		}
		if !scopeMatches(scope, environment) {
			continue
		}
		plain, err := s.cipher.Decrypt(value)
		if err != nil {
			return nil, err
		}
		// Scoped variables override '*' ones for the same key: '*' sorts
		// before named scopes, so later rows win.
		out[key] = plain
	}
	return out, rows.Err()
}

// MaskedValuesForJob returns the masked variable values that were eligible
// for injection into this job, for log redaction at ingestion time.
func (s *Store) MaskedValuesForJob(ctx context.Context, jobID int64) ([]string, error) {
	var repo, ref string
	var environment *string
	err := s.pool.QueryRow(ctx,
		`SELECT p.repo, p.ref, j.environment FROM jobs j
		 JOIN pipelines p ON p.id = j.pipeline_id WHERE j.id=$1`, jobID).
		Scan(&repo, &ref, &environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	refIsProtected, err := s.refProtected(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	env := ""
	if environment != nil {
		env = *environment
	}
	rows, err := s.pool.Query(ctx,
		`SELECT value, protected, environment_scope
		 FROM repo_variables WHERE repo=$1 AND masked`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var value, scope string
		var protected bool
		if err := rows.Scan(&value, &protected, &scope); err != nil {
			return nil, err
		}
		if protected && !refIsProtected {
			continue
		}
		if scopeMatches(scope, env) {
			plain, err := s.cipher.Decrypt(value)
			if err != nil {
				return nil, err
			}
			out = append(out, plain)
		}
	}
	return out, rows.Err()
}
