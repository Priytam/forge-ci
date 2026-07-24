package store

import (
	"context"
	"encoding/json"
	"time"
)

// AuditEntry is one row of the append-only audit trail. Detail carries only
// safe metadata — never secret values, tokens or client secrets.
type AuditEntry struct {
	ID       int64          `json:"id"`
	TS       time.Time      `json:"ts"`
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Target   string         `json:"target"`
	Repo     string         `json:"repo"`
	Detail   map[string]any `json:"detail"`
	SourceIP string         `json:"source_ip"`
	Result   string         `json:"result"`
}

// InsertAudit appends one audit row. It is the only write path (there is no
// update/delete in normal operation — the retention sweep is the sole pruner).
func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, repo, detail, source_ip, result)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.Actor, e.Action, e.Target, e.Repo, raw, e.SourceIP, e.Result)
	return err
}

// AuditFilter narrows an audit-log listing. Limit/offset are assumed clamped by
// the caller; empty Repo/Actor mean "no filter".
type AuditFilter struct {
	Repo   string
	Actor  string
	Limit  int
	Offset int
}

// ListAudit returns a page of audit rows, newest first. Secret detail is never
// stored, so nothing needs redacting here.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, ts, actor, action, target, repo, detail, source_ip, result
		 FROM audit_log
		 WHERE ($1 = '' OR repo = $1)
		   AND ($2 = '' OR actor = $2)
		 ORDER BY id DESC
		 LIMIT $3 OFFSET $4`,
		f.Repo, f.Actor, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var raw []byte
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &e.Target,
			&e.Repo, &raw, &e.SourceIP, &e.Result); err != nil {
			return nil, err
		}
		e.Detail = map[string]any{}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteExpiredAudit prunes audit rows older than the retention window. This is
// the only deletion path for the append-only table; wired into the scheduler's
// retention GC.
func (s *Store) DeleteExpiredAudit(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM audit_log WHERE ts < now() - $1::interval`, olderThan.String())
	return tag.RowsAffected(), err
}
