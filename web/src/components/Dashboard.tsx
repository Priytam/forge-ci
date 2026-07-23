import { useId, useRef, useState, type ReactNode } from "react";
import { getStats } from "../api";
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
  accent = "#8b949e",
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
      style={{ "--accent": accent } as React.CSSProperties}
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
  for (let k = 1; k < labelOrder.length; k++) {
    const prev = labelOrder[k - 1];
    const cur = labelOrder[k];
    if (endLabelYs[cur] - endLabelYs[prev] < 14) {
      endLabelYs[cur] = endLabelYs[prev] + 14;
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
                      stopColor={s.color}
                      stopOpacity={s.areaAlpha ?? 0.22}
                    />
                    <stop offset="1" stopColor={s.color} stopOpacity="0" />
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
                <stop offset="0" stopColor={s.color} stopOpacity="0.25" />
                <stop offset="1" stopColor={s.color} stopOpacity="1" />
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
              stroke="rgba(255,255,255,0.15)"
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

export default function Dashboard() {
  const { data } = usePoll(getStats, 5000);

  if (!data) return null;
  const { now } = data;

  const rate = now.success_rate_24h;
  const rateClass =
    rate < 0 ? "" : rate >= 90 ? "stat-good" : rate >= 70 ? "stat-warn" : "stat-bad";

  const hours = data.pipelines.map((b) => b.hour);

  return (
    <div className="dash">
      <div className="stat-grid">
        <StatTile
          value={now.running_jobs}
          label="Jobs running"
          dot={now.running_jobs > 0}
          accent="#58a6ff"
        />
        <StatTile value={now.pending_jobs} label="Queued" accent="#8b949e" />
        <StatTile
          value={now.blocked_jobs}
          label="Awaiting approval"
          valueClass={now.blocked_jobs > 0 ? "stat-blocked" : ""}
          accent="#d29922"
        />
        <StatTile
          value={now.online_runners}
          label="Runners online"
          accent="#3fb950"
        />
        <StatTile
          value={now.active_executors}
          label="Executors in flight"
          sublabel="forked runners/pods"
          accent="#bc8cff"
        />
        <StatTile
          value={rate < 0 ? "—" : `${rate}%`}
          label="Success rate 24h"
          valueClass={rateClass}
          accent={
            rate < 0
              ? "#8b949e"
              : rate >= 90
                ? "#3fb950"
                : rate >= 70
                  ? "#d29922"
                  : "#f85149"
          }
        />
      </div>

      <MiniChart
        title="Activity (24h)"
        hours={hours}
        legend
        endLabels
        series={[
          {
            name: "Passed",
            color: "#3fb950",
            values: data.pipelines.map((b) => b.success),
            area: true,
            areaAlpha: 0.2,
          },
          {
            name: "Failed",
            color: "#f85149",
            values: data.pipelines.map((b) => b.failed),
            area: false,
          },
          {
            name: "Jobs",
            color: "#58a6ff",
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
