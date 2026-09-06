import { useEffect, useState } from "react";
import { getMe, submitApproval, type AuthUser, type JobApproval } from "../api";
import Avatar, { displayName } from "./Avatar";

interface ApprovalGateProps {
  jobId: number;
  /** append-only vote record, oldest first */
  approvals?: JobApproval[];
  /** votes the environment's rule requires */
  required?: number;
  /** the job has already finished (rejected gates land here) */
  status?: string;
  onDone?: () => void;
}

/**
 * The approval gate on a blocked job: who has voted, what they said, how many
 * votes remain, and the viewer's own action.
 *
 * It shows the vote record because a gate's state is invisible from the job's
 * status alone — "blocked" looks the same whether nobody has voted or one of
 * two approvers already has. Without it a reviewer cannot tell whether they are
 * the first vote or the one that ships the deploy, and someone who has already
 * voted is offered buttons that can only return a duplicate-vote error.
 */
export default function ApprovalGate({
  jobId,
  approvals = [],
  required = 0,
  status,
  onDone,
}: ApprovalGateProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [me, setMe] = useState<AuthUser | null>(null);

  useEffect(() => {
    void getMe().then(setMe);
  }, []);

  const rejection = approvals.find((a) => a.verdict === "rejected");
  const approved = approvals.filter((a) => a.verdict === "approved");
  const need = Math.max(required, 1);
  const remaining = Math.max(need - approved.length, 0);
  const myVote = me ? approvals.find((a) => a.approver === me.email) : undefined;

  const vote = async (verdict: "approved" | "rejected") => {
    // Signed-in sessions vote as themselves (the server enforces the session
    // identity anyway); only open mode prompts for a name.
    let approver: string;
    if (me) {
      approver = me.email;
    } else {
      const entered = window.prompt(
        `Enter your name to record "${verdict}" for job #${jobId}:`
      );
      if (!entered || !entered.trim()) return;
      approver = entered.trim();
    }
    setBusy(true);
    setError(null);
    try {
      await submitApproval(jobId, { approver, verdict });
      onDone?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  // A single rejection closes the gate for good — show who and why, nothing else.
  if (rejection) {
    return (
      <div className="approval approval-rejected" onClick={(e) => e.stopPropagation()}>
        <div className="approval-head">
          <span className="approval-note approval-note-rejected">Rejected</span>
          <span className="approval-tally-muted">gate closed</span>
        </div>
        <ApprovalRow vote={rejection} me={me} />
      </div>
    );
  }

  const settled = approved.length >= need;

  return (
    <div className="approval" onClick={(e) => e.stopPropagation()}>
      <div className="approval-head">
        <span className={settled ? "approval-note approval-note-done" : "approval-note"}>
          {settled ? "Approved" : "Waiting for approval"}
        </span>
        <span className="approval-tally">
          <span className="approval-count">
            {approved.length}
            <span className="approval-of"> of </span>
            {need}
          </span>
          <span className="approval-pips" aria-hidden="true">
            {Array.from({ length: need }, (_, i) => (
              <span
                key={i}
                className={i < approved.length ? "approval-pip approval-pip-on" : "approval-pip"}
              />
            ))}
          </span>
        </span>
      </div>

      {approvals.length > 0 && (
        <div className="approval-votes">
          {approvals.map((a) => (
            <ApprovalRow key={a.approver} vote={a} me={me} />
          ))}
        </div>
      )}

      {remaining > 0 && (
        <div className="approval-pending">
          <Avatar identity="" size={26} title="awaiting an approver" />
          <span className="approval-pending-text">
            {myVote
              ? `waiting on someone else — ${remaining} more needed`
              : `${remaining} more approval${remaining === 1 ? "" : "s"} needed`}
          </span>
        </div>
      )}

      {/* Someone who has already voted gets their state, not buttons that can
          only fail: the server records one vote per approver. */}
      {myVote ? (
        <div className="approval-voted">
          <svg
            width="12"
            height="12"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="M20 6 9 17l-5-5" />
          </svg>
          Your vote is recorded. One vote per person.
        </div>
      ) : (
        status !== "failed" && (
          <div className="approval-actions">
            <button
              type="button"
              className="btn btn-approve"
              disabled={busy}
              onClick={() => void vote("approved")}
            >
              Approve
            </button>
            <button
              type="button"
              className="btn btn-reject"
              disabled={busy}
              onClick={() => void vote("rejected")}
            >
              Reject
            </button>
          </div>
        )
      )}

      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}

/** One recorded vote: who, verdict, when, and their comment when they left one. */
function ApprovalRow({ vote, me }: { vote: JobApproval; me: AuthUser | null }) {
  const isMe = me?.email === vote.approver;
  const rejected = vote.verdict === "rejected";
  return (
    <div className="approval-vote">
      <Avatar identity={vote.approver} size={26} title={vote.approver} />
      <div className="approval-vote-body">
        <div className="approval-vote-line">
          <span className="approval-who">{isMe ? "you" : displayName(vote.approver)}</span>
          <span className={rejected ? "approval-verdict-no" : "approval-verdict-yes"}>
            {rejected ? (
              <svg
                width="11"
                height="11"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="3"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <path d="M18 6 6 18M6 6l12 12" />
              </svg>
            ) : (
              <svg
                width="11"
                height="11"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="3"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="M20 6 9 17l-5-5" />
              </svg>
            )}
            {vote.verdict}
          </span>
          <span className="approval-when">{relativeTime(vote.created_at)}</span>
        </div>
        {vote.comment && <div className="approval-comment">“{vote.comment}”</div>}
      </div>
    </div>
  );
}

/** Coarse relative time — the gate cares about "a moment ago" vs "yesterday". */
function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (secs < 45) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}
