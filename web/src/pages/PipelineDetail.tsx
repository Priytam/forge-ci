import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  duration,
  getPipeline,
  isTerminalStatus,
  shortSha,
  sourceLabel,
  triggerVerb,
  commitSubject,
  type Job,
  type Pipeline,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import { StatusIcon } from "../components/StageDots";
import ConfigChip from "../components/ConfigChip";
import Avatar, { displayName } from "../components/Avatar";
import ApprovalGate from "../components/ApprovalGate";
import PlayButton from "../components/PlayButton";

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

/**
 * authorDiffers reports whether the commit author is a DIFFERENT person from
 * the one who started the run, and is therefore worth naming separately.
 *
 * The comparison is loose on purpose: providers report the same human under
 * different spellings across the two fields (a GitHub username in one, a git
 * author name in the other), and showing "alice authored · alice pushed" for
 * every ordinary push would be noise that trains people to ignore the line.
 */
function authorDiffers(p: Pipeline): boolean {
  const author = (p.commit_author ?? "").trim().toLowerCase();
  const actor = (p.triggered_by ?? "").trim().toLowerCase();
  if (!author || !actor) return false;
  if (author === actor) return false;
  // Compare local parts too: "alice" vs "alice@corp.com" is one person.
  const local = (s: string) => s.split("@")[0];
  return local(author) !== local(actor);
}

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
          {/* What this run is. A bare sha says nothing; the subject line is the
              one thing that identifies a run to a human reading the page. */}
          {commitSubject(pipeline.commit_message) && (
            <div className="commit-subject" title={pipeline.commit_message}>
              {commitSubject(pipeline.commit_message)}
            </div>
          )}
          <div className="pipeline-meta">
            {/* Who started this run — the first question anyone asks of a
                pipeline, and previously not answered anywhere on the page. */}
            {/* Author and actor are shown separately ONLY when they differ —
                a merge, a bot push, or a re-run of someone else's commit. When
                they are the same person one avatar is the honest rendering. */}
            {authorDiffers(pipeline) && (
              <span className="actor">
                <Avatar identity={pipeline.commit_author!} size={22} />
                <span className="actor-name">{displayName(pipeline.commit_author!)}</span>
                <span className="actor-verb">authored</span>
              </span>
            )}
            <span className="actor">
              <Avatar identity={pipeline.triggered_by ?? ""} size={22} />
              {pipeline.triggered_by && (
                <span className="actor-name">{displayName(pipeline.triggered_by)}</span>
              )}
              <span className="actor-verb">{triggerVerb(pipeline.source)}</span>
            </span>
            <span className="mono sha">{shortSha(pipeline.sha)}</span>
            <ConfigChip version={pipeline.config_version ?? null} />
            {pipeline.config_url && (
              <a
                className="cfg-link"
                href={pipeline.config_url}
                target="_blank"
                rel="noreferrer"
                title={`View the config this run used, at ${shortSha(pipeline.sha)}`}
              >
                view config ↗
              </a>
            )}
            {sourceLabel(pipeline.source) && (
              <span className="source-chip">{sourceLabel(pipeline.source)}</span>
            )}
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
                // An allow_failure job that failed doesn't fail the pipeline —
                // show it as a warning, not a hard red. It is FINISHED though,
                // so it gets its own "warning" state rather than borrowing
                // "blocked", whose glyph means "waiting".
                const allowedFail =
                  job.allow_failure === true && job.status === "failed";
                const pillClass = allowedFail
                  ? "pill-warning"
                  : `pill-${job.status}`;
                const tooltip = [
                  job.name,
                  allowedFail ? "failed (allowed to fail)" : job.status,
                  isTerminalStatus(job.status) && dur ? dur : null,
                  job.environment,
                ]
                  .filter(Boolean)
                  .join(" · ");
                // Blocked jobs: manual gate → Play, approval gate →
                // Approve/Reject. When the payload can't distinguish, show both.
                const showPlay =
                  job.status === "blocked" && job.manual !== false;
                // The gate is shown while blocked, AND after it settles when
                // votes were cast: a rejected job goes straight to 'failed', and
                // dropping the panel there would erase who rejected the deploy
                // and why — the one record worth keeping.
                const showApproval =
                  (job.status === "blocked" && job.manual !== true) ||
                  (job.approvals?.length ?? 0) > 0;
                return (
                  <div key={job.id} className="job-node">
                    <div
                      ref={(el) => setCardRef(job.id, el)}
                      className={`job-pill ${pillClass}`}
                      title={tooltip}
                      onClick={() =>
                        navigate(`/jobs/${job.id}?pipeline=${pipeline.id}`)
                      }
                    >
                      <StatusIcon
                        status={allowedFail ? "warning" : job.status}
                      />
                      <span className="job-pill-name">{job.name}</span>
                      {job.allow_failure && (
                        <span className="allowfail-tag" title="allowed to fail">
                          AF
                        </span>
                      )}
                    </div>
                    {allowedFail && (
                      <div className="allowfail-note muted">allowed to fail</div>
                    )}
                    {(showPlay || showApproval) && (
                      <div className="card job-approval-panel glow glow-orange">
                        {showPlay && (
                          <PlayButton jobId={job.id} onDone={refresh} />
                        )}
                        {showApproval && (
                          <ApprovalGate
                            jobId={job.id}
                            approvals={job.approvals}
                            required={job.required_approvals}
                            status={job.status}
                            onDone={refresh}
                          />
                        )}
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
