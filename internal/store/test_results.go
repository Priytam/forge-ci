package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// defaultCaseHistoryRuns and maxCaseHistoryRuns bound ?limit on the per-repo
// test history endpoint, mirroring the page-size cap api.pageParams applies to
// list endpoints — except this limit is PER CASE (runs kept per test), not a
// total row count.
const (
	defaultCaseHistoryRuns = 20
	maxCaseHistoryRuns     = 100
)

// SaveTestCaseResults records every case from one job's parsed JUnit report.
// Unlike job_reports (an aggregate overwritten on re-upload), this is one row
// per case per job, kept so a case's pass/fail trend can be read back across
// every run Forge has seen (ListTestCaseHistory). A re-upload of THIS job's
// report (same job_id) replaces that job's own rows rather than duplicating
// them — matching job_reports' re-upload semantics — while a different job's
// rows are never touched, which is what keeps the history append-only across
// runs. A no-op when cases is empty (e.g. a job whose report parsed but
// produced no usable case list).
func (s *Store) SaveTestCaseResults(ctx context.Context, jobID int64, cases []proto.TestCaseResult) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM test_case_results WHERE job_id=$1`, jobID); err != nil {
		return err
	}
	if len(cases) > 0 {
		rows := make([][]any, len(cases))
		for i, c := range cases {
			rows[i] = []any{jobID, c.Name, c.Classname, c.Status, c.DurationSeconds, c.Message}
		}
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"test_case_results"},
			[]string{"job_id", "name", "classname", "status", "duration_seconds", "message"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListTestCaseHistory returns, for every test case Forge has recorded for
// repo, its most recent runs (newest first, capped at `limit` per case — 0
// means the default). Cases are ordered by (classname, name), which is the
// only ordering the store imposes; a caller sorting for "currently failing
// first" does so itself from each case's Runs[0].
func (s *Store) ListTestCaseHistory(ctx context.Context, repo string, limit int) ([]proto.TestCaseHistory, error) {
	if limit <= 0 {
		limit = defaultCaseHistoryRuns
	}
	if limit > maxCaseHistoryRuns {
		limit = maxCaseHistoryRuns
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, name, classname, status, duration_seconds, message, created_at
		 FROM (
		   SELECT tcr.*, ROW_NUMBER() OVER (
		            PARTITION BY tcr.classname, tcr.name ORDER BY tcr.created_at DESC
		          ) AS rn
		   FROM test_case_results tcr
		   JOIN jobs j ON j.id = tcr.job_id
		   JOIN pipelines p ON p.id = j.pipeline_id
		   WHERE p.repo = $1
		 ) ranked
		 WHERE rn <= $2
		 ORDER BY classname, name, created_at DESC`,
		repo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.TestCaseHistory{}
	for rows.Next() {
		var r proto.TestCaseResult
		if err := rows.Scan(&r.ID, &r.JobID, &r.Name, &r.Classname, &r.Status,
			&r.DurationSeconds, &r.Message, &r.CreatedAt); err != nil {
			return nil, err
		}
		// Rows for the same case arrive consecutively (ORDER BY classname, name),
		// so appending to the last group is enough — no need to key a map.
		if n := len(out); n > 0 && out[n-1].Name == r.Name && out[n-1].Classname == r.Classname {
			out[n-1].Runs = append(out[n-1].Runs, r)
		} else {
			out = append(out, proto.TestCaseHistory{
				Name: r.Name, Classname: r.Classname, Runs: []proto.TestCaseResult{r},
			})
		}
	}
	return out, rows.Err()
}
