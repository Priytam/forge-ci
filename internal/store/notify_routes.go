package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/secret"
)

// NotificationRoute sends some event kinds somewhere. Target and Secret are
// encrypted at rest; reads for the UI blank them (HasTarget/HasSecret).
type NotificationRoute struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"` // slack | teams | webhook | email
	Target      string    `json:"target,omitempty"`
	HasTarget   bool      `json:"has_target"`
	TargetHint  string    `json:"target_hint,omitempty"` // host, or address count — safe to show
	Secret      string    `json:"secret,omitempty"`
	HasSecret   bool      `json:"has_secret"`
	Events      []string  `json:"events"`
	RepoPattern string    `json:"repo_pattern"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// RepoPatternMatches is repoPatternMatches for other packages.
func RepoPatternMatches(pattern, repo string) bool { return repoPatternMatches(pattern, repo) }

func targetHint(kind, target string) string {
	if target == "" {
		return ""
	}
	if kind == "email" {
		n := 0
		for _, t := range strings.Split(target, ",") {
			if strings.TrimSpace(t) != "" {
				n++
			}
		}
		if strings.Contains(target, "approvers") {
			return "eligible approvers"
		}
		if n == 1 {
			return strings.TrimSpace(target)
		}
		return strings.TrimSpace(strings.Split(target, ",")[0]) + " +" + itoa(n-1)
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(target, "https://"), "http://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i] + "/…"
	}
	return rest
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ListNotificationRoutes returns every route; with reveal, targets and
// secrets are decrypted (for sending), else blanked (for the UI).
func (s *Store) ListNotificationRoutes(ctx context.Context, reveal bool) ([]NotificationRoute, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, kind, target, secret, events, repo_pattern, enabled, created_at
		 FROM notification_routes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationRoute{}
	for rows.Next() {
		var r NotificationRoute
		var encTarget, encSecret string
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &encTarget, &encSecret, &r.Events, &r.RepoPattern, &r.Enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		target, err := s.cipher.Decrypt(encTarget)
		if err != nil {
			return nil, err
		}
		r.HasTarget, r.HasSecret = target != "", encSecret != ""
		r.TargetHint = targetHint(r.Kind, target)
		if reveal {
			r.Target = target
			if r.Secret, err = s.cipher.Decrypt(encSecret); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveNotificationRoute inserts (ID 0) or updates a route. An empty Target
// or Secret on update keeps the stored value.
func (s *Store) SaveNotificationRoute(ctx context.Context, r NotificationRoute) (*NotificationRoute, error) {
	encTarget, err := s.cipher.Encrypt(r.Target)
	if err != nil {
		return nil, err
	}
	encSecret, err := s.cipher.Encrypt(r.Secret)
	if err != nil {
		return nil, err
	}
	if r.Events == nil {
		r.Events = []string{}
	}
	if r.RepoPattern == "" {
		r.RepoPattern = "*"
	}
	if r.ID == 0 {
		err = s.pool.QueryRow(ctx,
			`INSERT INTO notification_routes (name, kind, target, secret, events, repo_pattern, enabled)
			 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
			r.Name, r.Kind, encTarget, encSecret, r.Events, r.RepoPattern, r.Enabled).Scan(&r.ID, &r.CreatedAt)
	} else {
		err = s.pool.QueryRow(ctx,
			`UPDATE notification_routes SET name=$2, kind=$3,
			        target = CASE WHEN $4 = '' THEN target ELSE $4 END,
			        secret = CASE WHEN $5 = '' THEN secret ELSE $5 END,
			        events=$6, repo_pattern=$7, enabled=$8
			 WHERE id=$1 RETURNING created_at`,
			r.ID, r.Name, r.Kind, encTarget, encSecret, r.Events, r.RepoPattern, r.Enabled).Scan(&r.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
	}
	if err != nil {
		return nil, err
	}
	r.HasTarget, r.HasSecret = true, r.Secret != ""
	r.TargetHint = targetHint(r.Kind, r.Target)
	r.Target, r.Secret = "", ""
	return &r, nil
}

func (s *Store) DeleteNotificationRoute(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notification_routes WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- pipeline and deploy events ----

// PipelineEventNotice is a finished pipeline to announce.
type PipelineEventNotice struct {
	Kind          string // pipeline_failed | pipeline_fixed | pipeline_succeeded
	PipelineID    int64
	Repo, Ref     string
	SHA           string
	Status        string
	TriggeredBy   string
	CommitMessage string
	FailedJobs    []string
	Duration      time.Duration
}

// DeployEventNotice is a finished environment job to announce.
type DeployEventNotice struct {
	Kind         string // deploy_succeeded | deploy_failed
	JobID        int64
	JobName      string
	PipelineID   int64
	Repo, Ref    string
	SHA          string
	Environment  string
	Status       string
	DeployedBy   string
	URL, Version string
	Digest       string
}

const eventWindow = time.Hour

// ClaimPipelineEvents finds pipelines that reached a terminal status in the
// last hour and haven't been announced, claims each (pipeline_notifications)
// and returns them with their kind: failed, succeeded, or fixed when the
// previous finished pipeline on the same repo+ref had failed.
func (s *Store) ClaimPipelineEvents(ctx context.Context, limit int) ([]PipelineEventNotice, error) {
	rows, err := s.pool.Query(ctx,
		`WITH fin AS (
		   SELECT p.id, p.repo, p.ref, p.sha, p.triggered_by, p.commit_message, p.created_at,
		          CASE WHEN bool_or(j.status = 'failed' AND NOT j.allow_failure) OR bool_or(j.status = 'canceled') THEN 'failed'
		               ELSE 'success' END AS status,
		          max(j.finished_at) AS finished_at,
		          array_remove(array_agg(j.name || CASE WHEN j.status_reason <> '' THEN ' (' || j.status_reason || ')' ELSE '' END)
		                       FILTER (WHERE j.status = 'failed' AND NOT j.allow_failure), NULL) AS failed_jobs
		   FROM pipelines p JOIN jobs j ON j.pipeline_id = p.id AND NOT j.superseded
		   WHERE p.created_at > now() - $2::interval
		   GROUP BY p.id
		   HAVING bool_and(j.status IN ('success','failed','canceled'))
		      AND bool_or(j.status <> 'canceled')
		      AND NOT EXISTS (SELECT 1 FROM pipeline_notifications n WHERE n.pipeline_id = p.id)
		 ), claimed AS (
		   INSERT INTO pipeline_notifications (pipeline_id, event)
		   SELECT id, status FROM fin ORDER BY id LIMIT $1
		   ON CONFLICT DO NOTHING RETURNING pipeline_id
		 )
		 SELECT f.id, f.repo, f.ref, f.sha, f.status, f.triggered_by, f.commit_message, f.failed_jobs,
		        EXTRACT(EPOCH FROM (f.finished_at - f.created_at)),
		        (SELECT CASE WHEN bool_or(j2.status = 'failed' AND NOT j2.allow_failure) THEN 'failed' ELSE 'success' END
		         FROM pipelines p2 JOIN jobs j2 ON j2.pipeline_id = p2.id AND NOT j2.superseded
		         WHERE p2.repo = f.repo AND p2.ref = f.ref AND p2.id < f.id
		           AND p2.id = (SELECT max(p3.id) FROM pipelines p3 JOIN jobs j3 ON j3.pipeline_id = p3.id
		                        WHERE p3.repo = f.repo AND p3.ref = f.ref AND p3.id < f.id
		                          AND j3.status IN ('success','failed'))
		         GROUP BY p2.id) AS previous
		 FROM fin f JOIN claimed c ON c.pipeline_id = f.id ORDER BY f.id`, limit, eventWindow.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PipelineEventNotice{}
	for rows.Next() {
		var n PipelineEventNotice
		var secs *float64
		var previous *string
		if err := rows.Scan(&n.PipelineID, &n.Repo, &n.Ref, &n.SHA, &n.Status, &n.TriggeredBy, &n.CommitMessage,
			&n.FailedJobs, &secs, &previous); err != nil {
			return nil, err
		}
		if secs != nil && *secs > 0 {
			n.Duration = time.Duration(*secs * float64(time.Second))
		}
		switch {
		case n.Status == "failed":
			n.Kind = "pipeline_failed"
		case previous != nil && *previous == "failed":
			n.Kind = "pipeline_fixed"
		default:
			n.Kind = "pipeline_succeeded"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClaimDeployEvents claims environment jobs that finished in the last hour
// and haven't been announced.
func (s *Store) ClaimDeployEvents(ctx context.Context, limit int) ([]DeployEventNotice, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE jobs j SET deploy_notified_at = now()
		 FROM pipelines p
		 WHERE j.id IN (SELECT j2.id FROM jobs j2
		                WHERE j2.environment IS NOT NULL AND j2.environment <> '' AND NOT j2.superseded
		                  AND j2.status IN ('success','failed') AND j2.deploy_notified_at IS NULL
		                  AND j2.finished_at > now() - $2::interval
		                ORDER BY j2.id LIMIT $1 FOR UPDATE SKIP LOCKED)
		   AND p.id = j.pipeline_id
		 RETURNING j.id, j.name, p.id, p.repo, p.ref, p.sha, j.environment, j.status,
		           COALESCE((SELECT d.deployed_by FROM deployments d WHERE d.job_id = j.id), p.triggered_by),
		           COALESCE((SELECT d.url FROM deployments d WHERE d.job_id = j.id), ''),
		           COALESCE((SELECT d.version FROM deployments d WHERE d.job_id = j.id), ''),
		           COALESCE((SELECT d.digest FROM deployments d WHERE d.job_id = j.id), '')`,
		limit, eventWindow.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployEventNotice{}
	for rows.Next() {
		var n DeployEventNotice
		if err := rows.Scan(&n.JobID, &n.JobName, &n.PipelineID, &n.Repo, &n.Ref, &n.SHA, &n.Environment, &n.Status,
			&n.DeployedBy, &n.URL, &n.Version, &n.Digest); err != nil {
			return nil, err
		}
		n.Kind = "deploy_succeeded"
		if n.Status == "failed" {
			n.Kind = "deploy_failed"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Cipher exposes the store's cipher for signing (approval links).
func (s *Store) Cipher() *secret.Cipher { return s.cipher }
