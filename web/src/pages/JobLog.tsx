import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import {
  artifactDownloadUrl,
  duration,
  expiryLabel,
  getJobLogs,
  getJobLogsIncremental,
  getJobReport,
  getPipeline,
  humanSize,
  isTerminalStatus,
  jobLogStreamUrl,
  listArtifacts,
  testDuration,
  type Artifact,
  type Job,
  type JUnitReport,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import ApprovalGate from "../components/ApprovalGate";
import PlayButton from "../components/PlayButton";
import CancelJobButton from "../components/CancelJobButton";

/**
 * Live job log: an SSE EventSource on /logs/stream?offset=0 that appends
 * `log` chunks and settles on `eof`. If the stream can't be established
 * (buffering proxies) it falls back to incremental polling from the byte
 * cursor; a finished job that can't stream just renders its full log. On
 * completion a final plain GET guarantees the complete log is shown.
 */
function useJobLog(
  jobId: string,
  terminalRef: React.MutableRefObject<boolean>
) {
  const [text, setText] = useState("");
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!jobId) return;

    setText("");
    setStreaming(false);
    setError(null);

    let cancelled = false;
    let receivedAny = false;
    let done = false;
    let offset = 0;
    let es: EventSource | null = null;
    let pollTimer: number | null = null;

    const append = (bytes: string, nextOffset: unknown) => {
      if (bytes) setText((prev) => prev + bytes);
      if (typeof nextOffset === "number") offset = nextOffset;
    };

    const finalize = async () => {
      if (done) return;
      done = true;
      setStreaming(false);
      es?.close();
      if (pollTimer !== null) window.clearInterval(pollTimer);
      // Guarantee the complete log with one authoritative plain GET.
      try {
        const full = await getJobLogs(jobId);
        if (!cancelled) setText(full);
      } catch {
        /* keep whatever we streamed */
      }
    };

    const startPolling = () => {
      if (done || cancelled || pollTimer !== null) return;
      const tick = async () => {
        if (cancelled || done) return;
        try {
          const chunk = await getJobLogsIncremental(jobId, offset);
          if (cancelled) return;
          append(chunk.bytes, chunk.next_offset);
          setError(null);
          if (chunk.eof) await finalize();
        } catch (err) {
          if (!cancelled) {
            setError(err instanceof Error ? err.message : String(err));
          }
        }
      };
      void tick();
      pollTimer = window.setInterval(() => void tick(), 2000);
    };

    try {
      es = new EventSource(jobLogStreamUrl(jobId, 0));
    } catch {
      // EventSource unavailable — go straight to a full GET / polling.
      if (terminalRef.current) void finalize();
      else startPolling();
      return () => {
        cancelled = true;
      };
    }

    setStreaming(true);

    es.addEventListener("log", (ev) => {
      receivedAny = true;
      setError(null);
      try {
        const d = JSON.parse((ev as MessageEvent).data);
        append(d.bytes ?? "", d.next_offset);
      } catch {
        /* ignore malformed frame */
      }
    });

    es.addEventListener("eof", (ev) => {
      receivedAny = true;
      try {
        const d = JSON.parse((ev as MessageEvent).data);
        if (typeof d.next_offset === "number") offset = d.next_offset;
      } catch {
        /* ignore */
      }
      void finalize();
    });

    es.onerror = () => {
      if (done || cancelled) return;
      // Never rely on EventSource auto-reconnect: our URL is fixed at
      // offset=0, so a reconnect would resend the whole log. Close and
      // recover deterministically from the byte cursor instead.
      es?.close();
      setStreaming(false);
      if (terminalRef.current) {
        // Error after terminal (or a finished job that couldn't stream):
        // one final GET shows the complete log.
        void finalize();
      } else if (!receivedAny) {
        // Never streamed a byte and still running (buffering proxy): poll.
        startPolling();
      } else {
        // Streamed some, then dropped mid-run: resume from the cursor.
        startPolling();
      }
    };

    return () => {
      cancelled = true;
      es?.close();
      if (pollTimer !== null) window.clearInterval(pollTimer);
    };
  }, [jobId, terminalRef]);

  return { text, streaming, error };
}

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
  } = usePoll(() => getPipeline(pipelineId ?? ""), 2000, Boolean(pipelineId));

  const job: Job | null = useMemo(() => {
    if (!detail) return null;
    return detail.jobs.find((j) => String(j.id) === jobId) ?? null;
  }, [detail, jobId]);

  const jobStatus = job?.status ?? null;

  // Latest terminal state, readable inside the stream effect without
  // re-subscribing the EventSource on every status poll.
  const terminalRef = useRef(false);
  terminalRef.current = jobStatus !== null && isTerminalStatus(jobStatus);

  const { text: logs, streaming, error: logsError } = useJobLog(
    jobId,
    terminalRef
  );

  // Artifacts uploaded by this job (fetched once metadata is known and
  // refreshed when the job reaches a terminal state).
  const repo = detail?.pipeline.repo;
  const artifactsFetcher = useCallback(
    () => (repo ? listArtifacts(repo, jobId) : Promise.resolve<Artifact[]>([])),
    [repo, jobId]
  );
  const { data: artifacts, refresh: refreshArtifacts } = usePoll(
    artifactsFetcher,
    0,
    false
  );
  useEffect(() => {
    if (repo) refreshArtifacts();
  }, [repo, jobStatus, refreshArtifacts]);

  // JUnit test report (null until known / when the job has none). Fetched once
  // metadata is available and re-fetched when the job reaches a terminal state.
  const reportFetcher = useCallback(
    () => (jobId ? getJobReport(jobId) : Promise.resolve<JUnitReport | null>(null)),
    [jobId]
  );
  const { data: report, refresh: refreshReport } = usePoll(
    reportFetcher,
    0,
    false
  );
  useEffect(() => {
    if (jobId) refreshReport();
  }, [jobId, jobStatus, refreshReport]);

  // Auto-scroll: stick to the bottom unless the user scrolled up.
  const termRef = useRef<HTMLPreElement>(null);
  const stickRef = useRef(true);
  const onTermScroll = () => {
    const el = termRef.current;
    if (!el) return;
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };
  useEffect(() => {
    const el = termRef.current;
    if (el && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [logs]);

  const dur = job ? duration(job.started_at, job.finished_at) : null;

  const [showFailures, setShowFailures] = useState(false);

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
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
          <div className="pipeline-head-side">
            <StatusBadge status={job.status} />
            {!isTerminalStatus(job.status) && (
              <CancelJobButton jobId={job.id} onDone={refreshPipeline} />
            )}
          </div>
        </div>
      )}

      {job && (job.status === "blocked" || (job.approvals?.length ?? 0) > 0) && (
        <div className="card blocked-card glow glow-orange">
          {job.manual ? (
            <PlayButton jobId={job.id} onDone={refreshPipeline} />
          ) : (
            <ApprovalGate
              jobId={job.id}
              approvals={job.approvals}
              required={job.required_approvals}
              status={job.status}
              onDone={refreshPipeline}
            />
          )}
        </div>
      )}

      {report && report.total > 0 && (
        <div className="card report-box">
          <div className="report-summary">
            <span className="report-label">Tests</span>
            <span className="report-stat report-pass">{report.passed} passed</span>
            <span
              className={
                report.failed > 0
                  ? "report-stat report-fail"
                  : "report-stat report-muted"
              }
            >
              {report.failed} failed
            </span>
            <span className="report-stat report-muted">
              {report.skipped} skipped
            </span>
            {report.duration_seconds > 0 && (
              <span className="muted report-dur">
                {testDuration(report.duration_seconds)}
              </span>
            )}
            {report.failed > 0 && report.failures.length > 0 && (
              <button
                type="button"
                className="report-toggle"
                onClick={() => setShowFailures((v) => !v)}
              >
                {showFailures ? "Hide failures" : "Show failures"}
              </button>
            )}
          </div>
          {showFailures && report.failures.length > 0 && (
            <ul className="report-failures">
              {report.failures.map((f, i) => (
                <li key={`${f.classname ?? ""}.${f.name}.${i}`} className="report-failure">
                  <div className="report-failure-name mono">
                    {f.classname ? `${f.classname} · ` : ""}
                    {f.name}
                    <span className="report-failure-type">{f.type}</span>
                  </div>
                  {f.message && (
                    <div className="report-failure-msg muted">{f.message}</div>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {artifacts && artifacts.length > 0 && (
        <div className="card artifacts-box">
          <div className="artifacts-title">Artifacts</div>
          {artifacts.map((a) => (
            <div key={a.id} className="artifact-row">
              <span className="mono">{a.name}</span>
              <span className="muted">{humanSize(a.size_bytes)}</span>
              <span
                className={
                  a.expires_at ? "artifact-expiry" : "artifact-expiry artifact-expiry-none"
                }
                title={a.expires_at ? new Date(a.expires_at).toLocaleString() : undefined}
              >
                {expiryLabel(a.expires_at)}
              </span>
              <a className="btn" href={artifactDownloadUrl(a.id)}>
                Download
              </a>
            </div>
          ))}
        </div>
      )}

      {logsError && <div className="error-banner">Log stream error: {logsError}</div>}

      <div className="log-head">
        <span className="artifacts-title">Log</span>
        {streaming && <span className="log-live">● live</span>}
      </div>
      <pre ref={termRef} className="terminal" onScroll={onTermScroll}>
        {logs || "Waiting for logs…"}
      </pre>
    </div>
  );
}
