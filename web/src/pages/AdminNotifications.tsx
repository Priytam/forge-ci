import { useEffect, useState, type FormEvent } from "react";
import { getIsAdmin, getNotifySettings, putNotifySettings, testNotify, type NotifySettings } from "../api";

/**
 * Admin: where "a job is waiting for your approval" notifications go. Slack
 * (one incoming webhook for the team channel) and/or email to each person who
 * may approve. Secrets are write-only: leave a field blank to keep it.
 */
export default function AdminNotifications() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [cfg, setCfg] = useState<NotifySettings | null>(null);
  const [slackURL, setSlackURL] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    void getNotifySettings()
      .then(setCfg)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);

  if (isAdmin === false) return <div className="error-banner">Notifications are managed by admins.</div>;
  if (!cfg) return msg ? <div className="error-banner">{msg.text}</div> : <div className="muted">Loading…</div>;

  const set = (p: Partial<NotifySettings>) => setCfg({ ...cfg, ...p });

  const save = async (e: FormEvent, extra: Partial<NotifySettings> = {}) => {
    e.preventDefault();
    setBusy(true);
    setMsg(null);
    try {
      const saved = await putNotifySettings({
        ...cfg,
        slack_webhook_url: slackURL.trim() || undefined,
        smtp_password: password || undefined,
        ...extra,
      });
      setCfg(saved);
      setSlackURL("");
      setPassword("");
      setMsg({ ok: true, text: "Saved." });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setBusy(false);
    }
  };

  const sendTest = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const r = await testNotify();
      setMsg({ ok: true, text: `Test sent${cfg.email_enabled ? ` (email to ${r.email_to})` : ""}.` });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Notifications</h1>
      </div>
      <p className="page-lede">
        When a job starts waiting for environment approval, Forge tells the people who can
        approve it once: a message in your Slack channel and an email to each eligible approver
        (repo members whose role the environment accepts; the platform admins when a repo has
        no members). The message links straight to the job.
      </p>

      <form className="card settings-form" onSubmit={(e) => void save(e)}>
        <h2>Slack</h2>
        <label className="field">
          <span>Incoming webhook URL</span>
          <input
            value={slackURL}
            onChange={(e) => setSlackURL(e.target.value)}
            placeholder={cfg.has_slack ? "•••••• saved — paste a new URL to replace" : "https://hooks.slack.com/services/…"}
          />
        </label>
        {cfg.has_slack && (
          <button type="button" className="btn" disabled={busy} onClick={(e) => void save(e, { clear_slack: true })}>
            Remove Slack webhook
          </button>
        )}

        <h2 style={{ marginTop: 16 }}>Email</h2>
        <label className="check-row">
          <input
            type="checkbox"
            checked={cfg.email_enabled}
            onChange={(e) => set({ email_enabled: e.target.checked })}
          />
          Email each eligible approver
        </label>
        <div className="form-row">
          <label className="field">
            <span>SMTP host</span>
            <input value={cfg.smtp_host} onChange={(e) => set({ smtp_host: e.target.value })} placeholder="smtp.gmail.com" />
          </label>
          <label className="field">
            <span>Port</span>
            <input
              type="number"
              value={cfg.smtp_port || ""}
              onChange={(e) => set({ smtp_port: Number(e.target.value) || 0 })}
              placeholder="587"
            />
          </label>
          <label className="field">
            <span>From address</span>
            <input value={cfg.smtp_from} onChange={(e) => set({ smtp_from: e.target.value })} placeholder="forge@company.com" />
          </label>
        </div>
        <div className="form-row">
          <label className="field">
            <span>Username</span>
            <input value={cfg.smtp_username} onChange={(e) => set({ smtp_username: e.target.value })} />
          </label>
          <label className="field">
            <span>Password</span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder={cfg.has_smtp_password ? "•••••• saved — leave blank to keep" : ""}
            />
          </label>
        </div>
        <p className="muted">Port 465 uses TLS; other ports upgrade with STARTTLS when the server offers it.</p>

        <div className="form-actions">
          <button type="submit" className="btn btn-primary" disabled={busy}>
            Save
          </button>
          <button type="button" className="btn" disabled={busy || (!cfg.has_slack && !cfg.email_enabled)} onClick={() => void sendTest()}>
            Send test
          </button>
        </div>
        {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      </form>
    </div>
  );
}
