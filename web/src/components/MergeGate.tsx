import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  dequeueChange,
  getMe,
  mergeChange,
  queueChange,
  relativeTime,
  reviewChange,
  type ChangePage,
  type QueueEntry,
} from "../api";

const MARK: Record<string, string> = { pass: "✓", pending: "…", fail: "✗" };

const METHOD_LABELS: Record<string, string> = {
  merge: "Merge commit",
  squash: "Squash and merge",
  rebase: "Rebase and merge",
  fast_forward: "Fast-forward",
};

/** Where a change stands in its merge queue, in words. */
function queueStatus(e: QueueEntry, target: string): string {
  switch (e.state) {
    case "queued":
      return e.position && e.position > 1 ? `In the merge queue — position ${e.position}` : "Next in the merge queue";
    case "testing":
      return e.pipeline_id ? `Testing on top of ${target}` : `Merging onto ${target} to test…`;
    case "merging":
      return "Tested — merging…";
    case "merged":
      return "Merged by the merge queue";
    case "failed":
      return `Left the merge queue: ${e.error ?? "failed"}`;
    default:
      return `Removed from the merge queue${e.error ? ` (${e.error})` : ""}`;
  }
}

/**
 * Forge's rules between the change and its merge: each rule's progress, the
 * caller's review buttons, and Merge. What is offered depends on the host's
 * capabilities and the caller's permissions, never on which host it is.
 * Writes are saved at once and reach the host through the outbox, so the
 * page shows them as queued until the host confirms.
 */
export default function MergeGate({ page, onChanged }: { page: ChangePage; onChanged: () => void }) {
  const { change, capabilities: caps, gate, actions, permissions, queue } = page;
  const [me, setMe] = useState<string | null | undefined>(undefined);
  const [reviewer, setReviewer] = useState("");
  const [comment, setComment] = useState("");
  const [method, setMethod] = useState(caps.merge_methods[0] ?? "");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    void getMe().then((u) => setMe(u?.email ?? null));
  }, []);

  const open = change.state === "open";
  const who = (me ?? reviewer).toLowerCase();
  const mine = change.reviews.find((r) => r.source === "forge" && r.reviewer === who && r.verdict !== "dismissed");
  const merge = actions.find((a) => a.kind === "merge");
  const merging = merge?.state === "pending";
  const gateOK = !gate || gate.state === "pass";
  // With the queue on, everything but the queue itself (and a freeze, which
  // the queue waits out) must pass before the change can join it.
  const queueReady = !!gate && gate.rules.every((r) => r.id === "merge_queue" || r.id === "freeze" || r.state === "pass");
  const entry = queue?.entry;
  const inQueue = !!entry && ["queued", "testing", "merging"].includes(entry.state);

  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      setMsg({ ok: true, text: ok });
      setComment("");
      onChanged();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };
  const review = (verdict: "approve" | "request_changes" | "dismiss", ok: string) =>
    run(() => reviewChange(change.id, verdict, comment, me ? undefined : reviewer), ok);

  return (
    <div className="card settings-form">
      <h2>
        Merge gate{" "}
        {gate ? (
          <span className={gate.state === "pass" ? "" : gate.state === "fail" ? "error-text" : "muted"}>· {gate.summary}</span>
        ) : (
          <span className="muted">· no merge rule applies to {change.target_branch}</span>
        )}
      </h2>
      {gate && (
        <ul className="gate-rules">
          {gate.rules.map((r) => (
            <li key={r.id} className={`gate-rule gate-${r.state}`}>
              <span className="gate-mark">{MARK[r.state]}</span> <b>{r.label}</b> <span className="muted">— {r.detail}</span>
            </li>
          ))}
        </ul>
      )}
      {gate && caps.status_checks && (
        <p className="muted">
          {gate.published_state === gate.state
            ? `The host shows this as forge/merge-gate (updated ${relativeTime(gate.published_at ?? gate.evaluated_at)}); require that check in branch protection so merges on the host wait for it.`
            : "Publishing this result to the host…"}
        </p>
      )}

      {open && permissions.can_review && (
        <div className="gate-actions">
          {me === null && (
            <label className="field">
              <span>Reviewing as (sign-in is off)</span>
              <input value={reviewer} onChange={(e) => setReviewer(e.target.value)} placeholder="you@company.com" />
            </label>
          )}
          <label className="field">
            <span>Comment (optional)</span>
            <input value={comment} onChange={(e) => setComment(e.target.value)} placeholder="Looks good" />
          </label>
          <div className="form-actions">
            <button className="btn btn-approve" disabled={busy} onClick={() => void review("approve", "Approved.")}>
              Approve
            </button>
            <button className="btn btn-reject" disabled={busy} onClick={() => void review("request_changes", "Changes requested.")}>
              Request changes
            </button>
            {mine && (
              <button className="btn" disabled={busy} onClick={() => void review("dismiss", "Your review was withdrawn.")}>
                Withdraw my review
              </button>
            )}
          </div>
        </div>
      )}

      {queue?.enabled && entry && (
        <p className={entry.state === "failed" ? "error-text" : "muted"}>
          {queueStatus(entry, change.target_branch)}
          {entry.pipeline_id && (
            <>
              {" "}
              · <Link to={`/pipelines/${entry.pipeline_id}`}>test pipeline #{entry.pipeline_id}</Link>
            </>
          )}
          {inQueue && queue.length > 1 && ` · ${queue.length} in line for ${change.target_branch}`}
        </p>
      )}
      {open && queue?.enabled && caps.merge_methods.length > 0 && permissions.can_merge && (
        <div className="form-actions" style={{ marginTop: 12 }}>
          {!inQueue && (
            <select value={method} onChange={(e) => setMethod(e.target.value)} disabled={busy}>
              {caps.merge_methods.map((m) => (
                <option key={m} value={m}>
                  {METHOD_LABELS[m] ?? m}
                </option>
              ))}
            </select>
          )}
          {inQueue ? (
            <button className="btn" disabled={busy} onClick={() => void run(() => dequeueChange(change.id), "Removed from the queue.")}>
              Remove from queue
            </button>
          ) : (
            <button
              className="btn btn-primary"
              disabled={busy || !queueReady || change.draft}
              title={!queueReady ? gate?.summary : change.draft ? "The change is a draft" : undefined}
              onClick={() => void run(() => queueChange(change.id, change.head_sha, method), "Added to the merge queue.")}
            >
              Add to merge queue
            </button>
          )}
        </div>
      )}
      {open && !queue?.enabled && caps.merge_methods.length > 0 && permissions.can_merge && (
        <div className="form-actions" style={{ marginTop: 12 }}>
          <select value={method} onChange={(e) => setMethod(e.target.value)} disabled={busy || merging}>
            {caps.merge_methods.map((m) => (
              <option key={m} value={m}>
                {METHOD_LABELS[m] ?? m}
              </option>
            ))}
          </select>
          <button
            className="btn btn-primary"
            disabled={busy || merging || !gateOK || change.draft}
            title={!gateOK ? gate?.summary : change.draft ? "The change is a draft" : undefined}
            onClick={() => void run(() => mergeChange(change.id, change.head_sha, method), "Merge queued.")}
          >
            {merging ? "Merging…" : "Merge"}
          </button>
        </div>
      )}
      {merge?.state === "failed" && <div className="error-banner">Merge failed: {merge.last_error}</div>}
      {merge?.state === "done" && change.state === "open" && <p className="muted">Merged on the host; waiting for it to confirm…</p>}
      {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
    </div>
  );
}
