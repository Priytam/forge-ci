import type { ReactNode } from "react";
import type { HistoryBucket, HistorySummary, RunnerLoad } from "../../api";
import Sparkline from "../charts/Sparkline";
import { duration, percent } from "../charts/scale";
import { Skeleton } from "../ui/Pill";
import { STATE_LABEL, type Capacity, type RunnerState } from "./model";

/** Queue wait above this is "stuck" (red) — also the threshold line on the charts. */
export const WAIT_THRESHOLD_S = 120;

type Tone = "ok" | "warn" | "bad" | "none";

function Tile({ label, tone, value, sub, spark, delta }: { label: string; tone: Tone; value: ReactNode; sub?: ReactNode; spark?: ReactNode; delta?: ReactNode }) {
  return (
    <section className={`kpi kpi-${tone}`} aria-label={label}>
      <div className="kpi-head">
        <span className="kpi-label">{label}</span>
        {delta}
      </div>
      <div className="kpi-value num">{value}</div>
      {sub && <div className="kpi-sub">{sub}</div>}
      <div className="kpi-spark">{spark}</div>
    </section>
  );
}

/** "+12% vs previous" — tone by whether up is good, bad, or neither. */
function Delta({
  now,
  prev,
  upIsBad,
  upIsGood,
  fmt,
  epsilon = 1e-9,
}: {
  now: number | null;
  prev: number | null;
  upIsBad?: boolean;
  upIsGood?: boolean;
  fmt?: (d: number) => string;
  /** changes smaller than this read "no change" (what the format would round to 0) */
  epsilon?: number;
}) {
  if (now == null || prev == null) return <span className="kpi-delta muted" title="No data for the previous period">—</span>;
  const d = now - prev;
  if (Math.abs(d) < epsilon) return <span className="kpi-delta muted">no change</span>;
  const bad = (upIsBad && d > 0) || (upIsGood && d < 0);
  const good = (upIsBad && d < 0) || (upIsGood && d > 0);
  const text = fmt ? fmt(d) : prev === 0 ? "new" : `${d > 0 ? "+" : ""}${Math.round((d / prev) * 100)}%`;
  return (
    <span className={`kpi-delta ${bad ? "delta-bad" : good ? "delta-good" : ""}`} title="Change vs the previous period of the same length">
      {d > 0 ? "▲" : "▼"} {text}
    </span>
  );
}

export interface HealthData {
  cap: Capacity;
  counts: Record<RunnerState, number>;
  load: RunnerLoad | null;
  buckets: HistoryBucket[] | null;
  summary: HistorySummary | null;
  prev: HistorySummary | null;
  utilisation: number | null;
  prevUtilisation: number | null;
  /** no runner filter: fleet-wide capacity/queue history applies */
  fleet: boolean;
}

/** "+1.5 slots" */
const signed = (unit: string) => (d: number) => `${d > 0 ? "+" : ""}${+d.toFixed(1)} ${unit}`;

export default function HealthStrip({ data, onState }: { data: HealthData | null; onState: (s: RunnerState | "all") => void }) {
  if (!data) {
    return (
      <div className="health-strip" aria-busy="true">
        {Array.from({ length: 5 }, (_, i) => (
          <section key={i} className="kpi">
            <Skeleton w="40%" h={10} />
            <Skeleton w="60%" h={22} />
            <Skeleton h={10} />
          </section>
        ))}
      </div>
    );
  }
  const { cap, counts, load, buckets, summary, prev } = data;
  const reduced = cap.paused + cap.draining;
  const capTone: Tone = cap.total === 0 ? "none" : cap.starved.length > 0 ? "bad" : reduced > 0 || cap.offline > 0 ? "warn" : "ok";
  const oldest = Math.max(0, ...(load?.queues ?? []).map((q) => q.oldest_wait_seconds));
  const blocked = (load?.queues ?? []).filter((q) => q.eligible_online === 0);
  const queueTone: Tone = !load ? "none" : blocked.length > 0 || oldest > WAIT_THRESHOLD_S ? "bad" : (load.pending ?? 0) > 0 ? "warn" : "ok";
  const p95 = summary?.wait_p95 ?? null;
  const waitTone: Tone = p95 == null ? "none" : p95 > WAIT_THRESHOLD_S ? "bad" : p95 > WAIT_THRESHOLD_S / 2 ? "warn" : "ok";
  // Capacity and queue history come from per-minute snapshots (fleet-wide);
  // before any exist, capacity falls back to busy share and the queue has none.
  const snapshots = data.fleet && !!buckets?.some((b) => b.capacity_available != null);
  const capSeries = snapshots
    ? buckets!.map((b) => b.capacity_available)
    : (buckets?.map((b) => (b.no_data ? null : cap.total > 0 ? b.busy_slots / cap.total : b.busy_slots)) ?? []);
  const capMax = snapshots ? Math.max(cap.total, ...buckets!.map((b) => b.capacity_total ?? 0)) : 1;

  return (
    <div className="health-strip">
      <Tile
        label="Capacity"
        tone={capTone}
        value={
          <>
            {cap.available}
            <span className="kpi-of"> / {cap.total}</span>
            <span className="kpi-unit"> slots available</span>
          </>
        }
        sub={
          cap.starved.length > 0 ? (
            <span className="kpi-alert">No slots for {cap.starved.map((t) => `“${t}”`).join(", ")}</span>
          ) : reduced > 0 || cap.offline > 0 ? (
            <span>
              {[cap.paused && `${cap.paused} paused`, cap.draining && `${cap.draining} draining`, cap.offline && `${cap.offline} offline`].filter(Boolean).join(" · ")} · {cap.busy} busy
            </span>
          ) : (
            <span>{cap.busy} busy now</span>
          )
        }
        delta={
          snapshots ? (
            <span title="Average available slots vs the previous period">
              <Delta now={summary?.capacity_available ?? null} prev={prev?.capacity_available ?? null} upIsGood fmt={signed("slots")} epsilon={0.05} />
            </span>
          ) : undefined
        }
        spark={
          buckets && (
            <Sparkline
              values={capSeries}
              max={capMax}
              label={snapshots ? "Available slots over the range" : "Busy share of capacity over the range"}
            />
          )
        }
      />
      <Tile
        label="Queue now"
        tone={queueTone}
        value={
          <>
            {load?.pending ?? "—"}
            <span className="kpi-unit"> waiting</span>
          </>
        }
        sub={
          blocked.length > 0 ? (
            <span className="kpi-alert">No runner for {blocked.map((q) => (q.tags.length ? q.tags.join("+") : "untagged")).join(", ")}</span>
          ) : (load?.pending ?? 0) > 0 ? (
            <span>
              oldest <span className="num">{duration(oldest)}</span> · {load?.running ?? 0} running
            </span>
          ) : (
            <span>
              {load?.running ?? 0} running · nothing stuck
              {snapshots && summary?.queue_pending_max != null && <> · peak {summary.queue_pending_max}</>}
            </span>
          )
        }
        delta={
          snapshots ? (
            <span title="Average jobs waiting vs the previous period">
              <Delta now={summary?.queue_pending_avg ?? null} prev={prev?.queue_pending_avg ?? null} upIsBad fmt={signed("avg")} epsilon={0.05} />
            </span>
          ) : undefined
        }
        spark={snapshots && <Sparkline values={buckets!.map((b) => b.queue_pending_max)} color="var(--attention)" label="Most jobs waiting per bucket over the range" />}
      />
      <Tile
        label="Queue wait p95"
        tone={waitTone}
        value={duration(p95)}
        delta={<Delta now={p95} prev={prev?.wait_p95 ?? null} upIsBad />}
        sub={
          <span>
            p50 <span className="num">{duration(summary?.wait_p50)}</span> · threshold {duration(WAIT_THRESHOLD_S)}
          </span>
        }
        spark={buckets && <Sparkline values={buckets.map((b) => b.wait_p95)} threshold={WAIT_THRESHOLD_S} label="Queue wait p95 over the range" />}
      />
      <Tile
        label="Utilisation"
        tone={data.utilisation == null ? "none" : data.utilisation >= 0.85 ? "warn" : "ok"}
        value={percent(data.utilisation)}
        delta={<Delta now={data.utilisation} prev={data.prevUtilisation} fmt={(d) => `${d > 0 ? "+" : ""}${Math.round(d * 100)} pts`} />}
        sub={<span title="Busy slot-hours over the slot-hours runners were available">of slot-hours · {summary?.jobs_started ?? 0} jobs</span>}
        spark={buckets && <Sparkline values={buckets.map((b) => (b.no_data ? null : b.busy_slots))} label="Busy slots over the range" />}
      />
      <Tile
        label="Runners"
        tone={counts.offline + counts.paused + counts.draining > 0 ? "warn" : "ok"}
        value={
          <>
            {counts.online + counts.busy}
            <span className="kpi-unit"> online</span>
          </>
        }
        sub={
          <span className="kpi-links">
            {(["busy", "paused", "draining", "offline"] as RunnerState[])
              .filter((s) => counts[s] > 0)
              .map((s) => (
                <button key={s} type="button" className={`kpi-link state-${s}`} onClick={() => onState(s)}>
                  {counts[s]} {STATE_LABEL[s].toLowerCase()}
                </button>
              ))}
            {counts.paused + counts.draining + counts.offline + counts.busy === 0 && <span>all idle and healthy</span>}
          </span>
        }
      />
    </div>
  );
}
