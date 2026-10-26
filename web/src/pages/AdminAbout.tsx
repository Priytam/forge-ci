import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { getAbout, getCodeReviewStats, getFailingWebhooks, getIsAdmin, relativeTime, setMaintenance, type AboutInfo, type CodeReviewStats, type WebhookHealth } from "../api";

/** Admin: server and runner versions, migrations, uptime, maintenance mode. */
export default function AdminAbout() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [info, setInfo] = useState<AboutInfo | null>(null);
  const [message, setMessage] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [failing, setFailing] = useState<WebhookHealth[]>([]);
  const [review, setReview] = useState<CodeReviewStats | null>(null);
  const reload = useCallback(() => {
    void getFailingWebhooks().then(setFailing).catch(() => undefined);
    void getCodeReviewStats().then(setReview).catch(() => undefined);
    void getAbout()
      .then((i) => {
        setInfo(i);
        setMessage(i.maintenance?.message ?? "");
      })
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);
  if (isAdmin === false) return <div className="error-banner">This page is for admins.</div>;
  if (!info) return msg ? <div className="error-banner">{msg.text}</div> : <div className="muted">Loading…</div>;

  const toggle = async (e: FormEvent, on: boolean) => {
    e.preventDefault();
    setMsg(null);
    try {
      await setMaintenance(on, message);
      setMsg({ ok: true, text: on ? "Maintenance mode is on: runners take no new jobs." : "Maintenance mode is off." });
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };
  const uptime = (s: number) => (s < 3600 ? `${Math.round(s / 60)} min` : s < 86400 ? `${(s / 3600).toFixed(1)} h` : `${(s / 86400).toFixed(1)} days`);

  return (
    <div>
      <div className="page-head">
        <h1>About this server</h1>
      </div>
      <div className="card settings-form">
        <h2>Versions</h2>
        <table className="dense-table">
          <tbody>
            <tr>
              <td className="muted">Server</td>
              <td className="mono">{info.version}</td>
            </tr>
            <tr>
              <td className="muted">Bundled runner (served from /downloads)</td>
              <td className="mono">{info.runner_version}</td>
            </tr>
            <tr>
              <td className="muted">Platform</td>
              <td className="mono">{info.platform}</td>
            </tr>
            <tr>
              <td className="muted">Started</td>
              <td>
                {relativeTime(info.started_at)} <span className="muted">(up {uptime(info.uptime_seconds)})</span>
              </td>
            </tr>
            <tr>
              <td className="muted">Database</td>
              <td>{info.database_ok ? "connected" : <span className="error-text">unreachable</span>}</td>
            </tr>
            <tr>
              <td className="muted">Migrations</td>
              <td>
                {info.schema?.applied_at ? (
                  <>
                    applied {relativeTime(info.schema.applied_at)} by version <span className="mono">{info.schema.version}</span>
                  </>
                ) : (
                  "applied at start (no record yet)"
                )}
              </td>
            </tr>
          </tbody>
        </table>
        {info.version === "dev" && (
          <p className="muted">
            This build is not stamped. Build with <code>-ldflags "-X github.com/priytamjeepandey/forge-ci/internal/buildinfo.Version=1.9.0"</code>{" "}
            (the Dockerfiles take <code>--build-arg VERSION=…</code>) so runners can tell when they are outdated.
          </p>
        )}
      </div>

      <div className="card settings-form" style={{ marginTop: 16 }}>
        <h2>Webhook health (24h)</h2>
        {failing.length === 0 ? (
          <p className="muted">No repo has had a rejected or failed webhook delivery in the last 24 hours.</p>
        ) : (
          <table className="dense-table">
            <thead>
              <tr>
                <th>Repo</th>
                <th>Accepted</th>
                <th>Rejected signature</th>
                <th>Errors</th>
                <th>Sources</th>
              </tr>
            </thead>
            <tbody>
              {failing.map((f) => (
                <tr key={f.repo}>
                  <td>
                    <Link to={`/repos/${encodeURIComponent(f.repo)}/settings`} className="mono">{f.repo}</Link>
                  </td>
                  <td>{f.accepted}</td>
                  <td className={f.rejected > 0 ? "error-text" : ""}>{f.rejected}{f.failing_sources?.length ? <span className="muted"> ({f.failing_sources.join(", ")})</span> : null}</td>
                  <td className={f.errors > 0 ? "error-text" : ""}>{f.errors}</td>
                  <td>{f.sources}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {review && <CodeReviewCard stats={review} />}

      <form className="card settings-form" style={{ marginTop: 16 }}>
        <h2>Maintenance mode</h2>
        <p className="muted">
          While on, runners take no new jobs (jobs queue; running jobs finish) and every page shows a banner.
          Turn it on before a server upgrade, off after.
        </p>
        {info.maintenance?.on && (
          <div className="error-banner">
            On since {relativeTime(info.maintenance.since ?? "")} by {info.maintenance.by || "an admin"}.
          </div>
        )}
        <label className="field">
          <span>Message shown in the banner (optional)</span>
          <input value={message} onChange={(e) => setMessage(e.target.value)} placeholder="Back in 10 minutes." />
        </label>
        <div className="form-actions">
          {info.maintenance?.on ? (
            <button type="button" className="btn btn-primary" onClick={(e) => void toggle(e, false)}>
              Turn maintenance off
            </button>
          ) : (
            <button type="button" className="btn btn-reject" onClick={(e) => void toggle(e, true)}>
              Turn maintenance on
            </button>
          )}
        </div>
        {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      </form>
    </div>
  );
}

/** Provider API footprint of code review: what it cost, what is left, and
 *  the read-path guard (pages must never call a provider). */
function CodeReviewCard({ stats }: { stats: CodeReviewStats }) {
  const byProvider = new Map<string, number>();
  for (const c of stats.calls) if (!c.read_path) byProvider.set(c.provider, (byProvider.get(c.provider) ?? 0) + c.calls);
  return (
    <div className="card settings-form" style={{ marginTop: 16 }}>
      <h2>Code review: provider API use ({stats.window_hours}h)</h2>
      <table className="dense-table">
        <tbody>
          <tr>
            <td className="muted">Provider calls</td>
            <td>
              {stats.total_calls}
              {byProvider.size > 0 && (
                <span className="muted"> ({Array.from(byProvider, ([p, n]) => `${p} ${n}`).join(", ")})</span>
              )}
            </td>
          </tr>
          <tr>
            <td className="muted">Answered 304 (free on GitHub)</td>
            <td>{Math.round(stats.not_modified_ratio * 100)}%</td>
          </tr>
          <tr>
            <td className="muted">Calls per open change</td>
            <td>
              {stats.calls_per_open_change.toFixed(1)} <span className="muted">({stats.open_changes} open)</span>
            </td>
          </tr>
          <tr>
            <td className="muted">Calls attempted while serving a page</td>
            <td>{stats.read_path_calls === 0 ? "0 (as designed)" : <span className="error-text">{stats.read_path_calls} — refused; this is a bug</span>}</td>
          </tr>
          <tr>
            <td className="muted">Refreshes waiting</td>
            <td>
              {stats.refresh_backlog}
              {stats.refresh_backlog > 0 && <span className="muted"> (oldest {Math.round(stats.oldest_refresh_seconds)}s)</span>}
            </td>
          </tr>
          {stats.budgets.map((b) => (
            <tr key={b.credential}>
              <td className="muted">
                Budget <span className="mono">{b.credential}</span>
              </td>
              <td>
                {b.remaining} / {b.capacity} left ({Math.round((1 - b.remaining / Math.max(b.capacity, 1)) * 100)}% used)
                {b.reset_at && <span className="muted"> · resets at {new Date(b.reset_at).toLocaleTimeString()}</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
