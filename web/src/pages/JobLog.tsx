import { useEffect, useMemo, useRef } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import {
  duration,
  getJobLogs,
  getPipeline,
  isTerminalStatus,
  type Job,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import ApprovalButtons from "../components/ApprovalButtons";

export default function JobLog() {
  const { id } = useParams<{ id: string }>();
  const [searchParams] = useSearchParams();
  const pipelineId = searchParams.get("pipeline");

  const jobId = id ?? "";

  // Job metadata comes from the pipeline detail endpoint.
  const {
    data: detail,
    error: pipelineError,
    refresh: refreshPipeline,
  } = usePoll(
    () => getPipeline(pipelineId ?? ""),
    2000,
    Boolean(pipelineId)
  );

  const job: Job | null = useMemo(() => {
    if (!detail) return null;
    return detail.jobs.find((j) => String(j.id) === jobId) ?? null;
  }, [detail, jobId]);

  const jobStatus = job?.status ?? null;
  // Poll logs while the job could still produce output; stop on terminal state.
  const active = jobStatus === null || !isTerminalStatus(jobStatus);

  const { data: logs, error: logsError } = usePoll(
    () => getJobLogs(jobId),
    2000,
    active
  );

  // Auto-scroll to the bottom while the job is running.
  const termRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    if (jobStatus === "running" && termRef.current) {
      termRef.current.scrollTop = termRef.current.scrollHeight;
    }
  }, [logs, jobStatus]);

  const dur = job ? duration(job.started_at, job.finished_at) : null;

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Pipelines</Link> <span className="crumb-sep">/</span>{" "}
        {pipelineId ? (
          <Link to={`/pipelines/${pipelineId}`} className="mono">
            #{pipelineId}
          </Link>
        ) : (
          <span className="muted">unknown pipeline</span>
        )}{" "}
        <span className="crumb-sep">/</span>{" "}
        <span className="mono">job {jobId}</span>
      </div>

      {pipelineError && (
        <div className="error-banner">Failed to load job metadata: {pipelineError}</div>
      )}
      {!pipelineId && (
        <div className="error-banner">
          Missing ?pipeline= query parameter — job metadata unavailable.
        </div>
      )}

      {job && (
        <div className="card pipeline-head">
          <div className="pipeline-head-main">
            <h1>{job.name}</h1>
            <div className="pipeline-meta">
              <span className="stage-tag">stage: {job.stage}</span>
              {dur && <span className="muted">{dur}</span>}
              {job.environment && <span className="env-tag">{job.environment}</span>}
              {job.exit_code !== null && (
                <span className="mono muted">exit {job.exit_code}</span>
              )}
            </div>
          </div>
          <StatusBadge status={job.status} />
        </div>
      )}

      {job && job.status === "blocked" && (
        <div className="card blocked-card">
          <ApprovalButtons jobId={job.id} onDone={refreshPipeline} />
        </div>
      )}

      {logsError && <div className="error-banner">Failed to load logs: {logsError}</div>}

      <pre ref={termRef} className="terminal">
        {logs ?? "Waiting for logs…"}
      </pre>
    </div>
  );
}
