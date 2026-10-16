import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { getPermissions, retryJob } from "../api";

/** "Retry job": re-runs this failed/canceled job and the downstream jobs that
 *  didn't succeed, inside the same pipeline, then opens the new attempt. */
export default function RetryJobButton({ jobId, pipelineId, repo }: { jobId: number; pipelineId: number; repo: string }) {
  const navigate = useNavigate();
  const [canRun, setCanRun] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (repo) void getPermissions(repo).then((p) => setCanRun(p.can_run));
  }, [repo]);
  if (!canRun) return null;

  const retry = async (debug = false) => {
    setBusy(true);
    setError(null);
    try {
      const r = await retryJob(jobId, debug);
      navigate(`/jobs/${r.job_id}?pipeline=${pipelineId}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };
  return (
    <div className="pipeline-actions">
      <button
        type="button"
        className="btn"
        disabled={busy}
        onClick={() => void retry()}
        title="Run this job again in the same pipeline, with the jobs after it that didn't succeed"
      >
        ↻ Retry job
      </button>
      <button
        type="button"
        className="btn"
        disabled={busy}
        onClick={() => void retry(true)}
        title="Retry with CI_DEBUG_TRACE=true: the log shows every command as it runs. Masked variables stay masked (including the shell's re-quoted forms); values your script derives from them, such as a base64-encoded token, can show."
      >
        Retry with debug trace
      </button>
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
