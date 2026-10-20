import { useSearchParams } from "react-router-dom";

export type RangeKey = "24h" | "7d" | "30d";

const KEYS: RangeKey[] = ["24h", "7d", "30d"];
const STORAGE = "forge.range";

/** Reads the shared time range: the URL wins, then the remembered choice. */
export function useRange(): [RangeKey, (r: RangeKey) => void] {
  const [params, setParams] = useSearchParams();
  const fromUrl = params.get("range") as RangeKey | null;
  let remembered: RangeKey | null = null;
  try {
    remembered = window.localStorage.getItem(STORAGE) as RangeKey | null;
  } catch {
    // storage unavailable
  }
  const range: RangeKey = fromUrl && KEYS.includes(fromUrl) ? fromUrl : remembered && KEYS.includes(remembered) ? remembered : "24h";
  const set = (r: RangeKey) => {
    try {
      window.localStorage.setItem(STORAGE, r);
    } catch {
      // ignore
    }
    const next = new URLSearchParams(params);
    next.set("range", r);
    setParams(next, { replace: true });
  };
  return [range, set];
}

/** One control for every time-series page: 24h · 7 days · 30 days. */
export default function RangePicker({ value, onChange }: { value: RangeKey; onChange: (r: RangeKey) => void }) {
  return (
    <div className="range-picker" role="group" aria-label="Time range">
      {KEYS.map((k) => (
        <button key={k} type="button" className={k === value ? "btn btn-icon range-on" : "btn btn-icon"} onClick={() => onChange(k)}>
          {k === "24h" ? "24h" : k === "7d" ? "7 days" : "30 days"}
        </button>
      ))}
    </div>
  );
}

/** Label for a bucket's start, sized to the range. */
export function bucketLabel(iso: string, range: RangeKey): string {
  const d = new Date(iso);
  if (range === "24h") return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (range === "7d") return d.toLocaleString([], { weekday: "short", hour: "2-digit" });
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}
