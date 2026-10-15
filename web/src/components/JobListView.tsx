import { useMemo } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { duration, type Job } from "../api";
import StatusBadge from "./StatusBadge";
import EnvTabs from "./EnvTabs";

/**
 * One pipeline's jobs as a flat, filterable table — for pipelines with too
 * many jobs to scan on the stage board. Filters (environment, status, stage,
 * name) live in the URL (?jenv, ?jstatus, ?jstage, ?jq) and run in the
 * browser: one pipeline's jobs are already loaded.
 */
export default function JobListView({ jobs, pipelineId }: { jobs: Job[]; pipelineId: number }) {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const env = params.get("jenv") ?? "";
  const status = params.get("jstatus") ?? "";
  const stage = params.get("jstage") ?? "";
  const q = params.get("jq") ?? "";

  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };

  const envs = useMemo(
    () => [...new Set(jobs.map((j) => j.environment).filter((e): e is string => !!e))],
    [jobs]
  );
  const stages = useMemo(() => {
    const seen = new Map<string, number>();
    for (const j of jobs) if (!seen.has(j.stage)) seen.set(j.stage, j.stage_idx);
    return [...seen.entries()].sort((a, b) => a[1] - b[1]).map(([s]) => s);
  }, [jobs]);
  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const j of jobs) c[j.status] = (c[j.status] ?? 0) + 1;
    return c;
  }, [jobs]);

  const shown = jobs.filter(
    (j) =>
      (!env || j.environment === env) &&
      (!status || j.status === status) &&
      (!stage || j.stage === stage) &&
      (!q || j.name.toLowerCase().includes(q.toLowerCase()))
  );

  return (
    <div className="job-list-view">
      <EnvTabs envs={envs} value={env} onChange={(e) => set("jenv", e)} allLabel="All jobs" />
      <div className="card filter-panel">
        <div className="filter-bar">
          <input
            className="filter-search"
            placeholder="Job name contains…"
            value={q}
            onChange={(e) => set("jq", e.target.value)}
            aria-label="Job name"
          />
          <select value={stage} onChange={(e) => set("jstage", e.target.value)} aria-label="Stage">
            <option value="">All stages</option>
            {stages.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
          <div className="segmented" role="group" aria-label="Status">
            <button
              type="button"
              className={status === "" ? "seg-btn seg-active" : "seg-btn"}
              onClick={() => set("jstatus", "")}
            >
              all {jobs.length}
            </button>
            {Object.entries(counts).map(([st, n]) => (
              <button
                key={st}
                type="button"
                className={status === st ? "seg-btn seg-active" : "seg-btn"}
                onClick={() => set("jstatus", status === st ? "" : st)}
              >
                {st} {n}
              </button>
            ))}
          </div>
        </div>
      </div>
      {shown.length === 0 ? (
        <div className="empty card">No jobs match.</div>
      ) : (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Job</th>
                <th>Stage</th>
                <th>Environment</th>
                <th>Status</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((j) => (
                <tr
                  key={j.id}
                  className="row-link"
                  onClick={() => navigate(`/jobs/${j.id}?pipeline=${pipelineId}`)}
                >
                  <td>
                    {j.name}
                    {j.allow_failure && <span className="muted"> (allowed to fail)</span>}
                  </td>
                  <td className="muted">{j.stage}</td>
                  <td>{j.environment && <span className="env-tag">{j.environment}</span>}</td>
                  <td>
                    <StatusBadge status={j.manual && j.status === "blocked" ? "manual" : j.status} />
                    {j.status_reason && <div className="status-reason">{j.status_reason}</div>}
                  </td>
                  <td className="muted">{duration(j.started_at, j.finished_at) ?? ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
