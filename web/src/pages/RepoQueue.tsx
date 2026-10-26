import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { decodeRepoParam, listMergeQueue, relativeTime, type QueueEntry } from "../api";
import Avatar, { displayName } from "../components/Avatar";
import Icon from "../components/Icon";
import RepoHeader, { repoTabPath } from "../components/RepoHeader";
import { QueueSteps } from "../components/review/ui";
import { Skeleton } from "../components/ui/Pill";
import { useChangeStream } from "../hooks/useChangeStream";

/**
 * A repo's merge queue: per target branch, the changes waiting to be tested
 * on top of the latest target and merged, in order. Live: the cards move as
 * the queue does.
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
    <div className="cr-page">
      <RepoHeader repo={repo} tab="queue" />
      {error && <div className="error-banner">{error}</div>}
      {entries === null && !error && (
        <div className="table-skeleton" aria-busy="true">
          <Skeleton h={64} />
          <Skeleton h={64} />
        </div>
      )}
      {entries !== null && entries.length === 0 && (
        <div className="card cr-empty">
          <Icon name="layers" size={24} />
          <div className="cr-empty-title">Nothing is waiting to merge</div>
          <div>
            A branch that merges through the queue tests each change on top of the latest target before merging it, so
            two changes that pass alone can't break it together. Turn it on in a{" "}
            <Link to={repoTabPath(repo, "rules")}>merge rule</Link>, then use Add to merge queue on a change.
          </div>
        </div>
      )}
      {targets.map((t) => {
        const list = entries!.filter((e) => e.target_branch === t);
        return (
          <section key={t} className="cr-queue-group">
            <div className="cr-queue-head">
              <Icon name="layers" size={14} /> Into <span className="cr-ref">{t}</span>
              <span className="muted" style={{ fontWeight: 400 }}>
                · {list.length} in line · tested on <span className="mono">forge-queue/{t}</span>
              </span>
            </div>
            {list.map((e, i) => (
              <div key={e.id} className={i === 0 ? "cr-qcard cr-qcard-first" : "cr-qcard"}>
                <span className="cr-qpos">{e.position ?? i + 1}</span>
                <Link className="cr-qtitle" to={`/changes/${e.change_id}`}>
                  <span className="mono muted">#{e.number}</span> {e.title || "(untitled)"}
                </Link>
                <QueueSteps entry={e} />
                <div className="cr-qmeta">
                  {e.requested_by && (
                    <span className="cr-who">
                      <Avatar identity={e.requested_by} size={14} /> added by {displayName(e.requested_by)}
                    </span>
                  )}
                  <span>waiting {relativeTime(e.enqueued_at).replace(" ago", "")}</span>
                  {e.pipeline_id && <Link to={`/pipelines/${e.pipeline_id}`}>test pipeline #{e.pipeline_id}</Link>}
                  {e.error && <span className="error-text">{e.error}</span>}
                </div>
              </div>
            ))}
          </section>
        );
      })}
    </div>
  );
}
