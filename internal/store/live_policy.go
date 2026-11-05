package store

import (
	"context"
	"encoding/json"
	"strings"
)

// Forge Live sync policies (phase C): which applications Forge keeps in
// sync with Git by itself. The controller lives in internal/live/hub.

// SetLivePolicy saves app's sync policy and tells live pages.
func (s *Store) SetLivePolicy(ctx context.Context, appID int64, p LivePolicy, by string) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO live_app_policies (target_id, app_key, auto_sync, prune, self_heal, status, updated_by, updated_at)
		SELECT target_id, key, $2, $3, $4, $5, $6, now() FROM live_apps WHERE id = $1
		ON CONFLICT (target_id, app_key) DO UPDATE SET auto_sync = $2, prune = $3, self_heal = $4, status = $5,
			updated_by = $6, updated_at = now()`, appID, p.AutoSync, p.Prune, p.SelfHeal, p.Status, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.notifyApp(ctx, appID)
}

// SetLivePolicyStatus records what auto-sync is doing for an app, telling
// live pages only when it changed.
func (s *Store) SetLivePolicyStatus(ctx context.Context, appID int64, status string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE live_app_policies p SET status = $2 FROM live_apps a
		WHERE a.id = $1 AND p.target_id = a.target_id AND p.app_key = a.key AND p.status <> $2`, appID, status)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return s.notifyApp(ctx, appID)
}

func (s *Store) notifyApp(ctx context.Context, appID int64) error {
	b, _ := json.Marshal(liveNote{ID: appID, Summary: true})
	_, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, liveChannel, string(b))
	return err
}

// AutoSyncCandidates are the applications auto-sync may have to act on: on
// with auto-sync, out of sync, on a target whose agent can sync and has
// reported in full.
func (s *Store) AutoSyncCandidates(ctx context.Context, limit int) ([]LiveApp, error) {
	rows, err := s.pool.Query(ctx, liveAppSelect+` WHERE p.auto_sync AND a.sync = 'OutOfSync' AND NOT a.orphans
		AND t.synced AND (t.capabilities->>'sync')::boolean ORDER BY a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveApp
	for rows.Next() {
		a, err := scanLiveAppRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// LiveChangeGate says what stands between a change and an app's
// environment right now: a freeze, and how many approvals it needs.
func (s *Store) LiveChangeGate(ctx context.Context, repo, env string) (frozen bool, approvals int, err error) {
	if env == "" {
		return false, 0, nil
	}
	if frozen, err = s.IsFrozen(ctx, repo, env); err != nil {
		return false, 0, err
	}
	rule, err := s.resolveProtectedEnv(ctx, repo, env)
	if err != nil || rule == nil {
		return frozen, 0, err
	}
	return frozen, rule.RequiredApprovals, nil
}

// LiveOrphans returns the orphaned resources' row of app's namespace
// (ErrNotFound when its agent reports none).
func (s *Store) LiveOrphans(ctx context.Context, appID int64) (*LiveApp, error) {
	return scanLiveApp(s.pool.QueryRow(ctx, `SELECT `+liveAppCols+` FROM live_apps a
		JOIN live_apps x ON x.target_id = a.target_id AND a.key = x.namespace || '/'
		WHERE x.id = $1 AND a.orphans`, appID))
}

// DeployedImage is the image the latest Forge deploy pushed: the running
// image's repository at the deploy record's digest ("" when there is none).
func (a *LiveApp) DeployedImage() string {
	if a.Deployed == nil || a.Deployed.Digest == "" || len(a.Images) == 0 {
		return ""
	}
	repo := a.Images[0]
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + "@" + a.Deployed.Digest
}
