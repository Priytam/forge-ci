import { createContext, useContext, useId, useMemo, useState, type KeyboardEvent, type PointerEvent, type ReactNode } from "react";
import { useElementWidth } from "../../hooks/useElementWidth";
import { durationAxis, durationTicks, linearTicks, logDurationTicks, timeTicks, type Ticks } from "./scale";

/*
 * A small time-series chart for bucketed data: fills its container's width,
 * draws step lines / step areas / bars per bucket, leaves gaps where there is
 * no value, labels both axes with units, and shares a crosshair with every
 * chart under the same <ChartSync>.
 */

export interface ChartSeries {
  key: string;
  name: string;
  /** one value per bucket; null = nothing measured (drawn as a gap) */
  values: (number | null)[];
  kind: "step" | "area" | "bars";
  color: string;
  dashed?: boolean;
  /** tooltip value */
  format?: (v: number) => string;
}

export interface RefLine {
  value: number;
  label: string;
  color: string;
}

/** A mark on the time axis: an instant, or a span (maintenance). */
export interface ChartAnnotation {
  at: number;
  until?: number;
  label: string;
  tone: "accent" | "warning" | "attention" | "neutral";
}

export type YUnit = "count" | "slots" | "duration";

interface Props {
  title: string;
  /** shown after the title: "slots", "jobs / bucket", "seconds" */
  unitLabel?: string;
  /** bucket starts, ms */
  times: number[];
  bucketMs: number;
  /** range shown, ms (defaults to the buckets' extent) */
  from?: number;
  to?: number;
  series: ChartSeries[];
  unit: YUnit;
  /** buckets the history doesn't cover (hatched) */
  noData?: boolean[];
  refLines?: RefLine[];
  annotations?: ChartAnnotation[];
  log?: boolean;
  height?: number;
  onPick?: (i: number) => void;
  picked?: number | null;
  /** controls rendered in the header (a log-scale toggle) */
  actions?: ReactNode;
  /** first column of the tooltip, e.g. "Mon 14:00 – 15:00" */
  bucketTitle?: (i: number) => string;
}

/* ---- crosshair sync ---- */

interface Sync {
  hover: number | null;
  setHover: (i: number | null) => void;
}

const SyncContext = createContext<Sync | null>(null);

/** Charts inside share one crosshair (same buckets, same index). */
export function ChartSync({ children }: { children: ReactNode }) {
  const [hover, setHover] = useState<number | null>(null);
  return <SyncContext.Provider value={{ hover, setHover }}>{children}</SyncContext.Provider>;
}

function useSync(): Sync {
  const shared = useContext(SyncContext);
  const [hover, setHover] = useState<number | null>(null);
  return shared ?? { hover, setHover };
}

const M = { top: 10, right: 12, bottom: 22, left: 44 };

const TONE: Record<ChartAnnotation["tone"], string> = {
  accent: "var(--accent)",
  warning: "var(--warning)",
  attention: "var(--attention)",
  neutral: "var(--text-subtle)",
};

function yTicks(unit: YUnit, max: number, log: boolean): Ticks {
  if (unit === "duration") return log ? logDurationTicks(max) : durationTicks(max);
  return linearTicks(max, 4, unit === "count");
}

function yLabel(unit: YUnit, v: number): string {
  if (unit === "duration") return durationAxis(v);
  return String(+v.toFixed(2));
}

export default function TimeChart({
  title,
  unitLabel,
  times,
  bucketMs,
  from,
  to,
  series,
  unit,
  noData,
  refLines = [],
  annotations = [],
  log = false,
  height = 160,
  onPick,
  picked = null,
  actions,
  bucketTitle,
}: Props) {
  const [wrap, width] = useElementWidth<HTMLDivElement>();
  const { hover, setHover } = useSync();
  const [local, setLocal] = useState(false); // this chart has the pointer (tooltip shows here only)
  const hatch = useId().replace(/:/g, "");
  const n = times.length;
  const x0 = from ?? (n ? times[0] : 0);
  const x1 = to ?? (n ? times[n - 1] + bucketMs : 1);
  const plotW = Math.max(0, width - M.left - M.right);
  const plotH = height - M.top - M.bottom;

  const dataMax = useMemo(() => {
    let m = 0;
    for (const s of series) {
      if (s.kind === "bars" && series.filter((o) => o.kind === "bars").length > 1) continue;
      for (const v of s.values) if (v != null && v > m) m = v;
    }
    // Stacked bars: the tallest stack.
    const bars = series.filter((s) => s.kind === "bars");
    if (bars.length > 1) for (let i = 0; i < n; i++) m = Math.max(m, bars.reduce((a, s) => a + (s.values[i] ?? 0), 0));
    for (const r of refLines) m = Math.max(m, r.value);
    return m;
  }, [series, refLines, n]);
  const ticks = yTicks(unit, dataMax, log);
  const lo = log ? 1 : 0;
  const y = (v: number) => {
    if (log) {
      const c = Math.max(1, v);
      return M.top + plotH - (Math.log10(c) / Math.log10(Math.max(ticks.max, 10))) * plotH;
    }
    return M.top + plotH - ((v - lo) / (ticks.max - lo || 1)) * plotH;
  };
  const x = (t: number) => M.left + ((t - x0) / (x1 - x0 || 1)) * plotW;
  const xt = useMemo(() => timeTicks(x0, x1, plotW), [x0, x1, plotW]);

  const idxAt = (px: number) => {
    const t = x0 + ((px - M.left) / (plotW || 1)) * (x1 - x0);
    const i = Math.floor((t - times[0]) / bucketMs);
    return i >= 0 && i < n ? i : null;
  };
  const onMove = (e: PointerEvent<SVGRectElement>) => {
    const box = (e.currentTarget.ownerSVGElement ?? e.currentTarget).getBoundingClientRect();
    setLocal(true);
    setHover(idxAt(e.clientX - box.left));
  };
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!n) return;
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      setLocal(true);
      const cur = hover ?? (e.key === "ArrowRight" ? -1 : n);
      setHover(Math.max(0, Math.min(n - 1, cur + (e.key === "ArrowRight" ? 1 : -1))));
    } else if (e.key === "Enter" && hover != null && onPick) onPick(hover);
    else if (e.key === "Escape") setHover(null);
  };

  const ready = width > 0 && n > 0;
  const bars = series.filter((s) => s.kind === "bars");
  const barGap = Math.min(2, (plotW / Math.max(n, 1)) * 0.2);

  // Step path for one series: a flat segment per bucket, broken at nulls.
  const stepPath = (s: ChartSeries, area: boolean): string => {
    let d = "";
    let runStart = -1;
    const close = (end: number) => {
      if (area && runStart >= 0) d += `L${x(times[end] + bucketMs).toFixed(1)},${y(lo).toFixed(1)}L${x(times[runStart]).toFixed(1)},${y(lo).toFixed(1)}Z`;
      runStart = -1;
    };
    for (let i = 0; i < n; i++) {
      const v = s.values[i];
      if (v == null) {
        if (runStart >= 0) close(i - 1);
        continue;
      }
      const xa = x(times[i]).toFixed(1);
      const xb = x(times[i] + bucketMs).toFixed(1);
      const yv = y(v).toFixed(1);
      if (runStart < 0) {
        runStart = i;
        d += `M${xa},${yv}`;
      } else d += `L${xa},${yv}`;
      d += `L${xb},${yv}`;
    }
    if (runStart >= 0) close(n - 1);
    return d;
  };

  const tipIdx = local ? hover : null;
  const tipLeft = tipIdx != null ? x(times[tipIdx] + bucketMs / 2) : 0;
  const tipEvents = tipIdx != null ? annotations.filter((a) => a.at >= times[tipIdx] && a.at < times[tipIdx] + bucketMs) : [];

  return (
    <figure className="tchart">
      <figcaption className="tchart-head">
        <span className="tchart-title">
          {title}
          {unitLabel && <span className="tchart-unit"> · {unitLabel}</span>}
        </span>
        <span className="tchart-legend">
          {series.map((s) => (
            <span key={s.key} className="tchart-key">
              <svg width="14" height="8" aria-hidden="true">
                {s.kind === "bars" ? (
                  <rect x="3" y="0" width="8" height="8" rx="1.5" fill={s.color} />
                ) : (
                  <line x1="0" y1="4" x2="14" y2="4" stroke={s.color} strokeWidth="2" strokeDasharray={s.dashed ? "3 2" : undefined} />
                )}
              </svg>
              {s.name}
            </span>
          ))}
          {noData?.some(Boolean) && (
            <span className="tchart-key" title="The history doesn't reach these buckets (not rolled up yet, or before rollups began)">
              <svg width="14" height="8" aria-hidden="true">
                <rect x="3" y="0" width="8" height="8" fill="none" stroke="var(--text-subtle)" strokeDasharray="1.5 1.5" />
              </svg>
              no data
            </span>
          )}
          {refLines.map((r) => (
            <span key={r.label} className="tchart-key">
              <svg width="14" height="8" aria-hidden="true">
                <line x1="0" y1="4" x2="14" y2="4" stroke={r.color} strokeWidth="1.5" strokeDasharray="1 2" />
              </svg>
              {r.label}
            </span>
          ))}
          {actions}
        </span>
      </figcaption>
      <div
        ref={wrap}
        className="tchart-plot"
        style={{ height }}
        tabIndex={0}
        role="img"
        aria-label={`${title} chart. Use the left and right arrow keys to read values.`}
        onKeyDown={onKey}
        onBlur={() => setLocal(false)}
      >
        {ready && (
          <svg width={width} height={height} className="tchart-svg">
            <defs>
              <pattern id={hatch} width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
                <line x1="3" y1="0" x2="3" y2="6" stroke="var(--text-subtle)" strokeWidth="1.25" />
              </pattern>
            </defs>
            {/* gridlines + y labels */}
            {ticks.values.map((v) => (
              <g key={v}>
                <line x1={M.left} x2={M.left + plotW} y1={y(v)} y2={y(v)} className="tchart-grid" />
                <text x={M.left - 6} y={y(v)} dy="0.32em" textAnchor="end" className="tchart-axis">
                  {yLabel(unit, v)}
                </text>
              </g>
            ))}
            {/* x labels */}
            {xt.map((t) => (
              <g key={t.at}>
                <line x1={x(t.at)} x2={x(t.at)} y1={M.top + plotH} y2={M.top + plotH + 4} className="tchart-tick" />
                <text x={x(t.at)} y={height - 6} textAnchor="middle" className="tchart-axis">
                  {t.label}
                </text>
              </g>
            ))}
            {/* no-data buckets */}
            {noData?.map((nd, i) =>
              nd ? <rect key={`nd${i}`} x={x(times[i])} y={M.top} width={Math.max(1, x(times[i] + bucketMs) - x(times[i]))} height={plotH} fill={`url(#${hatch})`} opacity={0.35} /> : null,
            )}
            {/* maintenance spans */}
            {annotations
              .filter((a) => a.until != null)
              .map((a, i) => (
                <rect key={`span${i}`} x={x(Math.max(a.at, x0))} y={M.top} width={Math.max(1, x(Math.min(a.until!, x1)) - x(Math.max(a.at, x0)))} height={plotH} fill={TONE[a.tone]} opacity={0.08} />
              ))}
            {picked != null && picked < n && (
              <rect x={x(times[picked])} y={M.top} width={Math.max(1, x(times[picked] + bucketMs) - x(times[picked]))} height={plotH} className="tchart-picked" />
            )}
            {/* bars (stacked in series order) */}
            {bars.length > 0 &&
              times.map((t, i) => {
                let base = 0;
                const bx = x(t) + barGap / 2;
                const bw = Math.max(1, x(t + bucketMs) - x(t) - barGap);
                return bars.map((s) => {
                  const v = s.values[i];
                  if (v == null || v <= 0) return null;
                  const top = y(base + v);
                  const bottom = y(base);
                  base += v;
                  return <rect key={`${s.key}${i}`} x={bx} y={top} width={bw} height={Math.max(0.5, bottom - top)} fill={s.color} rx={bw > 6 ? 1.5 : 0} />;
                });
              })}
            {series
              .filter((s) => s.kind === "area")
              .map((s) => (
                <path key={`${s.key}-fill`} d={stepPath(s, true)} fill={s.color} opacity={0.14} stroke="none" />
              ))}
            {series
              .filter((s) => s.kind !== "bars")
              .map((s) => (
                <path key={s.key} d={stepPath(s, false)} fill="none" stroke={s.color} strokeWidth={1.75} strokeDasharray={s.dashed ? "5 3" : undefined} strokeLinejoin="round" />
              ))}
            {/* reference lines */}
            {refLines.map((r) => (
              <g key={r.label}>
                <line x1={M.left} x2={M.left + plotW} y1={y(r.value)} y2={y(r.value)} stroke={r.color} strokeWidth={1.25} strokeDasharray="2 3" />
              </g>
            ))}
            {/* annotation marks */}
            {annotations
              .filter((a) => a.at >= x0 && a.at <= x1)
              .map((a, i) => (
                <g key={`an${i}`} className="tchart-annot">
                  <line x1={x(a.at)} x2={x(a.at)} y1={M.top} y2={M.top + plotH} stroke={TONE[a.tone]} strokeWidth={1} strokeDasharray="1 2" opacity={0.7} />
                  <path d={`M${x(a.at)},${M.top + plotH + 1}l3.5,4.5h-7z`} fill={TONE[a.tone]}>
                    <title>{a.label}</title>
                  </path>
                </g>
              ))}
            {/* crosshair */}
            {hover != null && hover < n && (
              <g pointerEvents="none">
                <line x1={x(times[hover] + bucketMs / 2)} x2={x(times[hover] + bucketMs / 2)} y1={M.top} y2={M.top + plotH} className="tchart-cross" />
                {series
                  .filter((s) => s.kind !== "bars")
                  .map((s) =>
                    s.values[hover] == null ? null : (
                      <circle key={s.key} cx={x(times[hover] + bucketMs / 2)} cy={y(s.values[hover]!)} r={3} fill="var(--surface)" stroke={s.color} strokeWidth={1.75} />
                    ),
                  )}
              </g>
            )}
            <rect
              x={M.left}
              y={0}
              width={plotW}
              height={height}
              fill="transparent"
              style={{ cursor: onPick ? "pointer" : "crosshair" }}
              onPointerMove={onMove}
              onPointerLeave={() => {
                setLocal(false);
                setHover(null);
              }}
              onClick={() => hover != null && onPick?.(hover)}
            />
          </svg>
        )}
        {!ready && <div className="skeleton tchart-skeleton" />}
        {ready && tipIdx != null && tipIdx < n && (
          <div className={tipLeft > width * 0.6 ? "tchart-tip tchart-tip-left" : "tchart-tip"} style={{ left: tipLeft }} role="status">
            <div className="tchart-tip-title">{bucketTitle ? bucketTitle(tipIdx) : new Date(times[tipIdx]).toLocaleString()}</div>
            {noData?.[tipIdx] ? (
              <div className="tchart-tip-row muted">No data for this period</div>
            ) : (
              series.map((s) => (
                <div key={s.key} className="tchart-tip-row">
                  <span className="tchart-tip-swatch" style={{ background: s.color }} />
                  <span>{s.name}</span>
                  <span className="tchart-tip-val">{s.values[tipIdx] == null ? "—" : s.format ? s.format(s.values[tipIdx]!) : String(s.values[tipIdx])}</span>
                </div>
              ))
            )}
            {refLines.map((r) => (
              <div key={r.label} className="tchart-tip-row muted">
                <span className="tchart-tip-swatch" style={{ background: r.color }} />
                <span>{r.label}</span>
              </div>
            ))}
            {tipEvents.map((a, i) => (
              <div key={i} className="tchart-tip-event" style={{ borderColor: TONE[a.tone] }}>
                {a.label}
              </div>
            ))}
            {onPick && <div className="tchart-tip-hint">Click for the jobs</div>}
          </div>
        )}
      </div>
    </figure>
  );
}
