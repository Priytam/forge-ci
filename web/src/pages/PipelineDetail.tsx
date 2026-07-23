import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { duration, getPipeline, isTerminalStatus, shortSha, type Job } from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import { StatusIcon } from "../components/StageDots";
import ApprovalButtons from "../components/ApprovalButtons";

interface Stage {
  name: string;
  idx: number;
  jobs: Job[];
}

interface Edge {
  key: string;
  d: string;
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

/**
 * Dependency depth per job: 0 for jobs with no needs, otherwise
 * 1 + max(depth of needed jobs). Cycle-safe (treats back-edges as depth 0).
 */
function computeDepths(jobs: Job[]): Map<number, number> {
  const byId = new Map(jobs.map((j) => [j.id, j]));
  const depths = new Map<number, number>();
  const visiting = new Set<number>();

  const depthOf = (jobId: number): number => {
    const cached = depths.get(jobId);
    if (cached !== undefined) return cached;
    if (visiting.has(jobId)) return 0;
    visiting.add(jobId);
    const job = byId.get(jobId);
    let depth = 0;
    if (job && job.needs && job.needs.length > 0) {
      let maxNeed = -1;
      for (const needId of job.needs) {
        if (byId.has(needId)) {
          maxNeed = Math.max(maxNeed, depthOf(needId));
        }
      }
      if (maxNeed >= 0) depth = maxNeed + 1;
    }
    visiting.delete(jobId);
    depths.set(jobId, depth);
    return depth;
  };

  for (const job of jobs) depthOf(job.id);
  return depths;
}

/** Group jobs into columns by dependency depth. */
function groupByDepth(jobs: Job[]): Stage[] {
  const depths = computeDepths(jobs);
  const cols = new Map<number, Stage>();
  for (const job of jobs) {
    const depth = depths.get(job.id) ?? 0;
    let col = cols.get(depth);
    if (!col) {
      col = { name: `Depth ${depth}`, idx: depth, jobs: [] };
      cols.set(depth, col);
    }
    col.jobs.push(job);
  }
  const columns = [...cols.values()];
  columns.sort((a, b) => a.idx - b.idx);
  for (const col of columns) {
    col.jobs.sort((a, b) => a.id - b.id);
  }
  return columns;
}

type GroupMode = "stage" | "deps";

export default function PipelineDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const mode: GroupMode = searchParams.get("group") === "deps" ? "deps" : "stage";

  const setMode = (next: GroupMode) => {
    const params = new URLSearchParams(searchParams);
    if (next === "deps") {
      params.set("group", "deps");
    } else {
      params.delete("group");
    }
    setSearchParams(params, { replace: true });
  };

  const { data, error, loading, refresh } = usePoll(
    () => getPipeline(id ?? ""),
    2000,
    true
  );

  const columns = useMemo(() => {
    if (!data) return [];
    return mode === "deps" ? groupByDepth(data.jobs) : groupStages(data.jobs);
  }, [data, mode]);

  // --- DAG edges (job.needs) drawn as an SVG overlay over the board ---
  const boardRef = useRef<HTMLDivElement | null>(null);
  const cardRefs = useRef(new Map<number, HTMLDivElement>());
  const [edges, setEdges] = useState<Edge[]>([]);
  const [overlaySize, setOverlaySize] = useState({ w: 0, h: 0 });

  const setCardRef = useCallback((jobId: number, el: HTMLDivElement | null) => {
    if (el) {
      cardRefs.current.set(jobId, el);
    } else {
      cardRefs.current.delete(jobId);
    }
  }, []);

  const jobs = data?.jobs;

  const computeEdges = useCallback(() => {
    const board = boardRef.current;
    if (!board || !jobs || jobs.length === 0) {
      setEdges([]);
      return;
    }
    const boardRect = board.getBoundingClientRect();
    const scrollLeft = board.scrollLeft;
    const scrollTop = board.scrollTop;

    const next: Edge[] = [];
    for (const job of jobs) {
      const toEl = cardRefs.current.get(job.id);
      if (!toEl) continue;
      const toRect = toEl.getBoundingClientRect();
      for (const needId of job.needs ?? []) {
        const fromEl = cardRefs.current.get(needId);
        if (!fromEl) continue;
        const fromRect = fromEl.getBoundingClientRect();
        // start = right-center of the needed job's card
        const x1 = fromRect.right - boardRect.left + scrollLeft;
        const y1 = fromRect.top + fromRect.height / 2 - boardRect.top + scrollTop;
        // end = left-center of the dependent job's card
        const x2 = toRect.left - boardRect.left + scrollLeft;
        const y2 = toRect.top + toRect.height / 2 - boardRect.top + scrollTop;
        // cubic bezier: leaves the source pill horizontally, enters the
        // target pill horizontally (GitLab-style rounded connector).
        const bend = Math.max(24, Math.abs(x2 - x1) * 0.5);
        const d = `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}`;
        next.push({ key: `${needId}-${job.id}`, d });
      }
    }
    setEdges(next);
    setOverlaySize({ w: board.scrollWidth, h: board.scrollHeight });
  }, [jobs]);

  // Recompute after every poll render and whenever the grouping changes...
  useLayoutEffect(() => {
    computeEdges();
  }, [computeEdges, mode]);

  // ...and on window resize.
  useEffect(() => {
    const onResize = () => computeEdges();
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [computeEdges]);

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
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <Link to={`/repos/${encodeURIComponent(pipeline.repo)}`}>
          {pipeline.repo}
        </Link>{" "}
        <span className="crumb-sep">/</span>{" "}
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

      <div className="group-toggle-row">
        <span className="muted group-toggle-label">Group jobs by</span>
        <div className="segmented">
          <button
            type="button"
            className={mode === "stage" ? "seg-btn seg-active" : "seg-btn"}
            onClick={() => setMode("stage")}
          >
            Stage
          </button>
          <button
            type="button"
            className={mode === "deps" ? "seg-btn seg-active" : "seg-btn"}
            onClick={() => setMode("deps")}
          >
            Job dependencies
          </button>
        </div>
      </div>

      <div
        className={mode === "deps" ? "board board-deps" : "board"}
        ref={boardRef}
        onScroll={computeEdges}
      >
        <svg
          className="dag-overlay"
          width={overlaySize.w}
          height={overlaySize.h}
          aria-hidden="true"
        >
          {edges.map((e) => (
            <path key={e.key} d={e.d} className="dag-edge" />
          ))}
        </svg>
        <div className="stages">
          {columns.map((stage) => (
            <div key={stage.name} className="stage-col">
              <div className="stage-title">{stage.name}</div>
              {stage.jobs.map((job) => {
                const dur = duration(job.started_at, job.finished_at);
                const tooltip = [
                  job.name,
                  job.status,
                  isTerminalStatus(job.status) && dur ? dur : null,
                  job.environment,
                ]
                  .filter(Boolean)
                  .join(" · ");
                return (
                  <div key={job.id} className="job-node">
                    <div
                      ref={(el) => setCardRef(job.id, el)}
                      className="job-pill"
                      title={tooltip}
                      onClick={() =>
                        navigate(`/jobs/${job.id}?pipeline=${pipeline.id}`)
                      }
                    >
                      <StatusIcon status={job.status} />
                      <span className="job-pill-name">{job.name}</span>
                    </div>
                    {job.status === "blocked" && (
                      <div className="card job-approval-panel">
                        <ApprovalButtons jobId={job.id} onDone={refresh} />
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
