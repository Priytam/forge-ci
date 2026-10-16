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

// notifyApprovals sends one notification per job that started waiting for
// approval. Nothing is claimed when no channel is configured, so turning
// notifications on later still reaches jobs blocked in the last hour.
func (sc *Scheduler) notifyApprovals(ctx context.Context) {
	cfg, err := sc.store.GetNotifySettings(ctx, true)
	if err != nil {
		slog.Error("notify: load settings", "err", err)
		return
	}
	if cfg.SlackWebhookURL == "" && !cfg.EmailEnabled {
		return
	}
	notices, err := sc.store.ClaimApprovalNotices(ctx, 20, adminEmails())
	if err != nil {
		slog.Error("notify: claim approval waits", "err", err)
		return
	}
	for _, n := range notices {
		msg := notify.ApprovalMessage(n, frontendURL())
		if err := sc.sender.Deliver(ctx, *cfg, n.Approvers, msg); err != nil {
			slog.Error("notify: approval notification failed", "job", n.JobID, "err", err)
			continue
		}
		slog.Info("notify: approvers told", "job", n.JobID, "env", n.Environment, "approvers", len(n.Approvers))
	}
}

// NotifyApprovalsOnce runs one notification pass (tests and tooling).
func (sc *Scheduler) NotifyApprovalsOnce(ctx context.Context) { sc.notifyApprovals(ctx) }
