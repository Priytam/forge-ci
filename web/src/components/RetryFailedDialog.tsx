import { useState } from "react";
import { Link } from "react-router-dom";
import { retryFailedJobs, type RetryFailedResult } from "../api";

const WINDOWS = [
  { v: "1h", label: "the last hour" },
  { v: "6h", label: "the last 6 hours" },
  { v: "24h", label: "the last 24 hours" },
  { v: "72h", label: "the last 3 days" },
];

/**
 * Retry every failed job matching a name and/or error text since a time —
 * for the dozens of deploys an outage failed with the same transient error.
 * Preview (a dry run) lists the jobs first; each retry needs the same rights
 * as retrying one job.
 */
export default function RetryFailedDialog({ repo, onClose }: { repo?: string; onClose: (changed: boolean) => void }) {
  const [name, setName] = useState("");
  const [error, setErrorText] = useState("");
  const [since, setSince] = useState("6h");
  const [preview, setPreview] = useState<RetryFailedResult | null>(null);
  const [done, setDone] = useState<RetryFailedResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const run = async (dryRun: boolean) => {
    setBusy(true);
    setFailure(null);
    try {
      const r = await retryFailedJobs({ repo, name: name.trim(), error: error.trim(), since, dry_run: dryRun });
      if (dryRun) setPreview(r);
      else setDone(r);
    } catch (e) {
      setFailure(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const edit = (f: () => void) => {
    f();
    setPreview(null);
  };
  const shown = done ?? preview;
  const skipped = new Map((shown?.skipped ?? []).map((s) => [s.job_id, s.reason]));
  const retried = new Set((done?.retried ?? []).map((r) => r.job_id));
  const willRetry = preview ? preview.matched.filter((m) => !skipped.has(m.job_id)).length : 0;

  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose(Boolean(done))}>
      <div className="dialog dialog-wide" role="dialog" aria-modal="true" aria-labelledby="retry-failed-title">
        <h2 id="retry-failed-title">Retry failed jobs{repo ? ` in ${repo}` : ""}</h2>
        <div className="dialog-body">
          <div className="form-row">
            <label className="field">
              <span>Job name contains</span>
              <input value={name} disabled={Boolean(done)} placeholder="deploy" onChange={(e) => edit(() => setName(e.target.value))} />
            </label>
            <label className="field">
              <span>Error text (status or end of log)</span>
              <input value={error} disabled={Boolean(done)} placeholder="i/o timeout" onChange={(e) => edit(() => setErrorText(e.target.value))} />
            </label>
            <label className="field">
              <span>Failed in</span>
              <select value={since} disabled={Boolean(done)} onChange={(e) => edit(() => setSince(e.target.value))}>
                {WINDOWS.map((w) => (
                  <option key={w.v} value={w.v}>
                    {w.label}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {failure && <div className="error-banner">{failure}</div>}
          {shown && (
            <div className="retry-failed-list">
              <strong>
                {done
                  ? `Retried ${done.retried.length}${done.skipped.length ? `, skipped ${done.skipped.length}` : ""}`
                  : `${shown.matched.length} failed job${shown.matched.length === 1 ? "" : "s"} match`}
              </strong>
              {shown.matched.length > 0 && (
                <table className="dense-table">
                  <tbody>
                    {shown.matched.map((m) => (
                      <tr key={m.job_id}>
                        <td className="mono">{m.repo}</td>
                        <td>
                          <Link to={`/pipelines/${m.pipeline_id}`}>#{m.pipeline_id}</Link> · {m.ref}
                        </td>
                        <td>{m.name}</td>
                        <td className="muted">
                          {skipped.get(m.job_id) ?? (retried.has(m.job_id) ? "retried" : done ? "" : "will retry")}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          )}
        </div>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={() => onClose(Boolean(done))}>
            {done ? "Close" : "Cancel"}
          </button>
          {!done && (
            <>
              <button type="button" className="btn" disabled={busy || (!name.trim() && !error.trim())} onClick={() => void run(true)}>
                Preview
              </button>
              <button type="button" className="btn btn-primary" disabled={busy || willRetry === 0} onClick={() => void run(false)}>
                {busy ? "Working…" : `Retry ${preview ? willRetry : "…"}`}
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
