import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  getEnvironmentBoard,
  relativeTime,
  shortSha,
  type EnvCell,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import EnvTabs, { envKind } from "../components/EnvTabs";

const PAGE = 50;

/** What a cell needs to say first: the latest attempt when it differs from
 *  what is deployed (in flight, waiting, failed), otherwise the deployment. */
function Cell({ repo, env, cell }: { repo: string; env: string; cell?: EnvCell }) {
  if (!cell || (!cell.current && !cell.latest)) {
    return <span className="muted">—</span>;
  }
  const cur = cell.current;
  const latest = cell.latest;
  const pending =
    latest && (!cur || latest.job_id !== cur.job_id) && latest.status !== "success" ? latest : null;
  const label =
    pending?.status === "blocked" ? (pending.manual ? "manual" : "approval") : pending?.status;
  return (
    <div className={`env-cell${pending?.status === "failed" ? " env-cell-failed" : ""}`}>
      {cur ? (
        <Link
          to={`/repos/${encodeURIComponent(repo)}/environments`}
          className="env-cell-current"
          title={`${cur.ref} @ ${cur.sha}, deployed by ${cur.deployed_by || "unknown"}`}
        >
          <span className="mono">{shortSha(cur.sha)}</span>{" "}
          <span className="muted">{cur.ref}</span>
          <div className="muted env-cell-when">{relativeTime(cur.deployed_at)}</div>
        </Link>
      ) : (
        <span className="muted">never deployed</span>
      )}
      {pending && (
        <Link
          to={`/jobs/${pending.job_id}?pipeline=${pending.pipeline_id}`}
          className={`badge badge-${pending.status === "blocked" ? "blocked" : pending.status}`}
          title={`${pending.job_name} on ${pending.ref} @ ${shortSha(pending.sha)} — ${relativeTime(pending.created_at)}`}
        >
          {label}
        </Link>
      )}
      {cell.frozen && (
        <span className="badge badge-canceled" title={`Deploys to ${env} are frozen`}>
          frozen
        </span>
      )}
    </div>
  );
}

/**
 * The org at a glance: one row per repo, one column per environment in
 * promotion order. Each cell shows what is deployed and, if newer, what is
 * in flight, waiting or failing. Search, environment and "needs attention"
 * filters and paging all run on the server, so it stays usable at hundreds
 * of repos.
 */
export default function EnvironmentBoard() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const env = params.get("env") ?? "";
  const attention = params.get("attention") === "1";
  const page = Math.max(0, Number(params.get("page") ?? 0) || 0);
  const key = params.toString();

  const set = (changes: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(changes)) {
      if (v) next.set(k, v);
      else next.delete(k);
    }
    if (!("page" in changes)) next.delete("page");
    setParams(next);
  };

  const fetcher = useCallback(
    () => getEnvironmentBoard({ q, env, attention }, page * PAGE, PAGE),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key]
  );
  const { data, error, loading, refresh } = usePoll(fetcher, 5000);
  useEffect(() => refresh(), [key, refresh]);

  const [search, setSearch] = useState(q);
  useEffect(() => setSearch(q), [q]);
  useEffect(() => {
    if (search === q) return;
    const t = window.setTimeout(() => set({ q: search }), 300);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);

  const envs = data?.environments ?? [];
  // With an environment picked, show just that column.
  const columns = env ? envs.filter((e) => e === env) : envs;

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>environments</span>
      </div>
      <div className="page-head">
        <h1>Environments</h1>
      </div>
      <p className="muted page-lede">
        What is deployed where, per repo. A badge means the newest deploy attempt
        differs from what is live: still running, waiting for approval or a manual
        play, or failed.
      </p>

      <EnvTabs envs={envs} value={env} onChange={(e) => set({ env: e })} />
      <div className="card filter-panel">
        <div className="filter-bar">
          <input
            className="filter-search"
            placeholder="Find a repo…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            aria-label="Find a repo"
          />
          <label className="check-row">
            <input
              type="checkbox"
              checked={attention}
              onChange={(e) => set({ attention: e.target.checked ? "1" : "" })}
            />
            Needs attention only (failed, waiting, canceled)
          </label>
        </div>
      </div>

      {error && <div className="error-banner">Failed to load: {error}</div>}
      {loading && !data && <div className="muted">Loading…</div>}
      {data && data.rows.length === 0 && (
        <div className="empty card">
          {q || env || attention
            ? "No repos match."
            : "No deployments yet. Jobs with an environment: show up here."}
        </div>
      )}
      {data && data.rows.length > 0 && (
        <div className="card table-card env-board">
          <table>
            <thead>
              <tr>
                <th>Repo</th>
                {columns.map((e) => (
                  <th key={e} className={`env-col env-col-${envKind(e)}`}>
                    {e}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {data.rows.map((row) => (
                <tr key={row.repo}>
                  <td>
                    <Link to={`/repos/${encodeURIComponent(row.repo)}`}>{row.repo}</Link>
                  </td>
                  {columns.map((e) => (
                    <td key={e}>
                      <Cell repo={row.repo} env={e} cell={row.cells[e]} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {data && data.total > 0 && (
        <div className="filter-pager">
          <span className="muted">
            {page * PAGE + 1}–{page * PAGE + data.rows.length} of {data.total} repos
          </span>
          <button
            type="button"
            className="btn"
            disabled={page === 0}
            onClick={() => set({ page: String(page - 1) })}
          >
            ← Prev
          </button>
          <button
            type="button"
            className="btn"
            disabled={!data.hasMore}
            onClick={() => set({ page: String(page + 1) })}
          >
            Next →
          </button>
        </div>
      )}
    </div>
  );
}
