import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { listChanges, relativeTime, type Change } from "../api";
import StatusBadge from "../components/StatusBadge";
import ChangeStateBadge from "../components/ChangeStateBadge";
import GateBadge from "../components/GateBadge";
import Avatar from "../components/Avatar";
import NewChange from "../components/NewChange";
import { useChangeStream } from "../hooks/useChangeStream";

/**
 * The inbox: open pull/merge requests across every repo and host, in one
 * list. It renders from Forge's own copy (kept current by webhooks), so
 * opening it never calls GitHub/GitLab/…; updates arrive live.
 */
export default function Changes() {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const state = params.get("state") ?? "open";
  const repo = params.get("repo") ?? "";
  const [q, setQ] = useState(params.get("q") ?? "");
  const [changes, setChanges] = useState<Change[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const load = useCallback(() => {
    listChanges({ state, repo, q: params.get("q") ?? "" })
      .then((r) => {
        setChanges(r.changes);
        setTotal(r.total);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [state, repo, params]);
  useEffect(load, [load]);
  useChangeStream(load);

  const set = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };
  const repos = Array.from(new Set((changes ?? []).map((c) => c.repo))).sort();

  return (
    <div>
      <div className="page-head">
        <h1>Changes</h1>
        <div className="page-head-actions">
          <button className="btn btn-primary" onClick={() => setCreating(true)} disabled={creating}>
            New change
          </button>
        </div>
      </div>
      {creating && <NewChange onClose={() => setCreating(false)} />}
      <p className="page-lede">
        Pull and merge requests from every connected repo, whichever host it lives on. This list is kept
        current by webhooks — opening it never calls the host.
      </p>
      <div className="filter-bar" style={{ marginBottom: 12 }}>
        <label className="field">
          <span>State</span>
          <select value={state} onChange={(e) => set("state", e.target.value === "open" ? "" : e.target.value)}>
            <option value="open">Open</option>
            <option value="merged">Merged</option>
            <option value="closed">Closed</option>
            <option value="all">All</option>
          </select>
        </label>
        <label className="field">
          <span>Repo</span>
          <select value={repo} onChange={(e) => set("repo", e.target.value)}>
            <option value="">All repos</option>
            {(repo && !repos.includes(repo) ? [repo, ...repos] : repos).map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </label>
        <form
          className="field filter-search"
          onSubmit={(e) => {
            e.preventDefault();
            set("q", q.trim());
          }}
        >
          <span>Search</span>
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Title or #number" />
        </form>
      </div>
      {error && <div className="error-banner">{error}</div>}
      {changes === null ? (
        <div className="muted">Loading…</div>
      ) : changes.length === 0 ? (
        <div className="card empty">
          No {state === "all" ? "" : state} changes. Connected GitHub repos fill this list from their
          pull-request webhooks.
        </div>
      ) : (
        <div className="card table-card">
          <table className="dense-table">
            <thead>
              <tr>
                <th style={{ width: 96 }}>CI</th>
                <th style={{ width: 96 }}>Gate</th>
                <th>Change</th>
                <th>Repo</th>
                <th>Branch</th>
                <th>Reviews</th>
                <th style={{ textAlign: "right" }}>Updated</th>
              </tr>
            </thead>
            <tbody>
              {changes.map((c) => (
                <tr key={c.id} className="row-link" onClick={() => navigate(`/changes/${c.id}`)}>
                  <td>{c.pipeline_status ? <StatusBadge status={c.pipeline_status} /> : <span className="muted">—</span>}</td>
                  <td>
                    <GateBadge change={c} />
                  </td>
                  <td className="cell-title">
                    <span className="mono muted">#{c.number}</span> {c.title || <span className="muted">(untitled)</span>}{" "}
                    {(c.draft || c.state !== "open") && <ChangeStateBadge change={c} />}
                    <span className="cell-meta">
                      {" "}
                      · <Avatar identity={c.author} size={14} /> {c.author}
                      {c.labels.length > 0 && <> · {c.labels.join(", ")}</>}
                    </span>
                  </td>
                  <td className="cell-repo">{c.repo}</td>
                  <td>
                    <span className="mono cell-branch" title={`${c.source_branch} → ${c.target_branch}`}>
                      {c.source_branch} → {c.target_branch}
                    </span>
                  </td>
                  <td>
                    {c.approvals > 0 && <span title="approvals">✓ {c.approvals}</span>}{" "}
                    {c.changes_requested > 0 && (
                      <span className="error-text" title="changes requested">
                        ✗ {c.changes_requested}
                      </span>
                    )}
                    {c.approvals === 0 && c.changes_requested === 0 && <span className="muted">none</span>}
                  </td>
                  <td className="muted" style={{ textAlign: "right" }}>
                    {c.updated_at ? relativeTime(c.updated_at) : ""}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {total > changes.length && (
            <div className="muted" style={{ padding: 8 }}>
              Showing {changes.length} of {total}. Narrow the filters to see the rest.
            </div>
          )}
        </div>
      )}
    </div>
  );
}
