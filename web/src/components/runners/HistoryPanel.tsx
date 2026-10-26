import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { getHistoryJobs, type HistoryFilter, type JobRow, type RunnerHistoryResponse } from "../../api";
import StatusBadge from "../StatusBadge";
import TimeChart, { ChartSync, type ChartAnnotation } from "../charts/TimeChart";
import { compact, duration } from "../charts/scale";
import { Skeleton } from "../ui/Pill";
import { WAIT_THRESHOLD_S } from "./HealthStrip";

/** Series colours: one hue per measure, percentiles told apart by dash, not colour. */
const C = {
  busy: "var(--series-1)",
  started: "var(--series-1)",
  failed: "var(--danger)",
  wait: "var(--series-1)",
  dur: "var(--series-2)",
  capacity: "var(--text-muted)",
  threshold: "var(--warning)",
};

function bucketTitle(at: string, seconds: number): string {
  const a = new Date(at);
  const b = new Date(a.getTime() + seconds * 1000);
  if (seconds >= 86400) return a.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" });
  const day = a.toLocaleDateString([], { weekday: "short", month: "short", day: "numeric" });
  const t = (d: Date) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return `${day}, ${t(a)} – ${t(b)}`;
}

/**
 * The history charts — utilisation against capacity, throughput, queue wait,
 * job duration — on one time axis with a shared crosshair; click a bucket to
 * list its jobs. Reusable: pass any filter's history.
 */
export default function HistoryPanel({
  history,
  filter,
  capacity,
  annotations,
  compactCharts,
}: {
  history: RunnerHistoryResponse | null;
  filter: HistoryFilter;
  /** slots available now, drawn as the reference line on utilisation */
  capacity?: number;
  annotations: ChartAnnotation[];
  /** smaller charts, one column (the drawer) */
  compactCharts?: boolean;
}) {
  const [picked, setPicked] = useState<number | null>(null);
  const [jobs, setJobs] = useState<JobRow[] | null>(null);
  const [log, setLog] = useState(false);
  useEffect(() => {
    setPicked(null);
    setJobs(null);
  }, [history]);

  const d = useMemo(() => {
    if (!history) return null;
    const b = history.buckets;
    const nd = b.map((x) => !!x.no_data);
    const gap = <T,>(i: number, v: T) => (nd[i] ? null : v);
    return {
      times: b.map((x) => Date.parse(x.at)),
      bucketMs: history.range.bucket_seconds * 1000,
      from: Date.parse(history.range.from),
      to: Date.parse(history.range.to),
      noData: nd,
      busy: b.map((x, i) => gap(i, x.busy_slots)),
      available: b.map((x) => x.capacity_available),
      ok: b.map((x, i) => gap(i, x.jobs_started - x.failed)),
      failed: b.map((x, i) => gap(i, x.failed)),
      w50: b.map((x) => x.wait_p50),
      w95: b.map((x) => x.wait_p95),
      d50: b.map((x) => x.duration_p50),
      d95: b.map((x) => x.duration_p95),
    };
  }, [history]);

  const pick = (i: number) => {
    if (!history) return;
    setPicked(i);
    setJobs(null);
    const from = history.buckets[i].at;
    const to = new Date(Date.parse(from) + history.range.bucket_seconds * 1000).toISOString();
    void getHistoryJobs(from, to, filter, 20).then(setJobs).catch(() => setJobs([]));
  };

  if (!history || !d) {
    return (
      <div className={compactCharts ? "chart-grid chart-grid-1" : "chart-grid"} aria-busy="true">
        {Array.from({ length: compactCharts ? 3 : 4 }, (_, i) => (
          <div key={i} className="tchart">
            <Skeleton w="30%" h={10} />
            <Skeleton h={compactCharts ? 110 : 150} r={6} />
          </div>
        ))}
      </div>
    );
  }
  const h = compactCharts ? 120 : 160;
  const capacityHistory = !filter.runner && d.available.some((v) => v != null);
  const common = {
    times: d.times,
    bucketMs: d.bucketMs,
    from: d.from,
    to: d.to,
    noData: d.noData,
    annotations,
    height: h,
    onPick: pick,
    picked,
    bucketTitle: (i: number) => bucketTitle(history.buckets[i].at, history.range.bucket_seconds),
  };
  const pickedBucket = picked != null ? history.buckets[picked] : null;
  const jobsLink = pickedBucket
    ? `/jobs?${new URLSearchParams({
        ...(filter.runner ? { runner: filter.runner } : {}),
        ...(filter.repo ? { repo: filter.repo } : {}),
        ...(filter.job ? { name: filter.job } : {}),
        started_from: pickedBucket.at,
        started_to: new Date(Date.parse(pickedBucket.at) + history.range.bucket_seconds * 1000).toISOString(),
      }).toString()}`
    : null;
  const bucketWord = history.range.bucket_seconds >= 86400 ? "day" : history.range.bucket_seconds >= 21600 ? "6 h" : "hour";

  return (
    <ChartSync>
      <div className={compactCharts ? "chart-grid chart-grid-1" : "chart-grid"}>
        <TimeChart
          {...common}
          title="Utilisation"
          unitLabel="busy slots"
          unit="slots"
          series={[
            { key: "busy", name: "busy slots", kind: "area", color: C.busy, values: d.busy, format: (v) => compact(v, 2) },
            // Recorded capacity (fleet-wide) where snapshots exist; else today's as a line.
            ...(capacityHistory ? [{ key: "cap", name: "available slots", kind: "step" as const, color: C.capacity, dashed: true, values: d.available, format: (v: number) => compact(v, 1) }] : []),
          ]}
          refLines={!capacityHistory && capacity != null && capacity > 0 ? [{ value: capacity, label: `capacity now (${capacity})`, color: C.capacity }] : []}
        />
        <TimeChart
          {...common}
          title="Throughput"
          unitLabel={`jobs started per ${bucketWord}`}
          unit="count"
          series={[
            { key: "ok", name: "started", kind: "bars", color: C.started, values: d.ok, format: (v) => String(v) },
            { key: "failed", name: "failed", kind: "bars", color: C.failed, values: d.failed, format: (v) => String(v) },
          ]}
        />
        {!compactCharts && (
          <TimeChart
            {...common}
            title="Queue wait"
            unitLabel="queued → started"
            unit="duration"
            log={log}
            series={[
              { key: "w50", name: "p50", kind: "step", color: C.wait, values: d.w50, format: duration },
              { key: "w95", name: "p95", kind: "step", color: C.wait, dashed: true, values: d.w95, format: duration },
            ]}
            refLines={[{ value: WAIT_THRESHOLD_S, label: `threshold ${duration(WAIT_THRESHOLD_S)}`, color: C.threshold }]}
            actions={
              <button type="button" className={log ? "chip-toggle chip-on" : "chip-toggle"} aria-pressed={log} onClick={() => setLog((l) => !l)}>
                log
              </button>
            }
          />
        )}
        <TimeChart
          {...common}
          title="Job duration"
          unitLabel="started → finished"
          unit="duration"
          series={[
            { key: "d50", name: "p50", kind: "step", color: C.dur, values: d.d50, format: duration },
            { key: "d95", name: "p95", kind: "step", color: C.dur, dashed: true, values: d.d95, format: duration },
          ]}
        />
      </div>
      {pickedBucket && (
        <div className="bucket-jobs">
          <div className="bucket-jobs-head">
            <strong>{bucketTitle(pickedBucket.at, history.range.bucket_seconds)}</strong>
            <span className="muted">
              {pickedBucket.jobs_started} started · {pickedBucket.failed} failed
            </span>
            {jobsLink && <Link to={jobsLink}>Open in the job list →</Link>}
            <button type="button" className="icon-btn" aria-label="Close" onClick={() => setPicked(null)}>
              ×
            </button>
          </div>
          {!jobs && <Skeleton h={60} />}
          {jobs && jobs.length === 0 && <div className="muted">No jobs started in this bucket.</div>}
          {jobs && jobs.length > 0 && (
            <table className="dense-table">
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
                    <td>
                      <StatusBadge status={j.status} />
                    </td>
                    <td className="muted num">{j.started_at ? new Date(j.started_at).toLocaleTimeString() : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </ChartSync>
  );
}
