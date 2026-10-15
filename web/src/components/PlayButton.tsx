import { useEffect, useState } from "react";
import { getPermissions, playJob } from "../api";

interface PlayButtonProps {
  jobId: number;
  repo: string;
  onDone?: () => void;
}

/** Release a manual-gated blocked job (blocked → created). The button shows
 *  only to viewers with a role on the repo; others see who can run it. */
export default function PlayButton({ jobId, repo, onDone }: PlayButtonProps) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [canRun, setCanRun] = useState<boolean | null>(null);
  useEffect(() => {
    void getPermissions(repo).then((p) => setCanRun(p.can_run));
  }, [repo]);

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
      {canRun && (
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
      )}
      {canRun === false && (
        <div className="muted">Needs a role on {repo} to run.</div>
      )}
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
