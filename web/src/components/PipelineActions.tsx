import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { cancelPipeline, isTerminalStatus, retryPipeline } from "../api";

interface PipelineActionsProps {
  pipelineId: number;
  status: string;
  onDone?: () => void;
}

/**
 * Pipeline-level Cancel / Retry. Cancel shows while the pipeline is still
 * running (isTerminalStatus false) and stops every non-terminal job. Retry
 * shows once the pipeline is finished — success, failed or canceled — and
 * creates a NEW pipeline that replays this one's exact config_yaml (see
 * api.retryPipeline); it navigates to that new run rather than refreshing in
 * place, since this page's own id no longer changes.
 */
export default function PipelineActions({
  pipelineId,
  status,
  onDone,
}: PipelineActionsProps) {
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const cancel = async () => {
    if (!window.confirm("Cancel every non-terminal job in this pipeline?")) {
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await cancelPipeline(pipelineId);
      onDone?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const retry = async () => {
    setBusy(true);
    setError(null);
    try {
      const { pipeline } = await retryPipeline(pipelineId);
      navigate(`/pipelines/${pipeline.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };

  const terminal = isTerminalStatus(status);

  return (
    <div className="pipeline-actions">
      {!terminal && (
        <button
          type="button"
          className="btn btn-reject"
          disabled={busy}
          onClick={() => void cancel()}
          title="Cancel every non-terminal job in this pipeline"
        >
          ✕ Cancel
        </button>
      )}
      {terminal && (
        <button
          type="button"
          className="btn"
          disabled={busy}
          onClick={() => void retry()}
          title="Run this exact config again as a new pipeline"
        >
          ↻ Retry
        </button>
      )}
      {error && <div className="approval-error">{error}</div>}
    </div>
  );
}
