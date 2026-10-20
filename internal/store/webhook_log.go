package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Webhook delivery log: one row per delivery a git host sent, accepted or
// not, so a repo whose hooks are misconfigured (a stale secret, a second hook
// nobody remembers) shows up on its settings page instead of only in server
// logs.

// WebhookEntry is one logged delivery.
type WebhookEntry struct {
	ID         int64     `json:"id"`
	ReceivedAt time.Time `json:"received_at"`
	Provider   string    `json:"provider"`
	Repo       string    `json:"repo,omitempty"`
	Event      string    `json:"event,omitempty"`
	Status     int       `json:"status"` // HTTP status Forge answered
	Reason     string    `json:"reason,omitempty"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	SourceIP   string    `json:"source_ip,omitempty"`
	PipelineID *int64    `json:"pipeline_id,omitempty"`
}

// LogWebhook records a delivery (best-effort: a logging failure never fails
// the webhook).
func (s *Store) LogWebhook(ctx context.Context, e WebhookEntry) {
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO webhook_log (provider, repo, event, status, reason, delivery_id, source_ip, pipeline_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.Provider, e.Repo, e.Event, e.Status, truncate(e.Reason, 200), truncate(e.DeliveryID, 120), e.SourceIP, e.PipelineID)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// WebhookHealth summarises a repo's deliveries over a window.
type WebhookHealth struct {
	Repo      string     `json:"repo"`
	Accepted  int        `json:"accepted"`
	Rejected  int        `json:"rejected"` // signature / token failures (401)
	Errors    int        `json:"errors"`   // 4xx/5xx other than 401, excluding duplicates
	Duplicate int        `json:"duplicates"`
	Sources   int        `json:"sources"` // distinct source IPs: more than one means more than one hook
	FailingIP []string   `json:"failing_sources,omitempty"`
	LastAt    *time.Time `json:"last_at,omitempty"`
}

// RepoWebhookHealth returns a repo's recent deliveries and their summary.
func (s *Store) RepoWebhookHealth(ctx context.Context, repo string, window time.Duration, limit int) (WebhookHealth, []WebhookEntry, error) {
	h := WebhookHealth{Repo: repo}
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status < 300 AND reason <> 'duplicate'),
		        count(*) FILTER (WHERE status = 401),
		        count(*) FILTER (WHERE status >= 400 AND status <> 401),
		        count(*) FILTER (WHERE reason = 'duplicate'),
		        count(DISTINCT source_ip) FILTER (WHERE source_ip <> ''),
		        COALESCE(array_agg(DISTINCT source_ip) FILTER (WHERE status = 401 AND source_ip <> ''), '{}'),
		        max(received_at)
		 FROM webhook_log WHERE repo = $1 AND received_at > now() - $2::interval`, repo, window.String()).
		Scan(&h.Accepted, &h.Rejected, &h.Errors, &h.Duplicate, &h.Sources, &h.FailingIP, &h.LastAt)
	if err != nil {
		return h, nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, received_at, provider, repo, event, status, reason, delivery_id, source_ip, pipeline_id
		 FROM webhook_log WHERE repo = $1 ORDER BY id DESC LIMIT $2`, repo, limit)
	if err != nil {
		return h, nil, err
	}
	entries, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WebhookEntry, error) {
		var e WebhookEntry
		err := r.Scan(&e.ID, &e.ReceivedAt, &e.Provider, &e.Repo, &e.Event, &e.Status, &e.Reason, &e.DeliveryID, &e.SourceIP, &e.PipelineID)
		return e, err
	})
	return h, entries, err
}

// FailingWebhookRepos lists repos with rejected or errored deliveries in the
// window (the admin overview), worst first.
func (s *Store) FailingWebhookRepos(ctx context.Context, window time.Duration) ([]WebhookHealth, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT repo,
		        count(*) FILTER (WHERE status < 300 AND reason <> 'duplicate'),
		        count(*) FILTER (WHERE status = 401),
		        count(*) FILTER (WHERE status >= 400 AND status <> 401),
		        count(*) FILTER (WHERE reason = 'duplicate'),
		        count(DISTINCT source_ip) FILTER (WHERE source_ip <> ''),
		        COALESCE(array_agg(DISTINCT source_ip) FILTER (WHERE status = 401 AND source_ip <> ''), '{}'),
		        max(received_at)
		 FROM webhook_log WHERE received_at > now() - $1::interval AND repo <> ''
		 GROUP BY repo
		 HAVING count(*) FILTER (WHERE status >= 400 AND reason <> 'duplicate') > 0
		 ORDER BY count(*) FILTER (WHERE status >= 400 AND reason <> 'duplicate') DESC, repo`, window.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WebhookHealth, error) {
		var h WebhookHealth
		err := r.Scan(&h.Repo, &h.Accepted, &h.Rejected, &h.Errors, &h.Duplicate, &h.Sources, &h.FailingIP, &h.LastAt)
		return h, err
	})
}

// PruneWebhookLog drops entries older than keep.
func (s *Store) PruneWebhookLog(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhook_log WHERE received_at < now() - $1::interval`, keep.String())
	return tag.RowsAffected(), err
}

// RepoFromWebhookBody pulls the repository name out of a provider payload
// without trusting anything else in it — enough to attribute a rejected
// delivery to a repo on its settings page.
func RepoFromWebhookBody(provider string, body []byte) string {
	var p struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		Detail struct {
			RepositoryName string `json:"repositoryName"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	switch {
	case p.Repository.FullName != "":
		return truncate(p.Repository.FullName, 200)
	case p.Project.PathWithNamespace != "":
		return truncate(p.Project.PathWithNamespace, 200)
	case p.Detail.RepositoryName != "":
		return truncate(p.Detail.RepositoryName, 200)
	}
	return ""
}
