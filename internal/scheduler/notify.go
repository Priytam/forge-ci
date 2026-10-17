package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/notify"
)

// notifyEvery is how often the scheduler looks for jobs that started waiting
// for approval. It runs in its own goroutine so a slow SMTP server never
// delays job transitions.
const notifyEvery = 5 * time.Second

// adminEmails mirrors the API's ADMIN_EMAILS parsing: the people to tell
// when a repo has no members of its own.
func adminEmails() []string {
	var out []string
	for _, e := range strings.Split(os.Getenv("ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// frontendURL is where links in notifications point (FRONTEND_URL, else
// EXTERNAL_URL, else the dev default).
func frontendURL() string {
	for _, k := range []string{"FRONTEND_URL", "EXTERNAL_URL"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	return "http://localhost:5173"
}

func (sc *Scheduler) notifyLoop(ctx context.Context) {
	t := time.NewTicker(sc.notifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.notifyApprovals(ctx)
		}
	}
}

// externalURL is the API's public base (EXTERNAL_URL, else FRONTEND_URL),
// where act-by-link approval URLs point.
func externalURL() string {
	for _, k := range []string{"EXTERNAL_URL", "FRONTEND_URL"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	return "http://localhost:8080"
}

// notifyApprovals sends every pending event — approval waits, finished
// pipelines, finished deploys — to the routes that subscribe to it (and
// approval waits to the legacy Slack/email settings). Nothing is claimed
// when there is nowhere to send it, so configuring notifications later still
// reaches events from the last hour.
func (sc *Scheduler) notifyApprovals(ctx context.Context) {
	cfg, err := sc.store.GetNotifySettings(ctx, true)
	if err != nil {
		slog.Error("notify: load settings", "err", err)
		return
	}
	routes, err := sc.store.ListNotificationRoutes(ctx, true)
	if err != nil {
		slog.Error("notify: load routes", "err", err)
		return
	}
	subscribed := func(kind string) bool {
		for _, r := range routes {
			if r.Enabled && containsEvent(r.Events, kind) {
				return true
			}
		}
		return false
	}
	legacyApprovals := cfg.SlackWebhookURL != "" || cfg.EmailEnabled

	if legacyApprovals || subscribed(notify.EventApprovalNeeded) {
		notices, err := sc.store.ClaimApprovalNotices(ctx, 20, adminEmails())
		if err != nil {
			slog.Error("notify: claim approval waits", "err", err)
		}
		for _, n := range notices {
			ev := notify.ApprovalEvent(n, frontendURL())
			notify.ApprovalLinksFor(sc.store.Cipher(), externalURL(), &ev)
			if err := sc.sender.Dispatch(ctx, routes, cfg, ev); err != nil {
				slog.Error("notify: approval notification failed", "job", n.JobID, "err", err)
				continue
			}
			slog.Info("notify: approvers told", "job", n.JobID, "env", n.Environment, "approvers", len(n.Approvers))
		}
	}
	if subscribed(notify.EventPipelineFailed) || subscribed(notify.EventPipelineFixed) || subscribed(notify.EventPipelineSucceeded) {
		notices, err := sc.store.ClaimPipelineEvents(ctx, 50)
		if err != nil {
			slog.Error("notify: claim pipeline events", "err", err)
		}
		for _, n := range notices {
			if err := sc.sender.Dispatch(ctx, routes, nil, notify.PipelineEvent(n, frontendURL())); err != nil {
				slog.Error("notify: pipeline event failed", "pipeline", n.PipelineID, "event", n.Kind, "err", err)
			}
		}
	}
	if subscribed(notify.EventDeploySucceeded) || subscribed(notify.EventDeployFailed) {
		notices, err := sc.store.ClaimDeployEvents(ctx, 50)
		if err != nil {
			slog.Error("notify: claim deploy events", "err", err)
		}
		for _, n := range notices {
			if err := sc.sender.Dispatch(ctx, routes, nil, notify.DeployEvent(n, frontendURL())); err != nil {
				slog.Error("notify: deploy event failed", "job", n.JobID, "event", n.Kind, "err", err)
			}
		}
	}
}

func containsEvent(list []string, kind string) bool {
	for _, e := range list {
		if e == kind {
			return true
		}
	}
	return false
}

// NotifyApprovalsOnce runs one notification pass (tests and tooling).
func (sc *Scheduler) NotifyApprovalsOnce(ctx context.Context) { sc.notifyApprovals(ctx) }
