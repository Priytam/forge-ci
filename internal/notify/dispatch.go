package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Dispatch sends ev to every enabled route subscribed to its kind whose
// repo pattern matches, and — for approval_needed — to the legacy global
// Slack webhook / approver email when configured, so an upgrade changes
// nothing until routes are added. Failures are logged per route and joined.
func (s *Sender) Dispatch(ctx context.Context, routes []store.NotificationRoute, legacy *store.NotifySettings, ev Event) error {
	var errs []error
	for _, r := range routes {
		if !r.Enabled || !containsStr(r.Events, ev.Kind) || !store.RepoPatternMatches(r.RepoPattern, ev.Repo) {
			continue
		}
		if err := s.sendRoute(ctx, r, legacy, ev); err != nil {
			slog.Error("notify: route failed", "route", r.Name, "kind", r.Kind, "event", ev.Kind, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
	}
	if ev.Kind == EventApprovalNeeded && legacy != nil {
		if legacy.SlackWebhookURL != "" {
			if err := (slackNotifier{}).Send(ctx, legacy.SlackWebhookURL, "", ev); err != nil {
				errs = append(errs, fmt.Errorf("slack: %w", err))
			}
		}
		if legacy.EmailEnabled {
			if err := s.emailEvent(ctx, *legacy, ev.Approvers, ev); err != nil {
				errs = append(errs, fmt.Errorf("email: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Sender) sendRoute(ctx context.Context, r store.NotificationRoute, legacy *store.NotifySettings, ev Event) error {
	if r.Kind == "email" {
		if legacy == nil || !legacy.EmailEnabled && legacy.SMTPHost == "" {
			return errors.New("email route needs SMTP settings (Admin → Notifications)")
		}
		var to []string
		for _, t := range strings.Split(r.Target, ",") {
			switch t = strings.TrimSpace(t); {
			case t == "":
			case t == "approvers":
				to = append(to, ev.Approvers...)
			default:
				to = append(to, t)
			}
		}
		return s.emailEvent(ctx, *legacy, to, ev)
	}
	n, ok := Lookup(r.Kind)
	if !ok {
		return fmt.Errorf("unknown notifier %q", r.Kind)
	}
	return n.Send(ctx, r.Target, r.Secret, ev)
}

// emailEvent mails ev to each recipient; an approver with signed links gets
// their own Approve / Reject links in the body.
func (s *Sender) emailEvent(ctx context.Context, cfg store.NotifySettings, to []string, ev Event) error {
	var errs []error
	seen := map[string]bool{}
	for _, rcpt := range to {
		rcpt = strings.ToLower(strings.TrimSpace(rcpt))
		if rcpt == "" || seen[rcpt] || !strings.Contains(rcpt, "@") {
			continue
		}
		seen[rcpt] = true
		var b strings.Builder
		b.WriteString(ev.Text)
		b.WriteString("\n")
		for _, f := range orderedFields(ev) {
			fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
		}
		if links, ok := ev.Actions[rcpt]; ok && links.Approve != "" {
			fmt.Fprintf(&b, "\nApprove: %s\nReject:  %s\n(These links are for you only; they expire.)\n", links.Approve, links.Reject)
		}
		if ev.Link != "" {
			fmt.Fprintf(&b, "\nOpen in Forge: %s\n", ev.Link)
		}
		links := ev.Actions[rcpt]
		if err := s.Email(ctx, cfg, []string{rcpt}, Message{Subject: "[Forge] " + ev.Title, Text: b.String(), HTML: RenderHTML(ev, links), Link: ev.Link}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
