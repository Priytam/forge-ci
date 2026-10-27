import Skeleton from "../components/ui/Skeleton";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  createNotificationRoute,
  deleteNotificationRoute,
  getIsAdmin,
  getNotifySettings,
  listNotificationRoutes,
  putNotifySettings,
  relativeTime,
  testNotificationRoute,
  testNotify,
  updateNotificationRoute,
  type NotificationRoute,
  type NotificationRoutesResponse,
  type NotifySettings,
} from "../api";

const EVENT_LABELS: Record<string, string> = {
  approval_needed: "deploy waits for approval",
  pipeline_failed: "pipeline failed",
  pipeline_fixed: "pipeline fixed",
  pipeline_succeeded: "pipeline succeeded",
  deploy_succeeded: "deploy succeeded",
  deploy_failed: "deploy failed",
};

const KIND_LABELS: Record<string, string> = {
  slack: "Slack",
  teams: "Microsoft Teams",
  webhook: "Webhook (JSON, signed)",
  email: "Email",
};

const TARGET_HINT: Record<string, string> = {
  slack: "https://hooks.slack.com/services/…",
  teams: "https://….logic.azure.com/… (Teams workflow or connector URL)",
  webhook: "https://your-service/forge-events",
  email: 'addresses separated by commas, or "approvers"',
};

/** Where events go: one row per destination, each with the events it takes. */
function Routes() {
  const [data, setData] = useState<NotificationRoutesResponse | null>(null);
  const [editing, setEditing] = useState<NotificationRoute | null>(null);
  const blank = { name: "", kind: "slack", target: "", secret: "", events: ["pipeline_failed"] as string[], repo_pattern: "*", enabled: true };
  const [form, setForm] = useState(blank);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const reload = useCallback(() => {
    void listNotificationRoutes()
      .then(setData)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(reload, [reload]);
  if (!data) return null;

  const startEdit = (r: NotificationRoute) => {
    setEditing(r);
    setForm({ name: r.name, kind: r.kind, target: "", secret: "", events: r.events, repo_pattern: r.repo_pattern, enabled: r.enabled });
    setMsg(null);
  };
  const toggleEvent = (e: string) =>
    setForm({ ...form, events: form.events.includes(e) ? form.events.filter((x) => x !== e) : [...form.events, e] });
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      const input = { ...form, target: form.target.trim() || undefined, secret: form.secret || undefined };
      if (editing) await updateNotificationRoute(editing.id, input);
      else await createNotificationRoute(input);
      setMsg({ ok: true, text: editing ? "Route updated." : "Route added." });
      setEditing(null);
      setForm(blank);
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };
  const remove = async (r: NotificationRoute) => {
    if (!window.confirm(`Delete the route "${r.name}"?`)) return;
    await deleteNotificationRoute(r.id).catch((e) => setMsg({ ok: false, text: String(e) }));
    reload();
  };
  const test = async (r: NotificationRoute) => {
    setMsg(null);
    try {
      await testNotificationRoute(r.id);
      setMsg({ ok: true, text: `Test sent to ${r.name}.` });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  return (
    <div className="card settings-form" style={{ marginTop: 16 }}>
      <h2>Routes</h2>
      <p className="muted">
        Send events where your team works: a Slack or Teams channel, your own service over a signed webhook,
        or email. Each route picks its events and, optionally, which repos (<code>*</code>, <code>acme/*</code>
        , one repo). Approval emails carry personal Approve / Reject links
        {data.links_enabled ? "." : " once the server runs with FORGE_SECRET_KEY."}
      </p>
      {data.routes.length > 0 && (
        <div className="table-card" style={{ marginBottom: 12 }}>
          <table className="dense-table">
            <thead>
              <tr>
                <th>Route</th>
                <th>Kind</th>
                <th>Target</th>
                <th>Events</th>
                <th>Repos</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {data.routes.map((r) => (
                <tr key={r.id} style={r.enabled ? undefined : { opacity: 0.55 }}>
                  <td>
                    {r.name}
                    {!r.enabled && <span className="muted"> (off)</span>}
                  </td>
                  <td>{KIND_LABELS[r.kind] ?? r.kind}</td>
                  <td className="mono muted">{r.target_hint || "—"}</td>
                  <td className="muted">{r.events.map((e) => EVENT_LABELS[e] ?? e).join(", ")}</td>
                  <td className="mono">{r.repo_pattern}</td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    <button type="button" className="btn btn-icon" onClick={() => void test(r)}>
                      Test
                    </button>{" "}
                    <button type="button" className="btn btn-icon" onClick={() => startEdit(r)}>
                      Edit
                    </button>{" "}
                    <button type="button" className="btn btn-icon" onClick={() => void remove(r)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <form onSubmit={(e) => void save(e)}>
        <h3>{editing ? `Edit ${editing.name}` : "New route"}</h3>
        <div className="form-row">
          <label className="field">
            <span>Name</span>
            <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="#deploys channel" required />
          </label>
          <label className="field">
            <span>Kind</span>
            <select value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
              {data.kinds.map((k) => (
                <option key={k} value={k}>
                  {KIND_LABELS[k] ?? k}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Repos</span>
            <input className="mono" value={form.repo_pattern} onChange={(e) => setForm({ ...form, repo_pattern: e.target.value })} placeholder="*" />
          </label>
        </div>
        <div className="form-row">
          <label className="field">
            <span>Target</span>
            <input
              className="mono"
              value={form.target}
              onChange={(e) => setForm({ ...form, target: e.target.value })}
              placeholder={editing?.has_target ? `•••••• saved (${editing.target_hint}) — paste to replace` : TARGET_HINT[form.kind]}
            />
          </label>
          {form.kind === "webhook" && (
            <label className="field">
              <span>Signing secret</span>
              <input
                type="password"
                autoComplete="off"
                value={form.secret}
                onChange={(e) => setForm({ ...form, secret: e.target.value })}
                placeholder={editing?.has_secret ? "•••••• saved — leave blank to keep" : "optional; verify X-Forge-Signature"}
              />
            </label>
          )}
        </div>
        <div className="field">
          <span>Events</span>
          <div className="check-inline">
            {data.events.map((ev) => (
              <label key={ev} className="check-row">
                <input type="checkbox" checked={form.events.includes(ev)} onChange={() => toggleEvent(ev)} />
                {EVENT_LABELS[ev] ?? ev}
              </label>
            ))}
          </div>
        </div>
        <label className="check-row">
          <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
          Enabled
        </label>
        <div className="form-actions">
          <button type="submit" className="btn btn-primary">
            {editing ? "Save route" : "Add route"}
          </button>
          {editing && (
            <button type="button" className="btn" onClick={() => { setEditing(null); setForm(blank); }}>
              Cancel
            </button>
          )}
        </div>
        {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      </form>

    </div>
  );
}

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
  if (!cfg) return msg ? <div className="error-banner">{msg.text}</div> : <Skeleton />;

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
      if (saved.smtp_check && !saved.smtp_check.ok) {
        setMsg({ ok: false, text: `Saved, but the SMTP server could not be reached: ${saved.smtp_check.error}` });
      } else if (saved.smtp_check?.ok) {
        setMsg({ ok: true, text: "Saved. SMTP connection and sign-in OK." });
      } else {
        setMsg({ ok: true, text: "Saved." });
      }
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
        Forge announces what matters: a deploy waiting for approval, a pipeline that failed or is
        fixed, a deploy that landed or didn't. The <strong>approval defaults</strong> below reach
        every eligible approver (a Slack channel and/or email); <strong>routes</strong> send any
        event to Slack, Microsoft Teams, your own webhook, or email.
      </p>

      {Object.entries(cfg.delivery ?? {}).some(([, d]) => d.consecutive_failures >= 3) && (
        <div className="error-banner">
          Notifications are failing:{" "}
          {Object.entries(cfg.delivery ?? {})
            .filter(([, d]) => d.consecutive_failures >= 3)
            .map(([ch, d]) => `${ch} — ${d.consecutive_failures} in a row, last: ${d.error}`)
            .join("; ")}
        </div>
      )}

      <form className="card settings-form" onSubmit={(e) => void save(e)}>
        <h2>Approval defaults</h2>
        {cfg.delivery && Object.keys(cfg.delivery).length > 0 && (
          <p className="muted">
            {Object.entries(cfg.delivery).map(([ch, d]) => (
              <span key={ch} style={{ marginRight: 16 }}>
                last {ch}: {d.ok ? "ok" : <span className="error-text">failed — {d.error}</span>} ({relativeTime(d.at)})
              </span>
            ))}
          </p>
        )}
        <h3>Slack</h3>
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

        <h3 style={{ marginTop: 16 }}>Email (SMTP — also used by email routes)</h3>
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

      <Routes />
    </div>
  );
}
