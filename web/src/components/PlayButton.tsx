import { useState } from "react";
import { playJob } from "../api";

interface PlayButtonProps {
  jobId: number;
  onDone?: () => void;
}

/** Release a manual-gated blocked job (blocked → created). */
export default function PlayButton({ jobId, onDone }: PlayButtonProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const play = async () => {
    setBusy(true);
    setError(null);
    try {
      await playJob(jobId);
      onDone?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="approval" onClick={(e) => e.stopPropagation()}>
      <div className="approval-note">Manual job</div>
      <div className="approval-actions">
        <button
          type="button"
          className="btn btn-approve"
          disabled={busy}
          onClick={() => void play()}
        >
          ▶ Play
        </button>
      </div>
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
