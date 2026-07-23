import { useMemo } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { duration, getPipeline, isTerminalStatus, shortSha, type Job } from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import ApprovalButtons from "../components/ApprovalButtons";

interface Stage {
  name: string;
  idx: number;
  jobs: Job[];
}

function groupStages(jobs: Job[]): Stage[] {
  const byStage = new Map<string, Stage>();
  for (const job of jobs) {
    let stage = byStage.get(job.stage);
    if (!stage) {
      stage = { name: job.stage, idx: job.stage_idx, jobs: [] };
      byStage.set(job.stage, stage);
    }
    stage.jobs.push(job);
  }
  const stages = [...byStage.values()];
  stages.sort((a, b) => a.idx - b.idx);
  for (const stage of stages) {
    stage.jobs.sort((a, b) => a.id - b.id);
  }
  return stages;
}

export default function PipelineDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const { data, error, loading, refresh } = usePoll(
    () => getPipeline(id ?? ""),
    2000,
    // Keep polling on transient errors; only stop once we know the pipeline
    // reached a terminal state AND every job has too (approvals can still
    // change blocked jobs, but blocked is not terminal anyway).
    true
  );

  const stages = useMemo(() => (data ? groupStages(data.jobs) : []), [data]);

  if (error && !data) {
    return <div className="error-banner">Failed to load pipeline: {error}</div>;
  }
  if (loading && !data) {
    return <div className="muted">Loading pipeline…</div>;
  }
  if (!data) return null;

  const { pipeline } = data;

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Pipelines</Link> <span className="crumb-sep">/</span>{" "}
        <span className="mono">#{pipeline.id}</span>
      </div>

      <div className="card pipeline-head">
        <div className="pipeline-head-main">
          <h1>
            {pipeline.repo} <span className="muted">·</span>{" "}
            <span className="ref-tag">{pipeline.ref}</span>
          </h1>
          <div className="pipeline-meta">
            <span className="mono sha">{shortSha(pipeline.sha)}</span>
          </div>
        </div>
        <StatusBadge status={pipeline.status} />
      </div>

      {error && <div className="error-banner">Refresh failed: {error}</div>}

      <div className="stages">
        {stages.map((stage) => (
          <div key={stage.name} className="stage-col">
            <div className="stage-title">{stage.name}</div>
            {stage.jobs.map((job) => {
              const dur = duration(job.started_at, job.finished_at);
              return (
                <div
                  key={job.id}
                  className={`card job-card job-${job.status}`}
                  onClick={() =>
                    navigate(`/jobs/${job.id}?pipeline=${pipeline.id}`)
                  }
                >
                  <div className="job-card-top">
                    <span className="job-name">{job.name}</span>
                    <StatusBadge status={job.status} />
                  </div>
                  <div className="job-card-meta">
                    {isTerminalStatus(job.status) && dur && (
                      <span className="muted">{dur}</span>
                    )}
                    {job.environment && (
                      <span className="env-tag">{job.environment}</span>
                    )}
                  </div>
                  {job.status === "blocked" && (
                    <ApprovalButtons jobId={job.id} onDone={refresh} />
                  )}
                </div>
              );
            })}
          </div>
        ))}
      </div>
    </div>
  );
}
