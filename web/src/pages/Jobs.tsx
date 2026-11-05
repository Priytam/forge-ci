import Skeleton from "../components/ui/Skeleton";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  filterToParams,
  getPipelineFacets,
  JOB_STATUSES,
  listJobsPage,
  paramsToFilter,
  relativeTime,
  shortSha,
  type JobFilter,
  type PipelineFacets,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import EnvTabs from "../components/EnvTabs";
import { STATUS_LABEL } from "../components/StatusBadge";

const PAGE = 50;

function duration(start: string | null, end: string | null): string {
  if (!start) return "";
  const ms = (end ? new Date(end).getTime() : Date.now()) - new Date(start).getTime();
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
}

/**
 * Every job across every pipeline, newest first — "what is running on prod",
 * "what is queued on runner X", "which deploys failed today". Filters live in
 * the URL and are applied on the server.
 */
export default function Jobs() {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const filter: JobFilter = useMemo(
    () => ({ ...paramsToFilter(params), name: params.get("name") ?? "" }),
    [params]
  );
  const page = Math.max(0, Number(params.get("page") ?? 0) || 0);
  const key = params.toString();

  const setFilter = (f: JobFilter) => {
    const next = filterToParams(f);
    if (f.name) next.set("name", f.name);
    setParams(next);
  };
  const patch = (p: Partial<JobFilter>) => setFilter({ ...filter, ...p });

  const fetcher = useCallback(
    () => listJobsPage(filter, page * PAGE, PAGE),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key]
  );
  const { data, error, loading, refresh } = usePoll(fetcher, 3000);
  useEffect(() => refresh(), [key, refresh]);

  const [facets, setFacets] = useState<PipelineFacets | null>(null);
  const repoKey = (filter.repos ?? []).join(",");
  useEffect(() => {
    void getPipelineFacets(filter.repos ?? [])
      .then(setFacets)
      .catch(() => setFacets(null));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [repoKey]);

  // Debounce the name box so typing doesn't refetch per keystroke.
  const [name, setName] = useState(filter.name ?? "");
  useEffect(() => setName(filter.name ?? ""), [filter.name]);
  useEffect(() => {
    if (name === (filter.name ?? "")) return;
    const t = window.setTimeout(() => patch({ name }), 300);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  const statuses = filter.statuses ?? [];
  const anyFilter = params.toString().replace(/(^|&)page=\d+/, "") !== "";
  const setPage = (n: number) => {
    const next = new URLSearchParams(params);
    next.set("page", String(n));
    setParams(next);
  };

  return (
    <div>
      <div className="page-head">
        <h1>Jobs</h1>
      </div>

      <EnvTabs
        envs={facets?.environments ?? []}
        value={filter.environment ?? ""}
        onChange={(environment) => patch({ environment })}
      />
      <div className="card filter-panel">
        <div className="filter-bar">
          <select
            value={(filter.repos ?? [])[0] ?? ""}
            onChange={(e) => patch({ repos: e.target.value ? [e.target.value] : [] })}
            aria-label="Repo"
          >
            <option value="">All repos</option>
            {facets?.repos.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
          <select
            value={filter.runner ?? ""}
            onChange={(e) => patch({ runner: e.target.value })}
            aria-label="Runner"
          >
            <option value="">All runners</option>
            {facets?.runners.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
          <input
            className="filter-search"
            placeholder="Job name contains…"
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-label="Job name"
          />
          <div className="state-chips" role="group" aria-label="Status">
            {JOB_STATUSES.map((st) => (
              <button
                key={st}
                type="button"
                className={`fchip fdot-${st}${statuses.includes(st) ? " fchip-on" : ""}`}
                aria-pressed={statuses.includes(st)}
                onClick={() =>
                  patch({
                    statuses: statuses.includes(st)
                      ? statuses.filter((x) => x !== st)
                      : [...statuses, st],
                  })
                }
              >
                {STATUS_LABEL[st] ?? st}
              </button>
            ))}
          </div>
          {anyFilter && (
            <button type="button" className="btn btn-ghost" onClick={() => setFilter({})}>
              Clear filters
            </button>
          )}
        </div>
      </div>

      {error && <div className="error-banner">Failed to load jobs: {error}</div>}
      {loading && !data && <Skeleton />}
      {data && data.items.length === 0 && (
        <div className="empty card">
          {anyFilter ? "No jobs match these filters." : "No jobs yet."}
        </div>
      )}
      {data && data.items.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Status</th>
                <th>Job</th>
                <th>Repo</th>
                <th>Ref</th>
                <th>Environment</th>
                <th>Runner</th>
                <th>Duration</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((j) => (
                <tr
                  key={j.id}
                  className="row-link"
                  onClick={() => navigate(`/jobs/${j.id}?pipeline=${j.pipeline_id}`)}
                >
                  <td>
                    <StatusBadge status={j.manual && j.status === "blocked" ? "manual" : j.status} />
                    {j.status_reason && <div className="status-reason">{j.status_reason}</div>}
                  </td>
                  <td>
                    <div>{j.name}</div>
                    <div className="muted mono">
                      #{j.pipeline_id} · {j.stage}
                    </div>
                  </td>
                  <td>{j.repo}</td>
                  <td>
                    {j.ref} <span className="mono muted">{shortSha(j.sha)}</span>
                  </td>
                  <td>{j.environment && <span className="env-tag">{j.environment}</span>}</td>
                  <td className="mono muted">{j.runner_id ?? ""}</td>
                  <td className="muted">{duration(j.started_at, j.finished_at)}</td>
                  <td className="muted">{relativeTime(j.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {data && data.total > 0 && (
        <div className="filter-pager">
          <span className="muted">
            {page * PAGE + 1}–{page * PAGE + data.items.length} of {data.total}
          </span>
          <button type="button" className="btn" disabled={page === 0} onClick={() => setPage(page - 1)}>
            ← Newer
          </button>
          <button type="button" className="btn" disabled={!data.hasMore} onClick={() => setPage(page + 1)}>
            Older →
          </button>
        </div>
      )}
    </div>
  );
}
