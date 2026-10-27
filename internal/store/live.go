package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Forge Live (batch 9): targets (where applications run, each reported by
// one agent), their applications' indexable status, events, and the blob
// keys awaiting collection. The resource trees and manifests themselves are
// blobs (internal/live/hub).

// LiveTarget is a cluster or cloud project reported by one forge-agent.
type LiveTarget struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Environment  string            `json:"environment"`
	Repo         string            `json:"repo"`
	Provider     string            `json:"provider"`
	Capabilities live.Capabilities `json:"capabilities"`
	AgentVersion string            `json:"agent_version"`
	Epoch        string            `json:"-"`
	Seq          int64             `json:"-"`
	Synced       bool              `json:"synced"`
	Stats        live.AgentStats   `json:"stats"`
	BytesIn      int64             `json:"bytes_in"`
	LastSeenAt   *time.Time        `json:"last_seen_at"`
	CreatedBy    string            `json:"created_by"`
	CreatedAt    time.Time         `json:"created_at"`
	TokenSuffix  string            `json:"token_suffix"`
	Apps         int               `json:"apps"`
}

// LiveDeploy is the latest successful Forge deployment to an app's
// environment from its repo: the revision the app runs, by whom, when.
type LiveDeploy struct {
	SHA        string    `json:"sha"`
	By         string    `json:"by"`
	At         time.Time `json:"at"`
	JobID      int64     `json:"job_id"`
	PipelineID int64     `json:"pipeline_id"`
	Digest     string    `json:"digest,omitempty"` // the image digest the deploy reported
	Message    string    `json:"message,omitempty"`
}

// LiveApp is one application's indexable status.
type LiveApp struct {
	ID            int64     `json:"id"`
	TargetID      int64     `json:"target_id"`
	Key           string    `json:"key"`
	Name          string    `json:"name"`
	Namespace     string    `json:"namespace"`
	Group         string    `json:"group"`
	Health        string    `json:"health"`
	HealthMessage string    `json:"health_message"`
	HealthSince   time.Time `json:"health_since"`
	Sync          string    `json:"sync"`
	Resources     int       `json:"resources"`
	Pods          int       `json:"pods"`
	PodsReady     int       `json:"pods_ready"`
	Images        []string  `json:"images"`
	Tree          string    `json:"-"`
	Version       int64     `json:"version"`
	Epoch         string    `json:"-"`
	UpdatedAt     time.Time `json:"updated_at"`
	// The desired state's revision and where it was rendered from.
	Revision        string      `json:"revision"`
	RevisionMessage string      `json:"revision_message"`
	RevisionAuthor  string      `json:"revision_author"`
	RevisionAt      *time.Time  `json:"revision_at"`
	SourcePath      string      `json:"source_path"`
	SourceTool      string      `json:"source_tool"`
	SourceError     string      `json:"source_error"`
	DesiredKeys     []string    `json:"-"`
	Target          string      `json:"target"`
	Environment     string      `json:"environment"`
	Repo            string      `json:"repo"`
	Provider        string      `json:"provider"`
	Deployed        *LiveDeploy `json:"deployed"`
	// Drifted and Prunable count the resources behind an OutOfSync.
	Drifted  int         `json:"drifted"`
	Prunable int         `json:"prunable"`
	Orphans  bool        `json:"-"` // a namespace's orphaned resources, not an app
	Policy   *LivePolicy `json:"policy"`
}

// LivePolicy is how Forge keeps an application in sync with Git.
type LivePolicy struct {
	AutoSync bool `json:"auto_sync"` // sync each new revision
	Prune    bool `json:"prune"`     // auto-syncs prune what Git dropped
	SelfHeal bool `json:"self_heal"` // and undo live drift
	// Status: what auto-sync is doing, or why it waits.
	Status    string     `json:"status"`
	UpdatedBy string     `json:"updated_by"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// SetStatus applies an agent's status; health_since moves only when the
// health does.
func (a *LiveApp) SetStatus(st live.Status, now time.Time) {
	if a.Health != st.Health.Status {
		a.HealthSince = now
	}
	a.Health, a.HealthMessage = st.Health.Status, st.Health.Message
	a.Resources, a.Pods, a.PodsReady, a.Images = st.Resources, st.Pods, st.PodsReady, st.Images
	a.Drifted, a.Prunable = st.Drifted, st.Prunable
	if st.Sync != "" {
		a.Sync = st.Sync
	} else {
		a.Sync = "Unknown"
	}
}

func newLiveToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "fla_" + hex.EncodeToString(raw), nil
}

// CreateLiveTarget registers a target and returns its agent token — the only
// time the raw token exists; Forge keeps its hash.
func (s *Store) CreateLiveTarget(ctx context.Context, name, environment, repo, by string) (*LiveTarget, string, error) {
	tok, err := newLiveToken()
	if err != nil {
		return nil, "", err
	}
	t := LiveTarget{Name: name, Environment: environment, Repo: repo, CreatedBy: by, TokenSuffix: tok[len(tok)-4:]}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO live_targets (name, environment, repo, token_hash, token_suffix, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
		name, environment, repo, hashAPIToken(tok), t.TokenSuffix, by).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "live_targets_name_key") {
			return nil, "", fmt.Errorf("a target named %q exists", name)
		}
		return nil, "", err
	}
	return &t, tok, nil
}

const liveTargetCols = `t.id, t.name, t.environment, t.repo, t.provider, t.capabilities, t.agent_version, t.epoch, t.seq,
	t.synced, t.stats, t.bytes_in, t.last_seen_at, t.created_by, t.created_at, t.token_suffix`

func scanLiveTarget(row pgx.Row, extra ...any) (*LiveTarget, error) {
	var t LiveTarget
	var caps, stats []byte
	dest := append([]any{&t.ID, &t.Name, &t.Environment, &t.Repo, &t.Provider, &caps, &t.AgentVersion, &t.Epoch, &t.Seq,
		&t.Synced, &stats, &t.BytesIn, &t.LastSeenAt, &t.CreatedBy, &t.CreatedAt, &t.TokenSuffix}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(caps, &t.Capabilities)
	_ = json.Unmarshal(stats, &t.Stats)
	return &t, nil
}

// LiveTargetByToken resolves an agent token (ErrNotFound when unknown).
func (s *Store) LiveTargetByToken(ctx context.Context, token string) (*LiveTarget, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return scanLiveTarget(s.pool.QueryRow(ctx, `SELECT `+liveTargetCols+` FROM live_targets t WHERE t.token_hash = $1`, hashAPIToken(token)))
}

// LiveTargets lists every target with its application count.
func (s *Store) LiveTargets(ctx context.Context) ([]LiveTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+liveTargetCols+`, (SELECT count(*) FROM live_apps a WHERE a.target_id = t.id AND NOT a.orphans)
		FROM live_targets t ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveTarget{}
	for rows.Next() {
		var n int
		t, err := scanLiveTarget(rows, &n)
		if err != nil {
			return nil, err
		}
		t.Apps = n
		out = append(out, *t)
	}
	return out, rows.Err()
}

// LiveBlobs are an app's blob references: its tree and its desired keys.
type LiveBlobs struct {
	Tree    string
	Desired []string
}

// LiveTargetTrees returns the blob references of a target's applications by
// app id (to collect their blobs before the target goes).
func (s *Store) LiveTargetTrees(ctx context.Context, targetID int64) (map[int64]LiveBlobs, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, tree, desired_keys FROM live_apps WHERE target_id = $1`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]LiveBlobs{}
	for rows.Next() {
		var id int64
		var b LiveBlobs
		if err := rows.Scan(&id, &b.Tree, &b.Desired); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// DeleteLiveTarget removes a target and its applications; blobs named in
// collect are queued for deletion in the same transaction.
func (s *Store) DeleteLiveTarget(ctx context.Context, id int64, collect []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM live_apps WHERE target_id = $1`, id)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM live_targets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := collectBlobs(ctx, tx, collect); err != nil {
		return err
	}
	for _, app := range ids {
		if err := notifyLive(ctx, tx, liveNote{ID: app, Gone: true}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---- ingest ----

// liveChannel is the LISTEN/NOTIFY channel live pages subscribe to.
const liveChannel = "forge_live"

// liveNote is a notification's payload: an app changed (to version V; its
// summary row too when Summary), is gone, or one of its operations moved.
type liveNote struct {
	ID      int64 `json:"id"`
	V       int64 `json:"v,omitempty"`
	Summary bool  `json:"s,omitempty"`
	Gone    bool  `json:"gone,omitempty"`
	Op      int64 `json:"op,omitempty"` // an operation of the app changed
}

func notifyLive(ctx context.Context, tx pgx.Tx, n liveNote) error {
	b, _ := json.Marshal(n)
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, liveChannel, string(b))
	return err
}

// LiveBlobGrace is how long an unreferenced blob is kept.
const LiveBlobGrace = 10 * time.Minute

func collectBlobs(ctx context.Context, tx pgx.Tx, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO live_blob_gc (key, due_at) SELECT unnest($1::text[]), now() + $2::interval
		ON CONFLICT (key) DO NOTHING`, keys, fmt.Sprintf("%d seconds", int(LiveBlobGrace.Seconds())))
	return err
}

// LiveTx is one agent batch being applied, with its target row locked.
type LiveTx struct {
	tx     pgx.Tx
	Target *LiveTarget
}

// LiveIngest runs fn in a transaction holding target's row lock (one batch
// at a time per target) and saves the target as fn left it.
func (s *Store) LiveIngest(ctx context.Context, targetID int64, fn func(*LiveTx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	t, err := scanLiveTarget(tx.QueryRow(ctx, `SELECT `+liveTargetCols+` FROM live_targets t WHERE t.id = $1 FOR UPDATE`, targetID))
	if err != nil {
		return err
	}
	ltx := &LiveTx{tx: tx, Target: t}
	if err := fn(ltx); err != nil {
		return err
	}
	caps, _ := json.Marshal(t.Capabilities)
	stats, _ := json.Marshal(t.Stats)
	if _, err := tx.Exec(ctx, `UPDATE live_targets SET provider=$2, capabilities=$3, agent_version=$4, epoch=$5, seq=$6,
		synced=$7, stats=$8, bytes_in=$9, last_seen_at=now() WHERE id=$1`,
		t.ID, t.Provider, caps, t.AgentVersion, t.Epoch, t.Seq, t.Synced, stats, t.BytesIn); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const liveAppCols = `a.id, a.target_id, a.key, a.name, a.namespace, a.app_group, a.health, a.health_message, a.health_since,
	a.sync, a.resources, a.pods, a.pods_ready, a.images, a.tree, a.version, a.epoch, a.updated_at,
	a.revision, a.revision_message, a.revision_author, a.revision_at, a.source_path, a.source_tool, a.source_error, a.desired_keys,
	a.orphans, a.drifted, a.prunable`

func scanLiveApp(row pgx.Row, extra ...any) (*LiveApp, error) {
	var a LiveApp
	dest := append([]any{&a.ID, &a.TargetID, &a.Key, &a.Name, &a.Namespace, &a.Group, &a.Health, &a.HealthMessage, &a.HealthSince,
		&a.Sync, &a.Resources, &a.Pods, &a.PodsReady, &a.Images, &a.Tree, &a.Version, &a.Epoch, &a.UpdatedAt,
		&a.Revision, &a.RevisionMessage, &a.RevisionAuthor, &a.RevisionAt, &a.SourcePath, &a.SourceTool, &a.SourceError, &a.DesiredKeys,
		&a.Orphans, &a.Drifted, &a.Prunable}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

// App returns the target's app with key, locked (nil when there is none).
func (l *LiveTx) App(ctx context.Context, key string) (*LiveApp, error) {
	a, err := scanLiveApp(l.tx.QueryRow(ctx, `SELECT `+liveAppCols+` FROM live_apps a WHERE a.target_id=$1 AND a.key=$2 FOR UPDATE`, l.Target.ID, key))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return a, err
}

// StaleApps are the target's apps the current epoch never mentioned.
func (l *LiveTx) StaleApps(ctx context.Context) ([]LiveApp, error) {
	rows, err := l.tx.Query(ctx, `SELECT `+liveAppCols+` FROM live_apps a WHERE a.target_id=$1 AND a.epoch <> $2 FOR UPDATE`, l.Target.ID, l.Target.Epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveApp
	for rows.Next() {
		a, err := scanLiveApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// SaveApp inserts or updates a (its ID is set on insert) and tells live
// pages; summary says whether what the list shows changed too.
func (l *LiveTx) SaveApp(ctx context.Context, a *LiveApp, summary bool) error {
	if a.Images == nil {
		a.Images = []string{}
	}
	if a.DesiredKeys == nil {
		a.DesiredKeys = []string{}
	}
	if a.Sync == "" {
		a.Sync = "Unknown"
	}
	var err error
	if a.ID == 0 {
		err = l.tx.QueryRow(ctx, `INSERT INTO live_apps (target_id, key, name, namespace, app_group, health, health_message, health_since,
			resources, pods, pods_ready, images, tree, version, epoch, orphans)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id, updated_at`,
			l.Target.ID, a.Key, a.Name, a.Namespace, a.Group, a.Health, a.HealthMessage, a.HealthSince,
			a.Resources, a.Pods, a.PodsReady, a.Images, a.Tree, a.Version, a.Epoch, a.Orphans).Scan(&a.ID, &a.UpdatedAt)
		summary = true
	} else {
		err = l.tx.QueryRow(ctx, `UPDATE live_apps SET name=$2, namespace=$3, app_group=$4, health=$5, health_message=$6, health_since=$7,
			resources=$8, pods=$9, pods_ready=$10, images=$11, tree=$12, version=$13, epoch=$14, sync=$15,
			revision=$16, revision_message=$17, revision_author=$18, revision_at=$19, source_path=$20, source_tool=$21,
			source_error=$22, desired_keys=$23, drifted=$24, prunable=$25, updated_at=now()
			WHERE id=$1 RETURNING updated_at`,
			a.ID, a.Name, a.Namespace, a.Group, a.Health, a.HealthMessage, a.HealthSince,
			a.Resources, a.Pods, a.PodsReady, a.Images, a.Tree, a.Version, a.Epoch, a.Sync,
			a.Revision, a.RevisionMessage, a.RevisionAuthor, a.RevisionAt, a.SourcePath, a.SourceTool,
			a.SourceError, a.DesiredKeys, a.Drifted, a.Prunable).Scan(&a.UpdatedAt)
	}
	if err != nil {
		return err
	}
	// An orphans row is never on the Applications page: only a page showing
	// it (subscribed to its patches) hears about it.
	return notifyLive(ctx, l.tx, liveNote{ID: a.ID, V: a.Version, Summary: summary && !a.Orphans})
}

// DeleteApp removes an application (its events go with it).
func (l *LiveTx) DeleteApp(ctx context.Context, id int64) error {
	if _, err := l.tx.Exec(ctx, `DELETE FROM live_apps WHERE id=$1`, id); err != nil {
		return err
	}
	return notifyLive(ctx, l.tx, liveNote{ID: id, Gone: true})
}

// liveEventsKept is how many events each app keeps.
const liveEventsKept = 200

// UpsertEvents records events (by uid: a repeated event updates its count
// and last seen) and keeps the app's newest liveEventsKept.
func (l *LiveTx) UpsertEvents(ctx context.Context, appID int64, events []live.Event) error {
	if len(events) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, e := range events {
		b.Queue(`INSERT INTO live_app_events (app_id, uid, resource_uid, kind, name, type, reason, message, count, first_seen, last_seen)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (app_id, uid) DO UPDATE SET count=EXCLUDED.count, last_seen=EXCLUDED.last_seen, message=EXCLUDED.message`,
			appID, e.UID, e.ResourceUID, e.Kind, e.Name, e.Type, e.Reason, e.Message, e.Count, e.FirstSeen, e.LastSeen)
	}
	b.Queue(`DELETE FROM live_app_events WHERE app_id=$1 AND uid IN (
		SELECT uid FROM live_app_events WHERE app_id=$1 ORDER BY last_seen DESC OFFSET $2)`, appID, liveEventsKept)
	return l.tx.SendBatch(ctx, b).Close()
}

// Collect queues blobs nothing references any more.
func (l *LiveTx) Collect(ctx context.Context, keys ...string) error {
	return collectBlobs(ctx, l.tx, keys)
}

// TakeLiveGarbage claims up to limit blob keys past their grace period.
func (s *Store) TakeLiveGarbage(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `DELETE FROM live_blob_gc WHERE key IN (
		SELECT key FROM live_blob_gc WHERE due_at < now() ORDER BY due_at LIMIT $1 FOR UPDATE SKIP LOCKED) RETURNING key`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- reads (pages: Postgres only) ----

// LiveAppQuery filters the Applications page. Each list holds the values
// allowed for its dimension (empty: any).
type LiveAppQuery struct {
	Health, Sync, Environment, Repo, Target, Namespace, Group []string
	Search                                                    string
	Limit, Offset                                             int
}

// liveDims maps each facet to its column.
var liveDims = []struct{ name, col string }{
	{"health", "a.health"}, {"sync", "a.sync"}, {"environment", "t.environment"}, {"repo", "t.repo"},
	{"target", "t.name"}, {"namespace", "a.namespace"}, {"group", "a.app_group"},
}

func (q LiveAppQuery) values(dim string) []string {
	return map[string][]string{"health": q.Health, "sync": q.Sync, "environment": q.Environment, "repo": q.Repo,
		"target": q.Target, "namespace": q.Namespace, "group": q.Group}[dim]
}

// where builds the filter, leaving out dimension skip (a facet's counts
// ignore the facet's own selection, so picking Degraded still shows how
// many are Healthy).
func (q LiveAppQuery) where(skip string) (string, []any) {
	conds, args := []string{"NOT a.orphans"}, []any{}
	for _, d := range liveDims {
		if vs := q.values(d.name); len(vs) > 0 && d.name != skip {
			args = append(args, vs)
			conds = append(conds, fmt.Sprintf("%s = ANY($%d)", d.col, len(args)))
		}
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		args = append(args, escapeLike(s))
		n := len(args)
		conds = append(conds, fmt.Sprintf("(a.name ILIKE '%%' || $%d || '%%' OR a.namespace ILIKE '%%' || $%d || '%%' OR a.app_group ILIKE '%%' || $%d || '%%')", n, n, n))
	}
	return strings.Join(conds, " AND "), args
}

const liveAppFrom = ` FROM live_apps a JOIN live_targets t ON t.id = a.target_id`

// liveDeployJoin finds the latest successful deployment of the target's
// repo to its environment (deployments_repo_env_id_idx).
const liveDeployJoin = ` LEFT JOIN LATERAL (
	SELECT d.sha, d.deployed_by, d.deployed_at, d.job_id, d.pipeline_id, d.digest FROM deployments d
	WHERE t.repo <> '' AND d.repo = t.repo AND d.environment = t.environment AND d.status = 'success'
	ORDER BY d.id DESC LIMIT 1) dep ON TRUE`

const liveAppSelect = `SELECT ` + liveAppCols + `, t.name, t.environment, t.repo, t.provider,
	dep.sha, dep.deployed_by, dep.deployed_at, dep.job_id, dep.pipeline_id, dep.digest,
	p.auto_sync, p.prune, p.self_heal, p.status, p.updated_by, p.updated_at` + liveAppFrom + liveDeployJoin +
	` LEFT JOIN live_app_policies p ON p.target_id = a.target_id AND p.app_key = a.key`

func scanLiveAppRow(row pgx.Row) (*LiveApp, error) {
	var sha, by, digest *string
	var at *time.Time
	var job, pipe *int64
	var target, env, repo, provider string
	var auto, prune, heal *bool
	var status, pby *string
	var pat *time.Time
	a, err := scanLiveApp(row, &target, &env, &repo, &provider, &sha, &by, &at, &job, &pipe, &digest,
		&auto, &prune, &heal, &status, &pby, &pat)
	if err != nil {
		return nil, err
	}
	a.Policy = &LivePolicy{}
	if auto != nil {
		a.Policy = &LivePolicy{AutoSync: *auto, Prune: *prune, SelfHeal: *heal, Status: *status, UpdatedBy: *pby, UpdatedAt: pat}
	}
	a.Target, a.Environment, a.Repo, a.Provider = target, env, repo, provider
	if sha != nil {
		a.Deployed = &LiveDeploy{SHA: *sha, By: *by, At: *at, JobID: *job, PipelineID: *pipe, Digest: *digest}
	}
	return a, nil
}

// LatestDeployDigest is the image digest of the latest successful deploy of
// repo to env ("" when none reported one).
func (s *Store) LatestDeployDigest(ctx context.Context, repo, env string) (string, error) {
	var d string
	err := s.pool.QueryRow(ctx, `SELECT digest FROM deployments WHERE repo=$1 AND environment=$2 AND status='success'
		ORDER BY id DESC LIMIT 1`, repo, env).Scan(&d)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return d, err
}

// LiveFacets counts apps per value of each dimension.
type LiveFacets map[string]map[string]int

// ListLiveApps returns one page of applications by name, the total matching,
// and the facet counts.
func (s *Store) ListLiveApps(ctx context.Context, q LiveAppQuery) ([]LiveApp, int, LiveFacets, error) {
	where, args := q.where("")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+liveAppFrom+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, nil, err
	}
	rows, err := s.pool.Query(ctx, liveAppSelect+` WHERE `+where+
		fmt.Sprintf(` ORDER BY a.name, a.namespace, a.id LIMIT %d OFFSET %d`, q.Limit, q.Offset), args...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	apps := []LiveApp{}
	for rows.Next() {
		a, err := scanLiveAppRow(rows)
		if err != nil {
			return nil, 0, nil, err
		}
		apps = append(apps, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	facets := LiveFacets{}
	for _, d := range liveDims {
		where, args := q.where(d.name)
		rows, err := s.pool.Query(ctx, `SELECT `+d.col+`, count(*)`+liveAppFrom+` WHERE `+where+` GROUP BY 1`, args...)
		if err != nil {
			return nil, 0, nil, err
		}
		counts := map[string]int{}
		for rows.Next() {
			var v string
			var n int
			if err := rows.Scan(&v, &n); err != nil {
				rows.Close()
				return nil, 0, nil, err
			}
			counts[v] = n
		}
		rows.Close()
		facets[d.name] = counts
	}
	return apps, total, facets, nil
}

// LiveApp returns one application with its latest deployment (and that
// deployment's commit message).
func (s *Store) LiveApp(ctx context.Context, id int64) (*LiveApp, error) {
	a, err := scanLiveAppRow(s.pool.QueryRow(ctx, liveAppSelect+` WHERE a.id = $1`, id))
	if err != nil || a.Deployed == nil {
		return a, err
	}
	_ = s.pool.QueryRow(ctx, `SELECT commit_message FROM pipelines WHERE id = $1`, a.Deployed.PipelineID).Scan(&a.Deployed.Message)
	return a, nil
}

// LiveAppTarget returns an app's target (its capabilities, its agent).
func (s *Store) LiveAppTarget(ctx context.Context, appID int64) (*LiveTarget, error) {
	return scanLiveTarget(s.pool.QueryRow(ctx, `SELECT `+liveTargetCols+` FROM live_targets t JOIN live_apps a ON a.target_id = t.id WHERE a.id = $1`, appID))
}

// LiveAppEvents returns an app's newest events, or one resource's.
func (s *Store) LiveAppEvents(ctx context.Context, appID int64, resourceUID string) ([]live.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT uid, resource_uid, kind, name, type, reason, message, count, first_seen, last_seen
		FROM live_app_events WHERE app_id = $1 AND ($2 = '' OR resource_uid = $2) ORDER BY last_seen DESC LIMIT 200`, appID, resourceUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []live.Event{}
	for rows.Next() {
		var e live.Event
		if err := rows.Scan(&e.UID, &e.ResourceUID, &e.Kind, &e.Name, &e.Type, &e.Reason, &e.Message, &e.Count, &e.FirstSeen, &e.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LiveSummaries returns repo → environment → summary for the given repos
// (all repos when nil).
func (s *Store) LiveSummaries(ctx context.Context, repos []string) (map[string]map[string]*proto.LiveSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.repo, t.environment, a.health, count(*)`+liveAppFrom+`
		WHERE t.repo <> '' AND NOT a.orphans AND ($1::text[] IS NULL OR t.repo = ANY($1)) GROUP BY 1, 2, 3`, repos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]*proto.LiveSummary{}
	for rows.Next() {
		var repo, env, health string
		var n int
		if err := rows.Scan(&repo, &env, &health, &n); err != nil {
			return nil, err
		}
		if out[repo] == nil {
			out[repo] = map[string]*proto.LiveSummary{}
		}
		sum := out[repo][env]
		if sum == nil {
			sum = &proto.LiveSummary{Health: live.Healthy, Counts: map[string]int{}}
			out[repo][env] = sum
		}
		sum.Apps += n
		sum.Counts[health] += n
		if live.Worse(health, sum.Health) {
			sum.Health = health
		}
	}
	return out, rows.Err()
}

// LiveHealthCounts counts apps per health (for metrics).
func (s *Store) LiveHealthCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT health, count(*) FROM live_apps WHERE NOT orphans GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var h string
		var n int
		if err := rows.Scan(&h, &n); err != nil {
			return nil, err
		}
		out[h] = n
	}
	return out, rows.Err()
}

// ListenLive calls fn with each live notification's payload until ctx ends,
// on one pooled connection; the caller fans out.
func (s *Store) ListenLive(ctx context.Context, fn func(payload string)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+liveChannel); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), "UNLISTEN "+liveChannel) }()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
