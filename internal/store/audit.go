package store

import (
	"context"
	"encoding/json"
	"time"
)

// AuditEntry is one row of the append-only audit trail. Detail carries only
// safe metadata — never secret values, tokens or client secrets.
type AuditEntry struct {
	ID int64     `json:"id"`
	TS time.Time `json:"ts"`
	// CreatedAt is TS under the name every other resource uses for its
	// timestamp, so generic clients don't read a missing field as null.
	CreatedAt time.Time      `json:"created_at"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Repo      string         `json:"repo"`
	Detail    map[string]any `json:"detail"`
	SourceIP  string         `json:"source_ip"`
	Result    string         `json:"result"`
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
// the caller; empty Repo/Actor/Target/Action mean "no filter". Action matches
// a prefix ("runner." → every runner action).
type AuditFilter struct {
	Repo   string
	Actor  string
	Target string
	Action string
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
		   AND ($5 = '' OR target = $5)
		   AND ($6 = '' OR starts_with(action, $6))
		 ORDER BY id DESC
		 LIMIT $3 OFFSET $4`,
		f.Repo, f.Actor, f.Limit, f.Offset, f.Target, f.Action)
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
		e.CreatedAt = e.TS
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

// eventActions are the audit actions the runners page draws as events:
// runner operations, maintenance windows and Forge deploys.
var eventActions = []string{"runner.", "maintenance.", "server.upgrade"}

// ListEvents returns the runner, maintenance and deploy audit rows in
// [from, to), oldest first — chart annotations and a runner's timeline.
// A non-empty target narrows to one runner.
func (s *Store) ListEvents(ctx context.Context, from, to time.Time, target string, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, ts, actor, action, target, repo, detail, source_ip, result
		 FROM audit_log
		 WHERE ts >= $1 AND ts < $2
		   AND EXISTS (SELECT 1 FROM unnest($3::text[]) p WHERE starts_with(action, p))
		   AND ($4 = '' OR target = $4)
		 ORDER BY ts DESC, id DESC
		 LIMIT $5`, from, to, eventActions, target, limit)
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
		e.CreatedAt = e.TS
		e.Detail = map[string]any{}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	// Newest first in the query so the limit keeps the latest; oldest first out.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
