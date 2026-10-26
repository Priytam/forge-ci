package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Code review (batch 7). Changes, revisions and reviews are written from
// webhooks and refreshes and read by pages; the read side here touches
// Postgres only, never a provider.

// ChangeSummary is one inbox row.
type ChangeSummary struct {
	ID             int64      `json:"id"`
	Repo           string     `json:"repo"`
	Provider       string     `json:"provider"`
	Number         string     `json:"number"`
	Title          string     `json:"title"`
	Author         string     `json:"author"`
	SourceBranch   string     `json:"source_branch"`
	TargetBranch   string     `json:"target_branch"`
	State          string     `json:"state"`
	Draft          bool       `json:"draft"`
	HeadSHA        string     `json:"head_sha"`
	BaseSHA        string     `json:"base_sha"`
	Labels         []string   `json:"labels"`
	URL            string     `json:"url,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	SyncedAt       *time.Time `json:"synced_at,omitempty"`
	Revisions      int        `json:"revisions"`
	Approvals      int        `json:"approvals"`
	ChangesWanted  int        `json:"changes_requested"`
	PipelineID     *int64     `json:"pipeline_id,omitempty"`
	PipelineStatus string     `json:"pipeline_status,omitempty"`
	// GateState is the merge gate on the head (pass | pending | fail); empty
	// when no merge rule applies.
	GateState   string `json:"gate_state,omitempty"`
	GateSummary string `json:"gate_summary,omitempty"`
}

// ChangeRevision is one push to a change.
type ChangeRevision struct {
	Number   int       `json:"number"`
	HeadSHA  string    `json:"head_sha"`
	BaseSHA  string    `json:"base_sha"`
	PushedAt time.Time `json:"pushed_at"`
}

// ChangeReview is one verdict, from the host or given in Forge.
type ChangeReview struct {
	ID          int64     `json:"id"`
	Source      string    `json:"source"` // host | forge
	Reviewer    string    `json:"reviewer"`
	Verdict     string    `json:"verdict"`
	Body        string    `json:"body"`
	CommitSHA   string    `json:"commit_sha,omitempty"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ChangeDetail is the change page.
type ChangeDetail struct {
	ChangeSummary
	Body         string           `json:"body"`
	RefreshError string           `json:"refresh_error,omitempty"`
	RevisionList []ChangeRevision `json:"revision_list"`
	Reviews      []ChangeReview   `json:"reviews"`
}

// ChangeQuery filters the inbox. State is open (default), merged, closed or
// all.
type ChangeQuery struct {
	State  string
	Repo   string
	Author string
	Search string
	Limit  int
	Offset int
}

// changeSummaryCols selects a ChangeSummary from changes c. Approvals count
// each reviewer's latest decisive verdict; CI is the newest pipeline on the
// head commit (pipelines.status is maintained by trigger).
const changeSummaryCols = `
	c.id, c.repo, c.provider, c.number, c.title, c.author, c.source_branch, c.target_branch,
	c.state, c.draft, c.head_sha, c.base_sha, c.labels, c.url, c.created_at, c.updated_at, c.synced_at,
	(SELECT count(*) FROM change_revisions r WHERE r.change_id = c.id),
	COALESCE(v.approvals, 0), COALESCE(v.wanted, 0), p.id, COALESCE(p.status, ''),
	COALESCE(gr.state, ''), COALESCE(gr.summary, '')`

const changeSummaryJoins = `
	LEFT JOIN LATERAL (
	    SELECT count(*) FILTER (WHERE verdict = 'approve') AS approvals,
	           count(*) FILTER (WHERE verdict = 'request_changes') AS wanted
	    FROM (SELECT DISTINCT ON (lower(reviewer)) verdict FROM change_reviews
	          WHERE change_id = c.id AND verdict <> 'comment'
	          ORDER BY lower(reviewer), submitted_at DESC) latest
	) v ON TRUE
	LEFT JOIN LATERAL (
	    SELECT id, status FROM pipelines WHERE repo = c.repo AND sha = c.head_sha AND c.head_sha <> ''
	    ORDER BY id DESC LIMIT 1
	) p ON TRUE
	LEFT JOIN gate_results gr ON gr.change_id = c.id AND gr.head_sha = c.head_sha`

func scanChangeSummary(row pgx.Row, c *ChangeSummary) error {
	return row.Scan(&c.ID, &c.Repo, &c.Provider, &c.Number, &c.Title, &c.Author, &c.SourceBranch,
		&c.TargetBranch, &c.State, &c.Draft, &c.HeadSHA, &c.BaseSHA, &c.Labels, &c.URL,
		&c.CreatedAt, &c.UpdatedAt, &c.SyncedAt, &c.Revisions, &c.Approvals, &c.ChangesWanted,
		&c.PipelineID, &c.PipelineStatus, &c.GateState, &c.GateSummary)
}

// ListChanges is the inbox: one indexed query across every repo and host.
// Placeholder rows (a webhook arrived, the first refresh hasn't) are hidden.
func (s *Store) ListChanges(ctx context.Context, q ChangeQuery) ([]ChangeSummary, int, error) {
	where := []string{"c.synced_at IS NOT NULL"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	switch q.State {
	case "", "open":
		where = append(where, "c.state = 'open'")
	case "merged", "closed":
		where = append(where, "c.state = "+arg(q.State))
	case "all":
	default:
		return nil, 0, fmt.Errorf("state must be open, merged, closed or all")
	}
	if q.Repo != "" {
		where = append(where, "c.repo = "+arg(q.Repo))
	}
	if q.Author != "" {
		where = append(where, "lower(c.author) = lower("+arg(q.Author)+")")
	}
	if q.Search != "" {
		where = append(where, "(c.title ILIKE "+arg("%"+q.Search+"%")+" OR c.number = "+arg(strings.TrimPrefix(q.Search, "#"))+")")
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM changes c WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+changeSummaryCols+` FROM changes c `+changeSummaryJoins+`
		 WHERE `+cond+` ORDER BY c.updated_at DESC NULLS LAST, c.id DESC
		 LIMIT `+arg(limit)+` OFFSET `+arg(q.Offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []ChangeSummary{}
	for rows.Next() {
		var c ChangeSummary
		if err := scanChangeSummary(rows, &c); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// GetChangeDetail returns one change with its revisions and reviews.
func (s *Store) GetChangeDetail(ctx context.Context, id int64) (*ChangeDetail, error) {
	d := &ChangeDetail{RevisionList: []ChangeRevision{}, Reviews: []ChangeReview{}}
	row := s.pool.QueryRow(ctx,
		`SELECT `+changeSummaryCols+`, c.body, c.refresh_error FROM changes c `+changeSummaryJoins+`
		 WHERE c.id = $1 AND c.synced_at IS NOT NULL`, id)
	err := row.Scan(&d.ID, &d.Repo, &d.Provider, &d.Number, &d.Title, &d.Author, &d.SourceBranch,
		&d.TargetBranch, &d.State, &d.Draft, &d.HeadSHA, &d.BaseSHA, &d.Labels, &d.URL,
		&d.CreatedAt, &d.UpdatedAt, &d.SyncedAt, &d.Revisions, &d.Approvals, &d.ChangesWanted,
		&d.PipelineID, &d.PipelineStatus, &d.GateState, &d.GateSummary, &d.Body, &d.RefreshError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT number, head_sha, base_sha, pushed_at FROM change_revisions
		 WHERE change_id = $1 ORDER BY number DESC`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ChangeRevision
		if err := rows.Scan(&r.Number, &r.HeadSHA, &r.BaseSHA, &r.PushedAt); err != nil {
			rows.Close()
			return nil, err
		}
		d.RevisionList = append(d.RevisionList, r)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx,
		`SELECT id, source, reviewer, verdict, body, commit_sha, submitted_at FROM change_reviews
		 WHERE change_id = $1 ORDER BY submitted_at DESC, id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ChangeReview
		if err := rows.Scan(&r.ID, &r.Source, &r.Reviewer, &r.Verdict, &r.Body, &r.CommitSHA, &r.SubmittedAt); err != nil {
			return nil, err
		}
		d.Reviews = append(d.Reviews, r)
	}
	return d, rows.Err()
}

// StoredChange returns Forge's copy of a change in the plugin's shape, or nil
// when there is none yet.
func (s *Store) StoredChange(ctx context.Context, repo, number string) (*vcs.Change, error) {
	var c vcs.Change
	var created, updated *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT number, title, body, author, source_branch, target_branch, state, draft,
		        head_sha, base_sha, labels, url, created_at, updated_at
		 FROM changes WHERE repo = $1 AND number = $2 AND synced_at IS NOT NULL`, repo, number).
		Scan(&c.ID, &c.Title, &c.Body, &c.Author, &c.SourceBranch, &c.TargetBranch, &c.State, &c.Draft,
			&c.HeadSHA, &c.BaseSHA, &c.Labels, &c.URL, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if created != nil {
		c.CreatedAt = *created
	}
	if updated != nil {
		c.UpdatedAt = *updated
	}
	return &c, nil
}

// changesChannel is the LISTEN/NOTIFY channel pages subscribe to; the payload
// is the change id.
const changesChannel = "forge_changes"

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ApplyChange writes the host's view of a change: the row, a new revision
// when the head moved, and — when ch.Reviews is not nil — the host's review
// list. An update older than the stored one (webhooks arrive out of order) is
// ignored. Returns the change id and whether anything was written.
func (s *Store) ApplyChange(ctx context.Context, repo, provider string, ch *vcs.Change) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	labels := ch.Labels
	if labels == nil {
		labels = []string{}
	}
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO changes (repo, provider, number, title, body, author, source_branch, target_branch,
		                      state, draft, head_sha, base_sha, labels, url, created_at, updated_at, synced_at, gate_dirty_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, now(), now())
		 ON CONFLICT (repo, number) DO UPDATE SET
		     provider = EXCLUDED.provider, title = EXCLUDED.title, body = EXCLUDED.body,
		     author = EXCLUDED.author, source_branch = EXCLUDED.source_branch,
		     target_branch = EXCLUDED.target_branch, state = EXCLUDED.state, draft = EXCLUDED.draft,
		     head_sha = EXCLUDED.head_sha, base_sha = EXCLUDED.base_sha, labels = EXCLUDED.labels,
		     url = EXCLUDED.url, created_at = COALESCE(EXCLUDED.created_at, changes.created_at),
		     updated_at = EXCLUDED.updated_at, synced_at = now(), gate_dirty_at = now()
		 WHERE changes.updated_at IS NULL OR EXCLUDED.updated_at IS NULL
		    OR EXCLUDED.updated_at >= changes.updated_at
		 RETURNING id`,
		repo, provider, ch.ID, ch.Title, ch.Body, ch.Author, ch.SourceBranch, ch.TargetBranch,
		ch.State, ch.Draft, ch.HeadSHA, ch.BaseSHA, labels, ch.URL,
		nullTime(ch.CreatedAt), nullTime(ch.UpdatedAt)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil // stale: a newer copy is already stored
	}
	if err != nil {
		return 0, false, err
	}
	if ch.HeadSHA != "" {
		// The row lock taken by the upsert serialises revision numbering.
		if _, err := tx.Exec(ctx,
			`INSERT INTO change_revisions (change_id, number, head_sha, base_sha)
			 SELECT $1, COALESCE(MAX(number), 0) + 1, $2, $3 FROM change_revisions WHERE change_id = $1
			 ON CONFLICT DO NOTHING`, id, ch.HeadSHA, ch.BaseSHA); err != nil {
			return 0, false, err
		}
	}
	// Warm the diff before anyone opens the page (a no-op when it exists).
	if err := requestChangeDiffTx(ctx, tx, repo, provider, ch); err != nil {
		return 0, false, err
	}
	if ch.Reviews != nil {
		keep := make([]string, 0, len(ch.Reviews))
		for _, r := range ch.Reviews {
			keep = append(keep, r.ExternalID)
			if _, err := tx.Exec(ctx,
				`INSERT INTO change_reviews (change_id, source, external_id, reviewer, verdict, body, commit_sha, submitted_at)
				 VALUES ($1, 'host', $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (change_id, source, external_id) DO UPDATE SET
				     reviewer = EXCLUDED.reviewer, verdict = EXCLUDED.verdict, body = EXCLUDED.body,
				     commit_sha = EXCLUDED.commit_sha, submitted_at = EXCLUDED.submitted_at`,
				id, r.ExternalID, r.Reviewer, r.Verdict, r.Body, r.CommitSHA, r.SubmittedAt); err != nil {
				return 0, false, err
			}
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM change_reviews WHERE change_id = $1 AND source = 'host' AND NOT (external_id = ANY($2))`,
			id, keep); err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(id, 10)); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}

// Refresh priorities, highest first (user writes never queue here).
const (
	PriorityBackfill  = 1
	PriorityReconcile = 2
	PriorityWebhook   = 3
)

// PriorityClass maps a refresh priority to its provider call class.
func PriorityClass(p int) string {
	switch p {
	case PriorityWebhook:
		return vcs.ClassWebhook
	case PriorityReconcile:
		return vcs.ClassReconcile
	}
	return vcs.ClassBackfill
}

// RequestChangeRefresh asks for one refresh of a change, due after delay.
// Requests coalesce: while a refresh is already due, its time is kept (so a
// burst collapses into one call), and a request arriving while one is in
// flight leaves exactly one more queued. fromWebhook stamps last_event_at.
func (s *Store) RequestChangeRefresh(ctx context.Context, repo, provider, number string, priority int, delay time.Duration, fromWebhook bool) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO changes (repo, provider, number, refresh_due_at, refresh_requested_at, refresh_priority, last_event_at)
		 VALUES ($1, $2, $3, now() + $4 * interval '1 millisecond', now(), $5, CASE WHEN $6 THEN now() END)
		 ON CONFLICT (repo, number) DO UPDATE SET
		     refresh_due_at = COALESCE(changes.refresh_due_at, EXCLUDED.refresh_due_at),
		     refresh_requested_at = now(),
		     refresh_priority = GREATEST(changes.refresh_priority, EXCLUDED.refresh_priority),
		     last_event_at = COALESCE(EXCLUDED.last_event_at, changes.last_event_at)`,
		repo, provider, number, delay.Milliseconds(), priority, fromWebhook)
	return err
}

// RefreshTask is one claimed change refresh.
type RefreshTask struct {
	ID       int64
	Repo     string
	Provider string
	Number   string
	Priority int
	Attempts int       // failed attempts so far (for backoff)
	Started  time.Time // when the claim was taken (DB clock)
}

// ClaimChangeRefreshes leases up to limit due refreshes, highest priority
// first. A lease that expires (the worker died) makes the refresh claimable
// again; SKIP LOCKED keeps replicas from taking the same one.
func (s *Store) ClaimChangeRefreshes(ctx context.Context, limit int, lease time.Duration) ([]RefreshTask, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE changes SET refresh_lease_until = now() + $2 * interval '1 millisecond'
		 WHERE id IN (
		     SELECT id FROM changes
		     WHERE refresh_due_at <= now() AND (refresh_lease_until IS NULL OR refresh_lease_until < now())
		     ORDER BY refresh_priority DESC, refresh_due_at
		     LIMIT $1 FOR UPDATE SKIP LOCKED)
		 RETURNING id, repo, provider, number, refresh_priority, refresh_attempts, now()`, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefreshTask
	for rows.Next() {
		var t RefreshTask
		if err := rows.Scan(&t.ID, &t.Repo, &t.Provider, &t.Number, &t.Priority, &t.Attempts, &t.Started); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FinishChangeRefresh releases a claim. On success the refresh is cleared
// unless another was requested meanwhile. On failure it is retried at
// retryAt (backoff or the host's reset time), or not at all when retryAt is
// zero, keeping the error for the page.
func (s *Store) FinishChangeRefresh(ctx context.Context, t RefreshTask, failure error, retryAt time.Time) error {
	if failure == nil {
		_, err := s.pool.Exec(ctx,
			`UPDATE changes SET refresh_lease_until = NULL, refresh_attempts = 0, refresh_error = '',
			     refresh_due_at = CASE WHEN refresh_requested_at > $2 THEN refresh_due_at END,
			     refresh_priority = CASE WHEN refresh_requested_at > $2 THEN refresh_priority ELSE 0 END
			 WHERE id = $1`, t.ID, t.Started)
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE changes SET refresh_lease_until = NULL, refresh_attempts = refresh_attempts + 1,
		     refresh_error = $2, refresh_due_at = $3
		 WHERE id = $1`, t.ID, truncate(failure.Error(), 300), nullTime(retryAt))
	return err
}

// DropChangeRefresh clears a refresh that can never succeed (the host does
// not know the change); placeholders that never synced are deleted.
func (s *Store) DropChangeRefresh(ctx context.Context, t RefreshTask, reason string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM changes WHERE id = $1 AND synced_at IS NULL`, t.ID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE changes SET refresh_lease_until = NULL, refresh_due_at = NULL, refresh_priority = 0,
		     refresh_error = $2 WHERE id = $1`, t.ID, truncate(reason, 300))
	return err
}

// RefreshBacklog is how many refreshes are due and the oldest one's age.
func (s *Store) RefreshBacklog(ctx context.Context) (n int, oldest time.Duration, err error) {
	var secs float64
	err = s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(refresh_due_at)), 0)
		 FROM changes WHERE refresh_due_at <= now()`).Scan(&n, &secs)
	return n, time.Duration(secs * float64(time.Second)), err
}

// ---- reconcile ----

// ReconcileClaim is one repo's reconcile lease plus its stored validators.
type ReconcileClaim struct {
	Repo       string
	Validators vcs.Validators
	First      bool // never reconciled: this is the backfill
}

// ClaimReconcile takes the reconcile lease for repo when it is due. ok=false
// when it isn't due or another replica holds it. Nothing is written for a
// repo that is not due, so checking every minute costs no row churn.
func (s *Store) ClaimReconcile(ctx context.Context, repo string, lease time.Duration) (ReconcileClaim, bool, error) {
	c := ReconcileClaim{Repo: repo}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO change_sync (repo) VALUES ($1) ON CONFLICT (repo) DO NOTHING`, repo); err != nil {
		return c, false, err
	}
	var last *time.Time
	err := s.pool.QueryRow(ctx,
		`UPDATE change_sync SET lease_until = now() + $2 * interval '1 millisecond'
		 WHERE repo = $1 AND next_reconcile_at <= now() AND (lease_until IS NULL OR lease_until < now())
		 RETURNING etag, last_modified, last_reconciled_at`, repo, lease.Milliseconds()).
		Scan(&c.Validators.ETag, &c.Validators.LastModified, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.First = last == nil
	return c, true, nil
}

// FinishReconcile stores the new validators and schedules the next run.
func (s *Store) FinishReconcile(ctx context.Context, repo string, v vcs.Validators, next time.Time, result string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE change_sync SET etag = $2, last_modified = $3, next_reconcile_at = $4,
		     last_reconciled_at = now(), last_result = $5, lease_until = NULL
		 WHERE repo = $1`, repo, v.ETag, v.LastModified, next, truncate(result, 200))
	return err
}

// OpenChangeRefs returns the stored open changes of repo, keyed by number,
// for comparison against the host's list.
func (s *Store) OpenChangeRefs(ctx context.Context, repo string) (map[string]vcs.ChangeRef, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT number, head_sha, updated_at FROM changes WHERE repo = $1 AND state = 'open'`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]vcs.ChangeRef{}
	for rows.Next() {
		var r vcs.ChangeRef
		var updated *time.Time
		if err := rows.Scan(&r.ID, &r.HeadSHA, &updated); err != nil {
			return nil, err
		}
		if updated != nil {
			r.UpdatedAt = *updated
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

// RegisteredProvider returns the provider a repo is connected through.
func (s *Store) RegisteredProvider(ctx context.Context, repo string) (string, bool, error) {
	var provider string
	err := s.pool.QueryRow(ctx, `SELECT provider FROM repo_registry WHERE repo = $1`, repo).Scan(&provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return provider, err == nil, err
}

// ReviewTarget resolves a repo for change-API calls: the connection with a
// ready credential, and the budget key that credential is counted under (an
// App installation, a token's fingerprint, or the host for anonymous calls).
func (s *Store) ReviewTarget(ctx context.Context, repo string) (vcs.RepoRef, string, bool, error) {
	conn, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil || !found {
		return vcs.RepoRef{}, "", found, err
	}
	var appID, installID string
	if err := s.pool.QueryRow(ctx,
		`SELECT github_app_id, github_app_installation_id FROM repo_registry WHERE repo = $1`, repo).
		Scan(&appID, &installID); err != nil {
		return vcs.RepoRef{}, "", false, err
	}
	ref := vcs.RepoRef{Provider: conn.Provider, Repo: repo, CloneURL: conn.CloneURL, Token: conn.Token,
		Region: conn.AWS.Region, Profile: conn.AWS.Profile, RoleARN: conn.AWS.RoleARN}
	var cred string
	switch {
	case appID != "" && installID != "":
		cred = "github-app:" + installID
	case conn.Provider == "codecommit":
		cred = "codecommit:" + conn.AWS.Region + ":" + conn.AWS.RoleARN
	case conn.Token != "":
		sum := sha256.Sum256([]byte(conn.Token))
		cred = conn.Provider + ":token:" + hex.EncodeToString(sum[:6])
	default:
		cred = conn.Provider + ":anon:" + repoHost(conn.CloneURL)
	}
	return ref, cred, true, nil
}

// repoHost is the host part of a clone URL (https or scp-style).
func repoHost(cloneURL string) string {
	u := cloneURL
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.IndexAny(u, "/:"); i >= 0 {
		u = u[:i]
	}
	return u
}

// ListenChanges calls fn with each changed change id until ctx ends. It holds
// one pooled connection for LISTEN; the caller fans out to subscribers.
func (s *Store) ListenChanges(ctx context.Context, fn func(id string)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+changesChannel); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), "UNLISTEN "+changesChannel) }()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
