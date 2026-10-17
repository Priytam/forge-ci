package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Event kinds a route can subscribe to.
const (
	EventApprovalNeeded    = "approval_needed"
	EventPipelineFailed    = "pipeline_failed"
	EventPipelineFixed     = "pipeline_fixed" // success after the previous run on the ref failed
	EventPipelineSucceeded = "pipeline_succeeded"
	EventDeploySucceeded   = "deploy_succeeded"
	EventDeployFailed      = "deploy_failed"
)

// EventKinds lists every event kind, in display order.
var EventKinds = []string{EventApprovalNeeded, EventPipelineFailed, EventPipelineFixed, EventPipelineSucceeded, EventDeploySucceeded, EventDeployFailed}

// Event is one thing that happened, rendered once and sent to every route
// that subscribes to its kind, in whatever shape the notifier wants.
type Event struct {
	Kind        string            `json:"event"`
	Repo        string            `json:"repo"`
	Ref         string            `json:"ref,omitempty"`
	SHA         string            `json:"sha,omitempty"`
	PipelineID  int64             `json:"pipeline_id,omitempty"`
	JobID       int64             `json:"job_id,omitempty"`
	JobName     string            `json:"job_name,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Status      string            `json:"status,omitempty"`
	Title       string            `json:"title"`
	Text        string            `json:"text"`
	Link        string            `json:"link"`
	Fields      map[string]string `json:"fields,omitempty"`
	At          time.Time         `json:"at"`
	// Approvers who may act; per-approver signed approve/reject links are
	// built by the email notifier from Actions (approver → links).
	Approvers []string                 `json:"approvers,omitempty"`
	Actions   map[string]ApprovalLinks `json:"-"`
}

// ApprovalLinks are one approver's signed act-by-link URLs ("" = disabled).
type ApprovalLinks struct {
	Approve string
	Reject  string
}

// Notifier delivers an event to one target. Implementations are stateless;
// target and secret come from the route.
type Notifier interface {
	Name() string
	// Send delivers ev. target is the route's address; secret its signing
	// key (webhook) — other notifiers ignore it.
	Send(ctx context.Context, target, secret string, ev Event) error
	// ValidateTarget checks a target before a route is saved.
	ValidateTarget(target string) error
}

var notifiers = map[string]Notifier{}

func register(n Notifier) { notifiers[n.Name()] = n }

// Lookup returns the notifier for kind.
func Lookup(kind string) (Notifier, bool) {
	n, ok := notifiers[kind]
	return n, ok
}

// Kinds lists the registered notifiers.
func Kinds() []string {
	out := make([]string, 0, len(notifiers))
	for k := range notifiers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func emoji(kind string) string {
	switch kind {
	case EventApprovalNeeded:
		return "⏸"
	case EventPipelineFailed, EventDeployFailed:
		return "❌"
	case EventPipelineFixed:
		return "✅"
	case EventPipelineSucceeded, EventDeploySucceeded:
		return "🚀"
	}
	return "•"
}

// orderedFields renders Fields deterministically.
func orderedFields(ev Event) [][2]string {
	keys := make([]string, 0, len(ev.Fields))
	for k := range ev.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, ev.Fields[k]})
	}
	return out
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("returned %d", resp.StatusCode)
	}
	return nil
}

// httpsOnly requires https, except for a loopback target (a receiver on the
// same machine, in development).
func httpsOnly(target string) error {
	if strings.HasPrefix(target, "https://") {
		return nil
	}
	if u, err := url.Parse(target); err == nil && u.Scheme == "http" {
		if h := u.Hostname(); h == "localhost" || net.ParseIP(h).IsLoopback() {
			return nil
		}
	}
	return errors.New("target must be an https:// URL")
}

// ---- Slack (incoming webhook) ----

type slackNotifier struct{}

func init() { register(slackNotifier{}) }

func (slackNotifier) Name() string                       { return "slack" }
func (slackNotifier) ValidateTarget(target string) error { return httpsOnly(target) }

func (slackNotifier) Send(ctx context.Context, target, _ string, ev Event) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s*\n%s", emoji(ev.Kind), slackEscape(ev.Title), slackEscape(ev.Text))
	for _, f := range orderedFields(ev) {
		fmt.Fprintf(&b, "\n• %s: %s", slackEscape(f[0]), slackEscape(f[1]))
	}
	blocks := []map[string]any{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": b.String()}}}
	if ev.Link != "" {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []map[string]any{{
			"type": "button", "text": map[string]string{"type": "plain_text", "text": "Open in Forge"}, "url": ev.Link,
		}}})
	}
	// The top-level text is the notification/fallback text; it carries the
	// whole message so clients without Block Kit lose nothing.
	body, _ := json.Marshal(map[string]any{"text": ev.Title + "\n" + ev.Text, "blocks": blocks})
	if err := postJSON(ctx, defaultClient(), target, nil, body); err != nil {
		return fmt.Errorf("slack webhook %w", err)
	}
	return nil
}

// ---- Microsoft Teams (incoming webhook / workflow) ----

type teamsNotifier struct{}

func init() { register(teamsNotifier{}) }

func (teamsNotifier) Name() string                       { return "teams" }
func (teamsNotifier) ValidateTarget(target string) error { return httpsOnly(target) }

// Send posts an Adaptive Card, accepted by Teams workflows ("Post to a
// channel when a webhook request is received") and the older Office 365
// connector webhooks.
func (teamsNotifier) Send(ctx context.Context, target, _ string, ev Event) error {
	facts := []map[string]string{}
	for _, f := range orderedFields(ev) {
		facts = append(facts, map[string]string{"title": f[0], "value": f[1]})
	}
	card := map[string]any{
		"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4",
		"body": []map[string]any{
			{"type": "TextBlock", "text": emoji(ev.Kind) + " " + ev.Title, "weight": "Bolder", "size": "Medium", "wrap": true},
			{"type": "TextBlock", "text": ev.Text, "wrap": true},
			{"type": "FactSet", "facts": facts},
		},
	}
	if ev.Link != "" {
		card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": "Open in Forge", "url": ev.Link}}
	}
	body, _ := json.Marshal(map[string]any{
		"type":        "message",
		"attachments": []map[string]any{{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}},
	})
	if err := postJSON(ctx, defaultClient(), target, nil, body); err != nil {
		return fmt.Errorf("teams webhook %w", err)
	}
	return nil
}

// ---- generic webhook (JSON, HMAC-signed) ----

type webhookNotifier struct{}

func init() { register(webhookNotifier{}) }

func (webhookNotifier) Name() string                       { return "webhook" }
func (webhookNotifier) ValidateTarget(target string) error { return httpsOnly(target) }

// Send posts the event as JSON. With a route secret the body is signed:
// X-Forge-Signature: sha256=<hex HMAC-SHA256 of the body>, plus
// X-Forge-Event and X-Forge-Delivery headers (a fresh id per send).
func (webhookNotifier) Send(ctx context.Context, target, secret string, ev Event) error {
	body, _ := json.Marshal(ev)
	headers := map[string]string{
		"X-Forge-Event":    ev.Kind,
		"X-Forge-Delivery": strconv.FormatInt(time.Now().UnixNano(), 36),
		"User-Agent":       "forge-ci-notifier",
	}
	if secret != "" {
		headers["X-Forge-Signature"] = "sha256=" + Sign(secret, body)
	}
	if err := postJSON(ctx, defaultClient(), target, headers, body); err != nil {
		return fmt.Errorf("webhook %w", err)
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of body under secret (what receivers verify).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func defaultClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }
