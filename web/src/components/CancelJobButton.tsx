import { useState } from "react";
import { cancelJob } from "../api";

interface CancelJobButtonProps {
  jobId: number;
  onDone?: () => void;
}

/** Cancel a single job. Idempotent for terminal jobs (backend no-ops); a
 * running job is signaled to stop via its next heartbeat, so the status may
 * take a moment to settle to 'canceled' after this returns. */
export default function CancelJobButton({ jobId, onDone }: CancelJobButtonProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const cancel = async () => {
    setBusy(true);
    setError(null);
    try {
      await cancelJob(jobId);
      onDone?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="pipeline-actions">
      <button
        type="button"
        className="btn btn-reject"
        disabled={busy}
        onClick={() => void cancel()}
        title="Cancel this job"
      >
        ✕ Cancel
      </button>
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
