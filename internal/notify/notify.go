// Package notify delivers approval notifications: one Slack message to the
// configured incoming webhook, and one email to each eligible approver.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Message is what gets sent, independent of channel.
type Message struct {
	Subject string
	Text    string // plain text body
	Link    string // where to approve
}

// ApprovalMessage renders the "job is waiting for approval" message.
func ApprovalMessage(n store.ApprovalNotice, frontend string) Message {
	link := fmt.Sprintf("%s/jobs/%d?pipeline=%d", strings.TrimSuffix(frontend, "/"), n.JobID, n.PipelineID)
	sha := n.SHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	subject := fmt.Sprintf("[Forge] %s: %s is waiting for approval to deploy to %s", n.Repo, n.JobName, n.Environment)
	var b strings.Builder
	fmt.Fprintf(&b, "%s in %s is waiting for approval to deploy to %s.\n", n.JobName, n.Repo, n.Environment)
	fmt.Fprintf(&b, "Branch %s @ %s", strings.TrimPrefix(n.Ref, "refs/heads/"), sha)
	if title := strings.SplitN(n.CommitMessage, "\n", 2)[0]; title != "" {
		fmt.Fprintf(&b, " — %s", title)
	}
	b.WriteString("\n")
	if n.TriggeredBy != "" {
		fmt.Fprintf(&b, "Started by %s.\n", n.TriggeredBy)
	}
	need := fmt.Sprintf("Needs %d approval", n.RequiredApprovals)
	if n.RequiredApprovals != 1 {
		need += "s"
	}
	if len(n.ApproverRoles) > 0 {
		need += " from " + strings.Join(n.ApproverRoles, "/")
	}
	if n.RequiredGroup != "" && n.RequiredGroupApprovals > 0 {
		need += fmt.Sprintf(", at least %d from %s", n.RequiredGroupApprovals, n.RequiredGroup)
	}
	b.WriteString(need + ".\n")
	if len(n.Approvers) > 0 {
		fmt.Fprintf(&b, "Can approve: %s.\n", strings.Join(n.Approvers, ", "))
	}
	fmt.Fprintf(&b, "Approve or reject: %s\n", link)
	return Message{Subject: subject, Text: b.String(), Link: link}
}

// Sender delivers messages using the server's notification settings.
type Sender struct {
	HTTP    *http.Client
	Timeout time.Duration
}

func (s *Sender) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Slack posts the message to an incoming-webhook URL.
func (s *Sender) Slack(ctx context.Context, webhookURL string, m Message) error {
	body, _ := json.Marshal(map[string]any{
		"text": m.Text,
		"blocks": []map[string]any{
			{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": slackEscape(m.Text)}},
			{"type": "actions", "elements": []map[string]any{{
				"type": "button", "text": map[string]string{"type": "plain_text", "text": "Review in Forge"},
				"url": m.Link,
			}}},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	return nil
}

func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Email sends m to each recipient over SMTP: implicit TLS on port 465,
// otherwise STARTTLS when the server offers it. Recipients that are not
// email addresses are skipped.
func (s *Sender) Email(ctx context.Context, cfg store.NotifySettings, to []string, m Message) error {
	var rcpts []string
	for _, r := range to {
		if strings.Contains(r, "@") {
			rcpts = append(rcpts, r)
		}
	}
	if len(rcpts) == 0 {
		return nil
	}
	if cfg.SMTPHost == "" || cfg.SMTPFrom == "" {
		return errors.New("email is enabled but smtp_host or smtp_from is not set")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(port))
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.SMTPHost})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: cfg.SMTPHost}); err != nil {
				return err
			}
		}
	}
	if cfg.SMTPUsername != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)); err != nil {
			return err
		}
	}
	for _, r := range rcpts {
		if err := c.Mail(cfg.SMTPFrom); err != nil {
			return err
		}
		if err := c.Rcpt(r); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n\r\n%s",
			cfg.SMTPFrom, r, m.Subject, strings.ReplaceAll(m.Text, "\n", "\r\n"))
		if err := w.Close(); err != nil {
			return err
		}
	}
	return c.Quit()
}

// Deliver sends m on every configured channel and returns the joined error of
// the channels that failed (nil when all succeeded or none is configured).
func (s *Sender) Deliver(ctx context.Context, cfg store.NotifySettings, to []string, m Message) error {
	var errs []error
	if cfg.SlackWebhookURL != "" {
		if err := s.Slack(ctx, cfg.SlackWebhookURL, m); err != nil {
			errs = append(errs, fmt.Errorf("slack: %w", err))
		}
	}
	if cfg.EmailEnabled {
		if err := s.Email(ctx, cfg, to, m); err != nil {
			errs = append(errs, fmt.Errorf("email: %w", err))
		}
	}
	return errors.Join(errs...)
}
