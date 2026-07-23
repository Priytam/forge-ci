import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { listRunners, relativeTime, setRunnerPaused } from "../api";
import { usePoll } from "../hooks/usePoll";

interface TagStat {
  tag: string;
  online: number;
  total: number;
}

export default function Runners() {
  const { data: runners, error, loading, refresh } = usePoll(listRunners, 5000);
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
