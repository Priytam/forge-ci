import { useCallback, useId, useRef, useState, type ReactNode } from "react";
import { getStats } from "../api";
import RangePicker, { useRange } from "./RangePicker";
import { usePoll } from "../hooks/usePoll";

function hourLabel(hour: string): string {
  const d = new Date(hour);
  if (!Number.isNaN(d.getTime())) {
    return `${String(d.getHours()).padStart(2, "0")}:00`;
  }
  return hour;
}

/* ---------------- Stat tiles ---------------- */

function StatTile({
  value,
  label,
  sublabel,
  valueClass,
  dot,
  accent = "var(--text-muted)",
}: {
  value: ReactNode;
  label: string;
  sublabel?: string;
  valueClass?: string;
  dot?: boolean;
  /** hue for the smoky glow + value text-shadow */
  accent?: string;
}) {
  return (
    <div
      className="stat-tile"
      style={{ "--tile-accent": accent } as React.CSSProperties}
    >
      <div className={`stat-value ${valueClass ?? ""}`}>
        {value}
        {dot && <span className="stat-pulse-dot" />}
      </div>
      <div className="stat-label">{label}</div>
      {sublabel && <div className="stat-sublabel">{sublabel}</div>}
    </div>
  );
}

/* ---------------- Hand-rolled SVG chart ---------------- */

interface Series {
  name: string;
  color: string;
  values: number[];
  /** gradient-fade area under the line */
  area: boolean;
  /** peak alpha of the area fade (default 0.22) */
  areaAlpha?: number;
  /** stroke width in px (default 2) */
  width?: number;
}

const PLOT_TOP = 6;
const PLOT_BOTTOM = 96;

interface Pt {
  x: number;
  y: number;
}

const clampY = (y: number) => Math.min(PLOT_BOTTOM, Math.max(2, y));

/**
 * Catmull-Rom → cubic bezier: a smooth, flowing curve through every point.
 * Control-point Y is clamped so spikes never dip below the baseline.
 */
function smoothPath(pts: Pt[]): string {
  if (pts.length === 0) return "";
  if (pts.length === 1) return `M ${pts[0].x} ${pts[0].y}`;
  let d = `M ${pts[0].x.toFixed(2)} ${pts[0].y.toFixed(2)}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] ?? pts[i];
    const p1 = pts[i];
    const p2 = pts[i + 1];
    const p3 = pts[i + 2] ?? p2;
    const c1x = p1.x + (p2.x - p0.x) / 6;
    const c1y = clampY(p1.y + (p2.y - p0.y) / 6);
    const c2x = p2.x - (p3.x - p1.x) / 6;
    const c2y = clampY(p2.y - (p3.y - p1.y) / 6);
    d += ` C ${c1x.toFixed(2)} ${c1y.toFixed(2)}, ${c2x.toFixed(2)} ${c2y.toFixed(2)}, ${p2.x.toFixed(2)} ${p2.y.toFixed(2)}`;
  }
  return d;
}

function MiniChart({
  title,
  hours,
  series,
  legend = false,
  endLabels = false,
}: {
  title: string;
  hours: string[];
  series: Series[];
  legend?: boolean;
  endLabels?: boolean;
}) {
  const uid = useId().replace(/[^a-zA-Z0-9]/g, "");
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const [hover, setHover] = useState<number | null>(null);

  const n = hours.length;
  if (n === 0) return null;

  const maxV = Math.max(1, ...series.flatMap((s) => s.values));

  const xOf = (i: number) => (n > 1 ? (i / (n - 1)) * 100 : 0);
  const yOf = (v: number) =>
    PLOT_TOP + (1 - v / maxV) * (PLOT_BOTTOM - PLOT_TOP);

  const ptsOf = (vals: number[]): Pt[] =>
    vals.map((v, i) => ({ x: xOf(i), y: yOf(v) }));
  const linePath = (vals: number[]) => smoothPath(ptsOf(vals));
  const areaPath = (vals: number[]) =>
    `${linePath(vals)} L 100 ${PLOT_BOTTOM} L 0 ${PLOT_BOTTOM} Z`;

  const onMove = (e: React.MouseEvent<HTMLDivElement>) => {
    const el = bodyRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const frac = (e.clientX - rect.left) / Math.max(1, rect.width);
    const idx = Math.min(n - 1, Math.max(0, Math.round(frac * (n - 1))));
    setHover(idx);
  };

  const hourTicks = n >= 17 ? [0, 8, 16] : n >= 2 ? [0, n - 1] : [0];

  // End labels: push apart (in y-order) when the lines converge so the text
  // never overlaps, then shift the whole stack up if it spills past the plot.
  const endLabelYs = series.map((s) => yOf(s.values[n - 1]));
  const labelOrder = endLabelYs
    .map((_, i) => i)
    .sort((a, b) => endLabelYs[a] - endLabelYs[b]);
  // The plot is stretched to its box, so a readable gap (~14px) is a different
  // number of viewBox units on a short chart than on a tall one.
  const plotPx = bodyRef.current?.clientHeight ?? 140;
  const labelGap = Math.max(10, (14 / plotPx) * 100);
  for (let k = 1; k < labelOrder.length; k++) {
    const prev = labelOrder[k - 1];
    const cur = labelOrder[k];
    if (endLabelYs[cur] - endLabelYs[prev] < labelGap) {
      endLabelYs[cur] = endLabelYs[prev] + labelGap;
    }
  }
  const labelOverflow = Math.max(0, Math.max(...endLabelYs) - 98);
  if (labelOverflow > 0) {
    for (let i = 0; i < endLabelYs.length; i++) {
      endLabelYs[i] = Math.max(6, endLabelYs[i] - labelOverflow);
    }
  }
  const tooltipLeft = hover !== null ? xOf(hover) : 0;
  const tooltipFlip = tooltipLeft > 60;

  return (
    <div className="chart-panel">
      <div className="chart-head">
        <span className="chart-title">{title}</span>
        <span className="chart-max">max {maxV}</span>
      </div>
      <div
        className="chart-body"
        ref={bodyRef}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        <svg
          viewBox="0 0 100 100"
          preserveAspectRatio="none"
          className="chart-svg"
          aria-hidden="true"
        >
          <defs>
            {series.map(
              (s, si) =>
                s.area && (
                  <linearGradient
                    key={`ag-${si}`}
                    id={`grad-${uid}-${si}`}
                    gradientUnits="userSpaceOnUse"
                    x1="0"
                    y1={PLOT_TOP}
                    x2="0"
                    y2={PLOT_BOTTOM}
                  >
                    <stop
                      offset="0"
                      style={{ stopColor: s.color }}
                      stopOpacity={s.areaAlpha ?? 0.22}
                    />
                    <stop offset="1" style={{ stopColor: s.color }} stopOpacity="0" />
                  </linearGradient>
                )
            )}
            {series.map((s, si) => (
              <linearGradient
                key={`sg-${si}`}
                id={`stroke-${uid}-${si}`}
                gradientUnits="userSpaceOnUse"
                x1="0"
                y1="0"
                x2="100"
                y2="0"
              >
                <stop offset="0" style={{ stopColor: s.color }} stopOpacity="0.25" />
                <stop offset="1" style={{ stopColor: s.color }} stopOpacity="1" />
              </linearGradient>
            ))}
          </defs>
          {series.map(
            (s, si) =>
              s.area && (
                <path
                  key={`a-${si}`}
                  d={areaPath(s.values)}
                  fill={`url(#grad-${uid}-${si})`}
                  stroke="none"
                />
              )
          )}
          {series.map((s, si) => (
            <path
              key={`l-${si}`}
              d={linePath(s.values)}
              fill="none"
              stroke={`url(#stroke-${uid}-${si})`}
              strokeWidth={s.width ?? 2}
              strokeLinecap="round"
              strokeLinejoin="round"
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {hover !== null && (
            <line
              x1={xOf(hover)}
              x2={xOf(hover)}
              y1={0}
              y2={100}
              style={{ stroke: "var(--border-strong)" }}
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
          )}
        </svg>

        {endLabels &&
          series.map((s, si) => (
            <span
              key={si}
              className="chart-endlabel"
              style={{ top: `${endLabelYs[si]}%` }}
            >
              {s.name}
            </span>
          ))}

        {hover !== null && (
          <div
            className="chart-tooltip"
            style={
              tooltipFlip
                ? { right: `${100 - tooltipLeft}%`, marginRight: 8 }
                : { left: `${tooltipLeft}%`, marginLeft: 8 }
            }
          >
            <div className="chart-tooltip-hour">{hourLabel(hours[hover])}</div>
            {series.map((s, si) => (
              <div key={si} className="chart-tooltip-row">
                <span
                  className="legend-dot"
                  style={{ background: s.color }}
                />
                {s.name}: {s.values[hover]}
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="chart-hours">
        {hourTicks.map((i) => (
          <span key={i} style={{ left: `${xOf(i)}%` }}>
            {hourLabel(hours[i])}
          </span>
        ))}
      </div>

      {legend && (
        <div className="chart-legend">
          {series.map((s, si) => (
            <span key={si}>
              <span className="legend-dot" style={{ background: s.color }} />
              {s.name}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

/* ---------------- Dashboard ---------------- */

/** "▲4 vs last week" for a current/previous pair; null when nothing to compare. */
function delta(cur: number, prev: number | undefined, unit = ""): { text: string; up: boolean } | null {
  if (prev == null || prev < 0 || cur < 0) return null;
  const d = cur - prev;
  if (d === 0) return { text: "same as before", up: true };
  return { text: `${d > 0 ? "▲" : "▼"}${Math.abs(d)}${unit} vs previous`, up: d > 0 };
}

export default function Dashboard() {
  const [range, setRange] = useRange();
  const fetcher = useCallback(() => getStats(range), [range]);
  const { data } = usePoll(fetcher, 5000);

  if (!data) return null;
  const { now } = data;

  const rangeLabel = range === "24h" ? "24h" : range === "7d" ? "7 days" : "30 days";
  const rate = data.totals?.success_rate ?? now.success_rate_24h;
  const finished = data.totals?.finished ?? now.finished_24h ?? 0;
  const rateDelta = delta(rate, data.previous?.success_rate, "pt");
  const runsDelta = delta(data.totals?.pipelines ?? 0, data.previous?.pipelines);
  const rateClass =
    rate < 0 ? "" : rate >= 90 ? "stat-good" : rate >= 70 ? "stat-warn" : "stat-bad";

  const hours = data.pipelines.map((b) => b.hour);

  return (
    <div className="dash">
      <div className="page-head dash-head">
        <h1>Overview</h1>
        <div className="page-head-actions">
          {runsDelta && (
            <span className="muted">
              {data.totals.pipelines} pipeline{data.totals.pipelines === 1 ? "" : "s"} in the last {rangeLabel} · {runsDelta.text}
            </span>
          )}
          <RangePicker value={range} onChange={setRange} />
        </div>
      </div>
      <div className="stat-grid">
        <StatTile
          value={now.running_jobs}
          label="Jobs running"
          dot={now.running_jobs > 0}
          accent="var(--accent)"
        />
        <StatTile value={now.pending_jobs} label="Queued" accent="var(--text-muted)" />
        <StatTile
          value={now.blocked_jobs}
          label="Awaiting approval"
          valueClass={now.blocked_jobs > 0 ? "stat-blocked" : ""}
          accent="var(--warning)"
        />
        <StatTile
          value={now.online_runners}
          label="Runners online"
          accent="var(--success)"
        />
        <StatTile
          value={now.active_executors}
          label="Executors in flight"
          sublabel="forked runners/pods"
          accent="var(--neutral)"
        />
        <StatTile
          value={
            rate < 0 ? (
              "—"
            ) : (
              <>
                {rate}%
                <span className="stat-denominator">
                  of {finished} run{finished === 1 ? "" : "s"}
                </span>
              </>
            )
          }
          label={`Success rate ${rangeLabel}`}
          /* The sample size sits beside the percentage: without it, 0% of one
             run is indistinguishable from 0% of two hundred. */
          sublabel={
            rate < 0
              ? "no runs finished yet"
              : rateDelta
                ? rateDelta.text
                : now.blocked_jobs > 0
                  ? `${now.blocked_jobs} still awaiting approval`
                  : undefined
          }
          valueClass={rateClass}
          accent={
            rate < 0
              ? "var(--text-muted)"
              : rate >= 90
                ? "var(--success)"
                : rate >= 70
                  ? "var(--warning)"
                  : "var(--danger)"
          }
        />
      </div>

      <MiniChart
        title={`Activity (${rangeLabel})`}
        hours={hours}
        legend
        endLabels
        series={[
          {
            name: "Passed",
            color: "var(--success)",
            values: data.pipelines.map((b) => b.success),
            area: true,
            areaAlpha: 0.2,
          },
          {
            name: "Failed",
            color: "var(--danger)",
            values: data.pipelines.map((b) => b.failed),
            area: false,
          },
          {
            name: "Jobs",
            color: "var(--accent)",
            values: data.jobs.map((b) => b.count),
            area: true,
            areaAlpha: 0.08,
            width: 1.5,
          },
        ]}
      />
    </div>
  );
}
