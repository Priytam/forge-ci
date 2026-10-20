import { useCallback, useEffect, useState, type FormEvent } from "react";
import { getAbout, getIsAdmin, relativeTime, setMaintenance, type AboutInfo } from "../api";

/** Admin: server and runner versions, migrations, uptime, maintenance mode. */
export default function AdminAbout() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [info, setInfo] = useState<AboutInfo | null>(null);
  const [message, setMessage] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const reload = useCallback(() => {
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
