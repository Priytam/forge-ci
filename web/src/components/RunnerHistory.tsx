import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getHistoryJobs, getRunnerHistory, type HistoryBucket, type JobRow } from "../api";
import RangePicker, { bucketLabel, useRange } from "./RangePicker";

/** Seconds → "42s", "3m 10s", "1h 05m". */
function dur(sec: number): string {
  if (!sec || sec < 0) return "0s";
  if (sec < 60) return `${Math.round(sec)}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${String(Math.round(sec % 60)).padStart(2, "0")}s`;
  return `${Math.floor(sec / 3600)}h ${String(Math.floor((sec % 3600) / 60)).padStart(2, "0")}m`;
}

interface Series {
  name: string;
  color: string;
  values: number[];
  format: (v: number) => string;
}

/** One chart: lines over the buckets, hover shows the values, click selects the bucket. */
function Chart({ title, buckets, series, range, onPick, picked }: {
  title: string;
  buckets: HistoryBucket[];
  series: Series[];
  range: "24h" | "7d" | "30d";
  onPick: (i: number) => void;
  picked: number | null;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const W = 640;
  const H = 120;
  const max = Math.max(1, ...series.flatMap((s) => s.values));
  const x = (i: number) => (buckets.length <= 1 ? W / 2 : (i / (buckets.length - 1)) * W);
  const y = (v: number) => H - (v / max) * (H - 8) - 4;
  const idx = hover ?? picked;
  return (
    <div className="hist-chart">
      <div className="hist-head">
        <span className="hist-title">{title}</span>
        <span className="hist-legend">
          {series.map((s) => (
            <span key={s.name} className="hist-legend-item">
              <span className="hist-swatch" style={{ background: s.color }} /> {s.name}
              {idx != null && <span className="mono"> {s.format(s.values[idx] ?? 0)}</span>}
            </span>
          ))}
          {idx != null && buckets[idx] && <span className="muted"> · {bucketLabel(buckets[idx].at, range)}</span>}
        </span>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="hist-svg" role="img" aria-label={title}
        onMouseLeave={() => setHover(null)}>
        {buckets.map((_, i) => (
          <rect key={i} x={x(i) - W / (2 * Math.max(buckets.length - 1, 1))} y={0} width={W / Math.max(buckets.length - 1, 1)} height={H}
            fill={i === idx ? "rgba(127,127,127,.15)" : "transparent"} onMouseEnter={() => setHover(i)} onClick={() => onPick(i)} style={{ cursor: "pointer" }} />
        ))}
        {series.map((s) => (
          <path key={s.name} d={s.values.map((v, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ")}
            fill="none" stroke={s.color} strokeWidth={1.6} vectorEffect="non-scaling-stroke" pointerEvents="none" />
        ))}
      </svg>
    </div>
  );
}

/**
 * Load, queue latency and job duration over time — for one runner or all —
 * with hover values, click-to-list the jobs behind a point, and a link to
 * the filtered job list.
 */
export default function RunnerHistory({ runners }: { runners: string[] }) {
  const [range, setRange] = useRange();
  const [runner, setRunner] = useState("");
  const [repo, setRepo] = useState("");
  const [job, setJob] = useState("");
  const [data, setData] = useState<HistoryBucket[] | null>(null);
  const [bucketSeconds, setBucketSeconds] = useState(3600);
  const [picked, setPicked] = useState<number | null>(null);
  const [jobs, setJobs] = useState<JobRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setPicked(null);
    setJobs(null);
    void getRunnerHistory(range, { runner, repo, job })
      .then((r) => {
        if (cancelled) return;
        setData(r.buckets);
        setBucketSeconds(r.range.bucket_seconds);
        setError(null);
      })
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [range, runner, repo, job]);

  const pick = (i: number) => {
    if (!data) return;
    setPicked(i);
    const from = data[i].at;
    const to = new Date(new Date(from).getTime() + bucketSeconds * 1000).toISOString();
    void getHistoryJobs(from, to, { runner, repo, job }).then(setJobs).catch(() => setJobs([]));
  };
  const pickedBucket = picked != null && data ? data[picked] : null;
  const jobsLink = pickedBucket
    ? `/jobs?${new URLSearchParams({
        ...(runner ? { runner } : {}),
        ...(repo ? { repo } : {}),
        ...(job ? { name: job } : {}),
        started_from: pickedBucket.at,
        started_to: new Date(new Date(pickedBucket.at).getTime() + bucketSeconds * 1000).toISOString(),
      }).toString()}`
    : null;

  return (
    <div className="card settings-form" style={{ marginBottom: 16 }}>
      <div className="sso-card-head" style={{ flexWrap: "wrap", gap: 8 }}>
        <h2>History</h2>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <select value={runner} onChange={(e) => setRunner(e.target.value)} aria-label="Runner">
            <option value="">All runners</option>
            {runners.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
          <input placeholder="repo (optional)" value={repo} onChange={(e) => setRepo(e.target.value)} style={{ width: 160 }} className="mono" />
          <input placeholder="job name (optional)" value={job} onChange={(e) => setJob(e.target.value)} style={{ width: 160 }} className="mono" />
          <RangePicker value={range} onChange={setRange} />
        </div>
      </div>
      <p className="muted">
        Queue wait is the time from queued to started — the "do we need more runners" signal. Duration is start to
        finish. Hover a point for its values; click it to list the jobs in that bucket.
      </p>
      {error && <div className="error-banner">{error}</div>}
      {data && (
        <>
          <Chart title="Load" range={range} buckets={data} picked={picked} onPick={pick} series={[
            { name: "busy slots", color: "var(--accent)", values: data.map((b) => b.busy_slots), format: (v) => v.toFixed(2) },
            { name: "jobs started", color: "var(--text-muted)", values: data.map((b) => b.jobs_started), format: (v) => String(v) },
          ]} />
          <Chart title="Queue wait" range={range} buckets={data} picked={picked} onPick={pick} series={[
            { name: "p50", color: "var(--success)", values: data.map((b) => b.wait_p50), format: dur },
            { name: "p95", color: "var(--warning)", values: data.map((b) => b.wait_p95), format: dur },
          ]} />
          <Chart title="Job duration" range={range} buckets={data} picked={picked} onPick={pick} series={[
            { name: "p50", color: "var(--success)", values: data.map((b) => b.duration_p50), format: dur },
            { name: "p95", color: "var(--danger)", values: data.map((b) => b.duration_p95), format: dur },
          ]} />
        </>
      )}
      {pickedBucket && (
        <div style={{ marginTop: 8 }}>
          <div className="muted">
            {bucketLabel(pickedBucket.at, range)}: {pickedBucket.jobs_started} job(s) started, {pickedBucket.failed} failed.{" "}
            {jobsLink && <Link to={jobsLink}>Open in the job list →</Link>}
          </div>
          {jobs && jobs.length > 0 && (
            <table className="dense-table" style={{ marginTop: 6 }}>
              <thead>
                <tr>
                  <th>Job</th>
                  <th>Repo</th>
                  <th>Runner</th>
                  <th>Status</th>
                  <th>Started</th>
                </tr>
              </thead>
              <tbody>
                {jobs.map((j) => (
                  <tr key={j.id}>
                    <td>
                      <Link to={`/jobs/${j.id}?pipeline=${j.pipeline_id}`}>{j.name}</Link>
                    </td>
                    <td className="mono">{j.repo}</td>
                    <td className="mono">{j.runner_id ?? "—"}</td>
                    <td>{j.status}</td>
                    <td className="muted">{j.started_at ? new Date(j.started_at).toLocaleTimeString() : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}
