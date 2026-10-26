import { Link } from "react-router-dom";
import { shortSha, type ChangePage } from "../../api";
import Avatar, { displayName } from "../Avatar";
import Icon from "../Icon";
import { CIPill, QueueSteps, RuleMark } from "./ui";

/** The change at a glance, beside its conversation: what the merge waits
 *  for, who reviewed, the queue, CI, branches and labels. */
export default function ChangeSidebar({ page }: { page: ChangePage }) {
  const { change, gate, queue } = page;
  // Each person's latest verdict.
  const latest = new Map<string, (typeof change.reviews)[number]>();
  for (const r of [...change.reviews].sort((a, b) => a.submitted_at.localeCompare(b.submitted_at))) {
    if (r.verdict !== "comment") latest.set(r.reviewer.toLowerCase(), r);
  }
  const reviewers = [...latest.values()].filter((r) => r.verdict !== "dismissed");
  const entry = queue?.entry;

  return (
    <aside className="cr-side" aria-label="About this change">
      <section className="cr-side-sec">
        <div className="cr-side-title">Merge checklist</div>
        {gate ? (
          gate.rules.map((r) => (
            <div key={r.id} className="cr-side-row">
              <RuleMark state={r.state} />
              <span className="cr-rule-label">
                {r.label}
                <span className="cr-rule-detail">{r.detail}</span>
              </span>
            </div>
          ))
        ) : (
          <div className="muted">No merge rules apply to {change.target_branch}.</div>
        )}
      </section>

      <section className="cr-side-sec">
        <div className="cr-side-title">Reviewers</div>
        {reviewers.length === 0 && <div className="muted">Nobody has reviewed it yet.</div>}
        {reviewers.map((r) => (
          <div key={r.id} className="cr-side-row">
            <Avatar identity={r.reviewer} size={18} />
            <span className="cr-rule-label">
              {displayName(r.reviewer)}
              {r.commit_sha && r.commit_sha !== change.head_sha && (
                <span className="cr-rule-detail">on an earlier revision</span>
              )}
            </span>
            <span
              className={r.verdict === "approve" ? "cr-vote-ok" : "cr-vote-no"}
              title={r.verdict === "approve" ? "approved" : "requested changes"}
            >
              <Icon name={r.verdict === "approve" ? "check" : "close"} size={14} />
            </span>
          </div>
        ))}
      </section>

      {queue?.enabled && (
        <section className="cr-side-sec">
          <div className="cr-side-title">
            Merge queue
            {queue.length > 0 && <span className="muted">{queue.length} in line</span>}
          </div>
          {entry ? (
            <>
              <QueueSteps entry={entry} />
              {entry.error && (
                <div className="cr-rule-detail" style={{ marginTop: 6 }}>
                  {entry.error}
                </div>
              )}
              {entry.pipeline_id && (
                <div style={{ marginTop: 6 }}>
                  <Link to={`/pipelines/${entry.pipeline_id}`}>Test pipeline #{entry.pipeline_id}</Link>
                </div>
              )}
            </>
          ) : (
            <div className="muted">Not in the queue. {change.target_branch} merges through it.</div>
          )}
        </section>
      )}

      <section className="cr-side-sec">
        <div className="cr-side-title">CI</div>
        {change.pipeline_id ? (
          <div className="cr-side-row">
            <CIPill status={change.pipeline_status} />
            <Link to={`/pipelines/${change.pipeline_id}`}>#{change.pipeline_id}</Link>
          </div>
        ) : (
          <div className="muted">No Forge pipeline on {shortSha(change.head_sha)} yet.</div>
        )}
      </section>

      <section className="cr-side-sec">
        <div className="cr-side-title">Branches</div>
        <div className="cr-side-row">
          <span className="cr-rule-label">
            <span className="cr-ref">{change.source_branch}</span>
            <span className="cr-rule-detail">head {shortSha(change.head_sha)}</span>
          </span>
        </div>
        <div className="cr-side-row">
          <span className="cr-rule-label">
            <span className="cr-ref">{change.target_branch}</span>
            <span className="cr-rule-detail">target {shortSha(change.base_sha)}</span>
          </span>
        </div>
      </section>

      {change.labels.length > 0 && (
        <section className="cr-side-sec">
          <div className="cr-side-title">Labels</div>
          <div className="chips">
            {change.labels.map((l) => (
              <span key={l} className="chip">
                {l}
              </span>
            ))}
          </div>
        </section>
      )}
    </aside>
  );
}
