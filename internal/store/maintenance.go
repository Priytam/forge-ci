package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Maintenance pauses job acquisition on every runner.
type Maintenance struct {
	On      bool      `json:"on"`
	Message string    `json:"message,omitempty"`
	By      string    `json:"by,omitempty"`
	Since   time.Time `json:"since,omitempty"`
}

const maintenanceKey = "maintenance"

func (s *Store) Maintenance(ctx context.Context) (Maintenance, error) {
	var raw []byte
	var m Maintenance
	err := s.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key=$1`, maintenanceKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(raw, &m)
}

func (s *Store) SetMaintenance(ctx context.Context, m Maintenance) (Maintenance, error) {
	if m.On {
		m.Since = time.Now()
	} else {
		m = Maintenance{}
	}
	raw, _ := json.Marshal(m)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, maintenanceKey, raw)
	return m, err
}

// SchemaStatus says when the embedded migrations last ran to completion.
type SchemaStatus struct {
	AppliedAt time.Time `json:"applied_at"`
	Version   string    `json:"version"` // server version that applied them
}

func (s *Store) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	var raw []byte
	var st SchemaStatus
	err := s.pool.QueryRow(ctx, `SELECT value FROM platform_settings WHERE key='schema_status'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// recordSchemaStatus notes the version that applied the migrations; a start
// on a different version than the last is a deploy, kept in the audit log so
// charts can mark it.
func (s *Store) recordSchemaStatus(ctx context.Context, version string) {
	if prev, err := s.SchemaStatus(ctx); err == nil && prev.Version != "" && prev.Version != version {
		_ = s.InsertAudit(ctx, AuditEntry{Actor: "system", Action: "server.upgrade", Target: version,
			Detail: map[string]any{"from": prev.Version}})
	}
	raw, _ := json.Marshal(SchemaStatus{AppliedAt: time.Now(), Version: version})
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ('schema_status',$1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, raw)
}

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
