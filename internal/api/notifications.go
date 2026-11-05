package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/notify"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Approval notifications settings (admin). Secrets (the Slack webhook URL and
// SMTP password) are write-only: reads return has_slack / has_smtp_password.

func (s *Server) registerNotificationRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/notification-settings", s.getNotifySettings)
	m.HandleFunc("PUT /api/v1/notification-settings", s.putNotifySettings)
	m.HandleFunc("POST /api/v1/notification-settings/test", s.testNotify)
}

func (s *Server) getNotifySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, err := s.store.GetNotifySettings(r.Context(), false)
	if err != nil {
		slog.Error("get notification settings", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load notification settings")
		return
	}
	writeJSON(w, http.StatusOK, s.notifyView(r, cfg, nil))
}

// notifyView is the settings plus delivery health and, after a save, the
// SMTP connection check.
func (s *Server) notifyView(r *http.Request, cfg *store.NotifySettings, check *smtpCheck) map[string]any {
	out := map[string]any{}
	raw, _ := json.Marshal(cfg)
	_ = json.Unmarshal(raw, &out)
	delivery, _ := s.store.DeliveryStatus(r.Context())
	out["delivery"] = delivery
	if check != nil {
		out["smtp_check"] = check
	}
	return out
}

type smtpCheck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

func (s *Server) putNotifySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in store.NotifySettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in.SlackWebhookURL = strings.TrimSpace(in.SlackWebhookURL)
	in.SMTPHost, in.SMTPUsername, in.SMTPFrom = strings.TrimSpace(in.SMTPHost), strings.TrimSpace(in.SMTPUsername), strings.TrimSpace(in.SMTPFrom)
	if in.SlackWebhookURL != "" && !strings.HasPrefix(in.SlackWebhookURL, "https://") {
		writeErr(w, http.StatusBadRequest, "slack_webhook_url must be an https:// incoming-webhook URL")
		return
	}
	if in.SMTPHost != "" && !hostRe.MatchString(in.SMTPHost) {
		writeErr(w, http.StatusBadRequest, "smtp_host must be a host name (no spaces, scheme or port)")
		return
	}
	if in.EmailEnabled && (in.SMTPHost == "" || !strings.Contains(in.SMTPFrom, "@")) {
		writeErr(w, http.StatusBadRequest, "email needs smtp_host and an smtp_from address")
		return
	}
	if in.SMTPPort < 0 || in.SMTPPort > 65535 {
		writeErr(w, http.StatusBadRequest, "smtp_port is out of range")
		return
	}
	if err := s.store.SaveNotifySettings(r.Context(), in); err != nil {
		slog.Error("save notification settings", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save notification settings")
		return
	}
	// NEVER audit the webhook URL or password.
	s.audit(r, "notifications.update", "", "", "ok", map[string]any{
		"slack_set":     in.SlackWebhookURL != "" && !in.ClearSlack,
		"slack_cleared": in.ClearSlack,
		"email_enabled": in.EmailEnabled,
		"smtp_host":     in.SMTPHost,
	})
	// Saved; now say whether the SMTP server can be reached and signed into,
	// so a typo shows up here and not as silently undelivered mail later.
	cfg, err := s.store.GetNotifySettings(r.Context(), false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load notification settings")
		return
	}
	var check *smtpCheck
	if cfg.EmailEnabled && r.URL.Query().Get("check") != "0" {
		full, _ := s.store.GetNotifySettings(r.Context(), true)
		check = &smtpCheck{OK: true}
		if err := (&notify.Sender{Timeout: 10 * time.Second}).CheckSMTP(r.Context(), *full); err != nil {
			check.OK, check.Error = false, err.Error()
		}
	}
	writeJSON(w, http.StatusOK, s.notifyView(r, cfg, check))
}

// testNotify sends a sample approval notification on every configured
// channel; the email goes to the signed-in admin.
func (s *Server) testNotify(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	cfg, err := s.store.GetNotifySettings(r.Context(), true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load notification settings")
		return
	}
	if cfg.SlackWebhookURL == "" && !cfg.EmailEnabled {
		writeErr(w, http.StatusBadRequest, "no channel configured — add a Slack webhook or enable email first")
		return
	}
	me := s.sessionActor(r)
	// The same event and template a real approval wait produces.
	ev := notify.ApprovalEvent(store.ApprovalNotice{
		JobID: 0, JobName: "deploy_production (test)", Repo: "forge/test", Ref: "main",
		SHA: "0000000000", Environment: "production", TriggeredBy: me, RequiredApprovals: 1,
		ApproverRoles: []string{"owner", "admin"}, Approvers: []string{me},
	}, frontendURL())
	ev.Title = "Test notification — " + ev.Title
	sender := &notify.Sender{OnResult: func(ch string, err error) { _ = s.store.RecordDeliveryResult(r.Context(), ch, err) }}
	if err := sender.Dispatch(r.Context(), nil, cfg, ev); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "email_to": me})
}
