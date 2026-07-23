import { useEffect, useState } from "react";
import { getMe, submitApproval, type AuthUser } from "../api";

interface ApprovalButtonsProps {
  jobId: number;
  onDone?: () => void;
}

/**
 * Inline approve / reject buttons for a blocked job. Prompts for the
 * approver's name via window.prompt and POSTs the verdict. API errors
 * (duplicate vote, job not blocked, ...) are shown inline.
 */
export default function ApprovalButtons({ jobId, onDone }: ApprovalButtonsProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [me, setMe] = useState<AuthUser | null>(null);

  useEffect(() => {
    void getMe().then(setMe);
  }, []);

  const vote = async (verdict: "approved" | "rejected") => {
    // Signed-in sessions vote as themselves (the server enforces the
    // session identity anyway); only open mode prompts for a name.
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

  return (
    <div className="approval" onClick={(e) => e.stopPropagation()}>
      <div className="approval-note">Approval required</div>
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
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
