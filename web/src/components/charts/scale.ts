/* Scales, ticks and formatters for the time-series charts. Pure functions,
   no DOM, so they are unit-tested. */

export interface Ticks {
  /** top of the y domain (bottom is 0, or 1 on a log scale) */
  max: number;
  values: number[];
}

/** 1, 2, 5 × 10^n at or above raw. */
export function niceStep(raw: number): number {
  if (!(raw > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(raw)));
  const m = raw / p;
  return (m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10) * p;
}

/** Ticks from 0 for counts and slots: about `count` steps, integers when the data is integral. */
export function linearTicks(max: number, count = 4, integers = false): Ticks {
  let step = niceStep((max > 0 ? max : 1) / count);
  if (integers) step = Math.max(1, Math.round(step));
  const top = Math.max(step, Math.ceil((max > 0 ? max : 1) / step - 1e-9) * step);
  const values: number[] = [];
  for (let v = 0; v <= top + step / 2; v += step) values.push(+v.toFixed(6));
  return { max: top, values };
}

const DURATION_STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];

/** Ticks in seconds that land on whole seconds / minutes / hours. */
export function durationTicks(max: number, count = 4): Ticks {
  const m = max > 0 ? max : 1;
  const step = DURATION_STEPS.find((s) => m / s <= count) ?? niceStep(m / count);
  const top = Math.ceil(m / step - 1e-9) * step;
  const values: number[] = [];
  for (let v = 0; v <= top; v += step) values.push(v);
  return { max: top, values };
}

const LOG_STEPS = [1, 10, 60, 600, 3600, 36000, 86400];

/** Log-scale ticks for durations (domain starts at 1 s). */
export function logDurationTicks(max: number): Ticks {
  const top = LOG_STEPS.find((s) => s >= max) ?? Math.max(max, 1);
  return { max: top, values: LOG_STEPS.filter((s) => s <= top) };
}

const HOUR = 3600_000;
const DAY = 24 * HOUR;
const TIME_STEPS = [HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR, DAY, 2 * DAY, 7 * DAY];

export interface TimeTick {
  at: number;
  label: string;
}

/**
 * Time-axis ticks on local hour / midnight boundaries, spaced at least
 * minPx apart for the given plot width. Midnights are labelled with the date.
 */
export function timeTicks(from: number, to: number, widthPx: number, minPx = 72): TimeTick[] {
  const span = to - from;
  if (!(span > 0) || widthPx <= 0) return [];
  const step = TIME_STEPS.find((s) => (span / s) * minPx <= widthPx) ?? TIME_STEPS[TIME_STEPS.length - 1];
  const out: TimeTick[] = [];
  // Walk in local time (setDate / setHours) so DST days stay aligned.
  const d = new Date(from);
  if (step >= DAY) {
    const every = step / DAY;
    d.setHours(0, 0, 0, 0);
    for (let i = 0; d.getTime() <= to; d.setDate(d.getDate() + 1)) {
      if (d.getTime() < from) continue;
      if (every === 7 ? d.getDay() === 1 : i % every === 0) out.push({ at: d.getTime(), label: tickLabel(d, step) });
      i++;
    }
  } else {
    const every = step / HOUR;
    d.setMinutes(0, 0, 0);
    for (; d.getTime() <= to; d.setHours(d.getHours() + 1)) {
      if (d.getTime() >= from && d.getHours() % every === 0) out.push({ at: d.getTime(), label: tickLabel(d, step) });
    }
  }
  return out;
}

function tickLabel(d: Date, step: number): string {
  if (step >= DAY || (d.getHours() === 0 && d.getMinutes() === 0)) {
    return d.toLocaleDateString([], { month: "short", day: "numeric" });
  }
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/** Axis label for seconds: 0, 30s, 5m, 1.5h. */
export function durationAxis(sec: number): string {
  if (sec === 0) return "0";
  if (sec < 60) return `${+sec.toFixed(1)}s`;
  if (sec < 3600) return `${+(sec / 60).toFixed(1)}m`;
  return `${+(sec / 3600).toFixed(1)}h`;
}

/** Full duration: 42s, 3m 10s, 1h 05m; "—" for none. */
export function duration(sec: number | null | undefined): string {
  if (sec == null || Number.isNaN(sec)) return "—";
  if (sec < 1) return sec <= 0 ? "0s" : "<1s";
  if (sec < 60) return `${Math.round(sec)}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${String(Math.round(sec % 60)).padStart(2, "0")}s`;
  return `${Math.floor(sec / 3600)}h ${String(Math.floor((sec % 3600) / 60)).padStart(2, "0")}m`;
}

/** Compact number: 3, 2.5, 1.2k. */
export function compact(v: number | null | undefined, digits = 1): string {
  if (v == null || Number.isNaN(v)) return "—";
  if (Math.abs(v) >= 1000) return `${+(v / 1000).toFixed(1)}k`;
  return String(+v.toFixed(digits));
}

/** Percent of a ratio: 0.423 → "42%"; null → "—". */
export function percent(ratio: number | null | undefined): string {
  if (ratio == null || !Number.isFinite(ratio)) return "—";
  return `${Math.round(ratio * 100)}%`;
}
