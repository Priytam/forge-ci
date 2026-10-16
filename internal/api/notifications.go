package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

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
	writeJSON(w, http.StatusOK, cfg)
}

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
	if in.SlackWebhookURL != "" && !strings.HasPrefix(in.SlackWebhookURL, "https://") {
		writeErr(w, http.StatusBadRequest, "slack_webhook_url must be an https:// incoming-webhook URL")
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
	s.getNotifySettings(w, r)
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
	msg := notify.ApprovalMessage(store.ApprovalNotice{
		JobID: 0, JobName: "deploy_production (test)", Repo: "forge/test", Ref: "main",
		SHA: "0000000000", Environment: "production", TriggeredBy: me, RequiredApprovals: 1,
		ApproverRoles: []string{"owner", "admin"}, Approvers: []string{me},
	}, frontendURL())
	msg.Subject = "[Forge] Test notification"
	if err := (&notify.Sender{}).Deliver(r.Context(), *cfg, []string{me}, msg); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "email_to": me})
}
