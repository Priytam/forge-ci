import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { getRunnerLoad, listRunners, relativeTime, setRunnerPaused, type RunnerLoadRow } from "../api";
import { usePoll } from "../hooks/usePoll";

interface TagStat {
  tag: string;
  online: number;
  total: number;
}

export default function Runners() {
  const { data: runners, error, loading, refresh } = usePoll(listRunners, 5000);
  const { data: load } = usePoll(getRunnerLoad, 5000);
  const loadById = useMemo(() => {
    const m = new Map<string, RunnerLoadRow>();
    for (const r of load?.runners ?? []) m.set(String(r.id), r);
    return m;
  }, [load]);
  const [actionError, setActionError] = useState<string | null>(null);

  const tagStats: TagStat[] = useMemo(() => {
    const map = new Map<string, TagStat>();
    for (const r of runners ?? []) {
      for (const tag of r.tags ?? []) {
        let stat = map.get(tag);
        if (!stat) {
          stat = { tag, online: 0, total: 0 };
          map.set(tag, stat);
        }
        stat.total += 1;
        if (r.online) stat.online += 1;
      }
    }
    return [...map.values()].sort((a, b) => a.tag.localeCompare(b.tag));
  }, [runners]);

  const togglePause = async (id: number | string, paused: boolean) => {
    setActionError(null);
    try {
      await setRunnerPaused(id, paused);
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Runners</h1>
      </div>

      <p className="muted page-lede">
        Runners are deployed independently of repos and advertise capability
        tags. Repos select a runner group via tags (repo Settings → Runner
        tags); job-level <code>tags:</code> override. Register one with:{" "}
        <code>
          ./forge-runner --server=&lt;url&gt;
          --executor=shell|docker|kubernetes --tags=a,b
        </code>{" "}
        — see docs/runners.md. Shell/docker runners take one job at a time; a
        kubernetes manager runs up to N parallel jobs as ephemeral pods with{" "}
        <code>--concurrency=N</code>.
      </p>

      <p className="muted page-lede">
        Guides: <Link to="/docs/runner-docker">Docker runner</Link> ·{" "}
        <Link to="/docs/runner-kubernetes">Kubernetes runner</Link> ·{" "}
        <Link to="/docs/runner-vm">VM runner</Link>
      </p>

      {tagStats.length > 0 && (
        <div className="tag-strip">
          {tagStats.map((s) => (
            <span key={s.tag} className="tag-stat-chip">
              <span className="mono">{s.tag}</span>{" "}
              <span className="muted">
                ({s.online} online / {s.total} total)
              </span>
            </span>
          ))}
        </div>
      )}

      {load && (
        <>
          <h2 className="section-title">
            Queue <span className="muted">· {load.pending} waiting · {load.running} running</span>
          </h2>
          {load.queues.length === 0 ? (
            <div className="empty card">Nothing is waiting for a runner.</div>
          ) : (
            <div className="card table-card" style={{ marginBottom: 16 }}>
              <table className="dense-table">
                <thead>
                  <tr>
                    <th>Needs runner tags</th>
                    <th>Waiting</th>
                    <th>Oldest wait</th>
                    <th>Eligible runners online</th>
                  </tr>
                </thead>
                <tbody>
                  {load.queues.map((q) => (
                    <tr key={q.tags.join(",") || "(none)"}>
                      <td>
                        {q.tags.length ? (
                          q.tags.map((t) => (
                            <span key={t} className="ref-tag" style={{ marginRight: 4 }}>
                              {t}
                            </span>
                          ))
                        ) : (
                          <span className="muted">any runner</span>
                        )}
                      </td>
                      <td>{q.pending}</td>
                      <td className={q.oldest_wait_seconds > 300 ? "status-reason" : "muted"}>{waitLabel(q.oldest_wait_seconds)}</td>
                      <td>
                        {q.eligible_online === 0 ? (
                          <span className="status-reason">none — jobs can't start</span>
                        ) : (
                          <span>
                            {q.eligible_online} <span className="muted">({q.eligible_busy} busy)</span>
                          </span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <p className="muted">
            Autoscalers can scale on <code>forge_queue_pending_jobs{"{tags=\"…\"}"}</code> and{" "}
            <code>forge_queue_oldest_wait_seconds</code> from <code>/api/v1/metrics</code> (docs/runners.md).
          </p>
        </>
      )}

      <h2 className="section-title">Runners</h2>
      {error && <div className="error-banner">Failed to load runners: {error}</div>}
      {actionError && <div className="error-banner">{actionError}</div>}
      {loading && !runners && <div className="muted">Loading runners…</div>}

      {runners && runners.length === 0 && (
        <div className="empty card">No runners registered yet.</div>
      )}

      {runners && runners.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>ID</th>
                <th>Executor</th>
                <th>Tags</th>
                <th>Description</th>
                <th>Status</th>
                <th>Running now</th>
                <th>Last 24h</th>
                <th>Last contact</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {runners.map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.id}</td>
                  <td>
                    <span className={`exec-badge exec-${r.executor}`}>
                      {r.executor}
                    </span>
                  </td>
                  <td>
                    {r.tags && r.tags.length > 0 ? (
                      <span className="tag-chips">
                        {r.tags.map((t) => (
                          <span key={t} className="ref-tag">
                            {t}
                          </span>
                        ))}
                      </span>
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                  <td>{r.description}</td>
                  <td>
                    <span className="runner-status">
                      <span
                        className={r.online ? "mini-dot mini-success" : "mini-dot"}
                      />
                      {r.online ? "online" : "offline"}
                      {r.paused && <span className="muted"> (paused)</span>}
                    </span>
                  </td>
                  <td>
                    {(loadById.get(String(r.id))?.running_jobs ?? []).map((j) => (
                      <div key={j.job_id}>
                        <Link to={`/jobs/${j.job_id}?pipeline=${j.pipeline_id}`}>{j.name}</Link>{" "}
                        <span className="muted">{j.repo}</span>
                      </div>
                    ))}
                    {!loadById.get(String(r.id))?.running && <span className="muted">idle</span>}
                  </td>
                  <td className="muted">
                    {(() => {
                      const l = loadById.get(String(r.id));
                      if (!l) return "—";
                      return `${l.jobs_24h} jobs · ${Math.round((l.busy_seconds_24h / 86400) * 100)}% busy`;
                    })()}
                  </td>
                  <td className="muted">{relativeTime(r.last_contact_at)}</td>
                  <td>
                    <button
                      type="button"
                      className="btn"
                      onClick={() => void togglePause(r.id, !r.paused)}
                    >
                      {r.paused ? "Resume" : "Pause"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function waitLabel(sec: number): string {
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m`;
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`;
}
