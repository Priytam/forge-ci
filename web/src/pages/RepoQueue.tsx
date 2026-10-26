import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { decodeRepoParam, listMergeQueue, relativeTime, type QueueEntry } from "../api";
import RepoHeader, { repoTabPath } from "../components/RepoHeader";
import { useChangeStream } from "../hooks/useChangeStream";

const STATE: Record<QueueEntry["state"], string> = {
  queued: "waiting",
  testing: "testing",
  merging: "merging",
  merged: "merged",
  failed: "failed",
  removed: "removed",
};

/**
 * A repo's merge queue: per target branch, the changes waiting to be tested
 * on top of the latest target and merged, in order. Live: the page follows
 * the queue as it moves.
 */
export default function RepoQueue() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");
  const [entries, setEntries] = useState<QueueEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(() => {
    listMergeQueue(repo)
      .then((e) => {
        setEntries(e);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [repo]);
  useEffect(load, [load]);
  useChangeStream(load);

  const targets = Array.from(new Set((entries ?? []).map((e) => e.target_branch))).sort();

  return (
    <div>
      <RepoHeader repo={repo} tab="queue" />
      <p className="page-lede">
        Changes into a branch that merges through the queue are tested one at a time, merged on top of the latest
        target (on <span className="mono">forge-queue/&lt;branch&gt;</span>), and merged only if that passes. Turn it on
        for a branch in its <Link to={repoTabPath(repo, "rules")}>merge rule</Link>.
      </p>
      {error && <div className="error-banner">{error}</div>}
      {entries === null && !error && <div className="muted">Loading…</div>}
      {entries !== null && entries.length === 0 && (
        <div className="card empty">Nothing is waiting to merge. Add a change from its page (Add to merge queue).</div>
      )}
      {targets.map((t) => (
        <div key={t} className="card table-card" style={{ marginBottom: 16 }}>
          <h2 style={{ padding: "12px 12px 0" }}>
            Into <span className="mono">{t}</span>
          </h2>
          <table className="dense-table">
            <thead>
              <tr>
                <th style={{ width: 40 }}>#</th>
                <th>Change</th>
                <th style={{ width: 160 }}>State</th>
                <th>Added by</th>
                <th style={{ textAlign: "right" }}>Waiting</th>
              </tr>
            </thead>
            <tbody>
              {entries!
                .filter((e) => e.target_branch === t)
                .map((e) => (
                  <tr key={e.id}>
                    <td className="muted">{e.position}</td>
                    <td className="cell-title">
                      <Link to={`/changes/${e.change_id}`}>
                        <span className="mono muted">#{e.number}</span> {e.title || "(untitled)"}
                      </Link>
                    </td>
                    <td>
                      {STATE[e.state]}
                      {e.pipeline_id && (
                        <>
                          {" "}
                          · <Link to={`/pipelines/${e.pipeline_id}`}>#{e.pipeline_id}</Link>
                        </>
                      )}
                    </td>
                    <td className="muted">{e.requested_by || "—"}</td>
                    <td className="muted" style={{ textAlign: "right" }}>
                      {relativeTime(e.enqueued_at)}
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  );
}
