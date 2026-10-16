package api

import (
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestNotificationSettingsAPI(t *testing.T) {
	h := newEnforcedHarness(t)
	admin := h.login("admin@acme.io")
	dev := h.login("dev@acme.io")
	const hook = "https://hooks.slack.com/services/T000/B000/secretpart"

	h.expect(h.do(dev, "GET", "/api/v1/notification-settings", nil), 403, "dev reads settings")
	h.expect(h.do(dev, "PUT", "/api/v1/notification-settings", map[string]any{"slack_webhook_url": hook}), 403, "dev saves")
	h.expect(h.do(admin, "PUT", "/api/v1/notification-settings",
		map[string]any{"slack_webhook_url": "http://insecure"}), 400, "non-https webhook")
	h.expect(h.do(admin, "PUT", "/api/v1/notification-settings",
		map[string]any{"email_enabled": true}), 400, "email without smtp")

	rec := h.do(admin, "PUT", "/api/v1/notification-settings", map[string]any{
		"slack_webhook_url": hook, "email_enabled": true, "smtp_host": "smtp.acme.io",
		"smtp_port": 587, "smtp_username": "forge", "smtp_password": "pw", "smtp_from": "forge@acme.io",
	})
	h.expect(rec, 200, "admin saves")
	if strings.Contains(rec.Body.String(), "secretpart") || strings.Contains(rec.Body.String(), `"pw"`) {
		t.Fatalf("settings response leaked a secret: %s", rec.Body.String())
	}
	got := decodeInto[store.NotifySettings](t, rec)
	if !got.HasSlack || !got.HasSMTPPassword || got.SMTPHost != "smtp.acme.io" {
		t.Errorf("saved settings = %+v", got)
	}
	// Saving again without secrets keeps them.
	h.expect(h.do(admin, "PUT", "/api/v1/notification-settings", map[string]any{
		"email_enabled": true, "smtp_host": "smtp2.acme.io", "smtp_from": "forge@acme.io",
	}), 200, "save without secrets")
	full, err := h.st.GetNotifySettings(t.Context(), true)
	if err != nil || full.SlackWebhookURL != hook || full.SMTPPassword != "pw" || full.SMTPHost != "smtp2.acme.io" {
		t.Errorf("stored settings = %+v, %v", full, err)
	}
	// Stored encrypted at rest.
	var raw string
	_ = h.db.QueryRow(t.Context(), `SELECT value::text FROM platform_settings WHERE key='notifications'`).Scan(&raw)
	if strings.Contains(raw, "secretpart") {
		t.Errorf("webhook URL stored in plaintext: %s", raw)
	}
	h.expect(h.do(admin, "PUT", "/api/v1/notification-settings", map[string]any{"clear_slack": true}), 200, "clear slack")
	if after, _ := h.st.GetNotifySettings(t.Context(), false); after.HasSlack {
		t.Error("clear_slack did not remove the webhook")
	}
}
