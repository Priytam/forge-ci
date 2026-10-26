import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { editChange, getChange, relativeTime, shortSha, type ChangePage } from "../api";
import StatusBadge from "../components/StatusBadge";
import ChangeStateBadge from "../components/ChangeStateBadge";
import Avatar from "../components/Avatar";
import MergeGate from "../components/MergeGate";
import DiffView from "../components/DiffView";
import Conversation from "../components/Conversation";
import { useChangeStream } from "../hooks/useChangeStream";
import RepoHeader from "../components/RepoHeader";

const VERDICT: Record<string, string> = {
  approve: "approved",
  request_changes: "requested changes",
  comment: "commented",
  dismissed: "review dismissed",
};

/**
 * One change: what it is, its CI, every revision (each push) and every
 * review. Rendered from Forge's copy; what the page offers depends on the
 * host's capabilities, never on which host it is.
 */
export default function ChangeDetail() {
  const { id } = useParams();
  const [page, setPage] = useState<ChangePage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<{ title: string; body: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [editError, setEditError] = useState<string | null>(null);

  const load = useCallback(() => {
    if (!id) return;
    getChange(id)
      .then((r) => {
        setPage(r);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [id]);
  useEffect(load, [load]);
  useChangeStream(load, id ? Number(id) : undefined);

  if (error) return <div className="error-banner">{error}</div>;
  if (!page) return <div className="muted">Loading…</div>;
  const { change, capabilities: caps, permissions } = page;
  // Forge-native changes are edited, closed and reopened here (they have no
  // host page).
  const save = async (patch: Parameters<typeof editChange>[1]) => {
    setBusy(true);
    setEditError(null);
    try {
      await editChange(change.id, patch);
      setEditing(null);
      load();
    } catch (e) {
      setEditError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <RepoHeader repo={change.repo} tab="changes" />
      <div className="page-head">
        <h2 className="change-title">
          {change.title || "(untitled)"} <span className="mono muted">#{change.number}</span>
        </h2>
        <div className="page-head-actions">
          {permissions.can_edit && change.state !== "merged" && !editing && (
            <button className="btn" onClick={() => setEditing({ title: change.title, body: change.body })}>
              Edit
            </button>
          )}
          {permissions.can_edit && change.state === "open" && (
            <button className="btn" disabled={busy} onClick={() => void save({ state: "closed" })}>
              Close
            </button>
          )}
          {permissions.can_edit && change.state === "closed" && (
            <button className="btn" disabled={busy} onClick={() => void save({ state: "open" })}>
              Reopen
            </button>
          )}
          {change.url && (
            <a className="btn" href={change.url} target="_blank" rel="noreferrer">
              Open on host
            </a>
          )}
        </div>
      </div>
      <p className="page-lede">
        <ChangeStateBadge change={change} /> <Avatar identity={change.author} size={16} /> <b>{change.author}</b> wants to
        merge <span className="mono">{change.source_branch}</span> into <span className="mono">{change.target_branch}</span>
        {caps.native_changes && <span className="muted"> · a Forge-native change (the host has no pull requests)</span>}
        {change.updated_at && <span className="muted"> · updated {relativeTime(change.updated_at)}</span>}
      </p>
      {editing && (
        <div className="card settings-form">
          <label className="field">
            <span>Title</span>
            <input value={editing.title} onChange={(e) => setEditing({ ...editing, title: e.target.value })} />
          </label>
          <label className="field">
            <span>Description</span>
            <textarea rows={6} value={editing.body} onChange={(e) => setEditing({ ...editing, body: e.target.value })} />
          </label>
          <div className="form-actions">
            <button
              className="btn btn-primary"
              disabled={busy || !editing.title.trim()}
              onClick={() => void save({ title: editing.title, body: editing.body })}
            >
              Save
            </button>
            <button className="btn" disabled={busy} onClick={() => setEditing(null)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {editError && <div className="error-banner">{editError}</div>}
      {change.refresh_error && (
        <div className="error-banner">Last sync with the host failed: {change.refresh_error}. Showing the last good copy.</div>
      )}

      <MergeGate page={page} onChanged={load} />
      <Conversation page={page} onChanged={load} />
      {caps.diffs && <DiffView page={page} onChanged={load} />}

      <div className="card settings-form" style={{ marginTop: 16 }}>
        <h2>Checks</h2>
        {change.pipeline_id ? (
          <p>
            <StatusBadge status={change.pipeline_status ?? "created"} />{" "}
            <Link to={`/pipelines/${change.pipeline_id}`}>Pipeline #{change.pipeline_id}</Link> on{" "}
            <span className="mono">{shortSha(change.head_sha)}</span>
          </p>
        ) : (
          <p className="muted">No Forge pipeline has run on the head commit ({shortSha(change.head_sha)}) yet.</p>
        )}
        {change.labels.length > 0 && <p className="muted">Labels: {change.labels.join(", ")}</p>}
      </div>

      {change.body && (
        <div className="card settings-form" style={{ marginTop: 16 }}>
          <h2>Description</h2>
          <pre className="change-body">{change.body}</pre>
        </div>
      )}

      <div className="card settings-form" style={{ marginTop: 16 }}>
        <h2>
          Reviews <span className="muted">· {change.approvals} approved{change.changes_requested > 0 && `, ${change.changes_requested} want changes`}</span>
        </h2>
        {change.reviews.length === 0 ? (
          <p className="muted">No reviews yet.</p>
        ) : (
          <table className="dense-table">
            <tbody>
              {change.reviews.map((r) => (
                <tr key={r.id}>
                  <td style={{ width: 220 }}>
                    <Avatar identity={r.reviewer} size={16} /> {r.reviewer}
                  </td>
                  <td>
                    <span className={r.verdict === "request_changes" ? "error-text" : r.verdict === "approve" ? "" : "muted"}>
                      {VERDICT[r.verdict] ?? r.verdict}
                    </span>
                    {r.commit_sha && r.commit_sha !== change.head_sha && <span className="muted"> · on an earlier revision</span>}
                    {r.body && <div className="muted">{r.body}</div>}
                  </td>
                  <td className="muted">{r.source === "forge" ? "in Forge" : "on the host"}</td>
                  <td className="muted" style={{ textAlign: "right" }}>
                    {relativeTime(r.submitted_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="card settings-form" style={{ marginTop: 16 }}>
        <h2>Revisions</h2>
        <p className="muted">Every push is a revision, on every host.</p>
        <table className="dense-table">
          <tbody>
            {change.revision_list.map((r) => (
              <tr key={r.number}>
                <td style={{ width: 60 }}>r{r.number}</td>
                <td className="mono">{shortSha(r.head_sha)}</td>
                <td className="muted">base {shortSha(r.base_sha)}</td>
                <td className="muted" style={{ textAlign: "right" }}>
                  {relativeTime(r.pushed_at)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
