import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { RangeQuery } from "../api";

export type RangeKey = "24h" | "7d" | "30d";

/** A preset range, or a custom one with from/to (ISO). */
export interface TimeRange {
  key: RangeKey | "custom";
  from?: string;
  to?: string;
}

const KEYS: RangeKey[] = ["24h", "7d", "30d"];
const STORAGE = "forge.range";
const LABEL: Record<RangeKey, string> = { "24h": "24h", "7d": "7d", "30d": "30d" };

function remembered(): RangeKey | null {
  try {
    const v = window.localStorage.getItem(STORAGE) as RangeKey | null;
    return v && KEYS.includes(v) ? v : null;
  } catch {
    return null; // storage unavailable
  }
}

function remember(r: RangeKey) {
  try {
    window.localStorage.setItem(STORAGE, r);
  } catch {
    // ignore
  }
}

/** Reads the shared time range: the URL wins, then the remembered choice. */
export function useRange(): [RangeKey, (r: RangeKey) => void] {
  const [params, setParams] = useSearchParams();
  const fromUrl = params.get("range") as RangeKey | null;
  const range: RangeKey = fromUrl && KEYS.includes(fromUrl) ? fromUrl : remembered() ?? "24h";
  const set = (r: RangeKey) => {
    remember(r);
    const next = new URLSearchParams(params);
    next.set("range", r);
    setParams(next, { replace: true });
  };
  return [range, set];
}

/** useRange plus custom ranges (?range=custom&from=&to=). */
export function useTimeRange(): [TimeRange, (r: TimeRange) => void] {
  const [params, setParams] = useSearchParams();
  const key = params.get("range");
  const from = params.get("from") ?? undefined;
  const to = params.get("to") ?? undefined;
  let range: TimeRange;
  if (key === "custom" && from && to && Date.parse(from) < Date.parse(to)) range = { key: "custom", from, to };
  else range = { key: key && KEYS.includes(key as RangeKey) ? (key as RangeKey) : remembered() ?? "24h" };
  const set = (r: TimeRange) => {
    const next = new URLSearchParams(params);
    next.set("range", r.key);
    if (r.key === "custom" && r.from && r.to) {
      next.set("from", r.from);
      next.set("to", r.to);
    } else {
      remember(r.key as RangeKey);
      next.delete("from");
      next.delete("to");
    }
    setParams(next, { replace: true });
  };
  return [range, set];
}

/** The API form of a range. */
export function toRangeQuery(r: TimeRange): RangeQuery {
  return r.key === "custom" ? { range: "custom", from: r.from, to: r.to } : { range: r.key };
}

/** The window a range covers, in ms (presets end now). */
export function rangeSpan(r: TimeRange, now = Date.now()): { from: number; to: number } {
  if (r.key === "custom" && r.from && r.to) return { from: Date.parse(r.from), to: Date.parse(r.to) };
  const hours = r.key === "7d" ? 7 * 24 : r.key === "30d" ? 30 * 24 : 24;
  return { from: now - hours * 3600_000, to: now };
}

/** The same-length window just before r, as a custom range. */
export function previousRange(r: TimeRange, now = Date.now()): RangeQuery {
  const { from, to } = rangeSpan(r, now);
  return { range: "custom", from: new Date(from - (to - from)).toISOString(), to: new Date(from).toISOString() };
}

/** "24h", "7d", "Oct 3 – Oct 9" — for headings and table labels. */
export function rangeLabel(r: TimeRange): string {
  if (r.key !== "custom") return r.key === "24h" ? "24h" : r.key === "7d" ? "7 days" : "30 days";
  const f = (iso?: string) => (iso ? new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" }) : "");
  return `${f(r.from)} – ${f(r.to)}`;
}

/** datetime-local value for an ISO instant (local time). */
function toLocalInput(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * One control for every time-series page: a segmented 24h · 7d · 30d, plus
 * Custom (from/to) where the page passes onCustom.
 */
export default function RangePicker({
  value,
  onChange,
  custom,
  onCustom,
}: {
  value: RangeKey | "custom";
  onChange: (r: RangeKey) => void;
  /** the current custom range, when value is "custom" */
  custom?: { from?: string; to?: string };
  onCustom?: (from: string, to: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const pop = useRef<HTMLDivElement>(null);
  const [from, setFrom] = useState(() => toLocalInput(custom?.from ?? new Date(Date.now() - 3 * 86400_000).toISOString()));
  const [to, setTo] = useState(() => toLocalInput(custom?.to ?? new Date().toISOString()));
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (pop.current && !pop.current.contains(e.target as Node)) setOpen(false);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  const valid = from && to && new Date(from) < new Date(to);
  return (
    <div className="range-picker" ref={pop}>
      <div className="segmented" role="radiogroup" aria-label="Time range">
        {KEYS.map((k) => (
          <button
            key={k}
            type="button"
            role="radio"
            aria-checked={k === value}
            className={k === value ? "seg-btn seg-active" : "seg-btn"}
            onClick={() => onChange(k)}
          >
            {LABEL[k]}
          </button>
        ))}
        {onCustom && (
          <button
            type="button"
            role="radio"
            aria-checked={value === "custom"}
            aria-expanded={open}
            className={value === "custom" ? "seg-btn seg-active" : "seg-btn"}
            onClick={() => setOpen((o) => !o)}
          >
            {value === "custom" && custom?.from ? rangeLabel({ key: "custom", ...custom }) : "Custom"}
          </button>
        )}
      </div>
      {open && onCustom && (
        <div className="popover range-pop" role="dialog" aria-label="Custom range">
          <label className="field">
            <span>From</span>
            <input type="datetime-local" value={from} onChange={(e) => setFrom(e.target.value)} />
          </label>
          <label className="field">
            <span>To</span>
            <input type="datetime-local" value={to} onChange={(e) => setTo(e.target.value)} />
          </label>
          <button
            type="button"
            className="btn btn-primary"
            disabled={!valid}
            onClick={() => {
              onCustom(new Date(from).toISOString(), new Date(to).toISOString());
              setOpen(false);
            }}
          >
            Apply
          </button>
        </div>
      )}
    </div>
  );
}

/** Label for a bucket's start, sized to the range. */
export function bucketLabel(iso: string, range: RangeKey | "custom"): string {
  const d = new Date(iso);
  if (range === "24h") return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (range === "7d" || range === "custom") return d.toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}
