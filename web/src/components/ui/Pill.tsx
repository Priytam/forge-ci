import type { ReactNode } from "react";
import Icon, { type IconName } from "../Icon";

export type Tone = "success" | "accent" | "warning" | "attention" | "danger" | "neutral";

/** A status pill: icon + colour + label. Colour only ever means status. */
export function Pill({ tone, icon, children, title }: { tone: Tone; icon?: IconName; children: ReactNode; title?: string }) {
  return (
    <span className={`pill2 pill2-${tone}`} title={title}>
      {icon && <Icon name={icon} size={12} />}
      {children}
    </span>
  );
}

/** A small outlined badge (versions, counts); amber when warn. */
export function Badge({ children, warn, title, mono }: { children: ReactNode; warn?: boolean; title?: string; mono?: boolean }) {
  return (
    <span className={`badge2${warn ? " badge2-warn" : ""}${mono ? " mono" : ""}`} title={title}>
      {children}
    </span>
  );
}

/** Tag chips that collapse to "+n" past max. */
export function TagChips({ tags, max = 2 }: { tags: string[]; max?: number }) {
  if (!tags.length) return <span className="muted">—</span>;
  const shown = tags.slice(0, max);
  const rest = tags.slice(max);
  return (
    <span className="chips">
      {shown.map((t) => (
        <span key={t} className="chip">
          {t}
        </span>
      ))}
      {rest.length > 0 && (
        <span className="chip chip-more" title={rest.join(", ")}>
          +{rest.length}
        </span>
      )}
    </span>
  );
}

/** An inline bar for a 0–1 ratio, with its label. */
export function Meter({ ratio, label, title }: { ratio: number | null; label: ReactNode; title?: string }) {
  const pct = ratio == null ? 0 : Math.round(Math.max(0, Math.min(1, ratio)) * 100);
  const tone = ratio == null ? "none" : ratio >= 0.85 ? "high" : ratio >= 0.6 ? "mid" : "low";
  return (
    <span className="meter" title={title}>
      <span className={`meter-track meter-${tone}`} role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
        <span className="meter-fill" style={{ width: `${pct}%` }} />
      </span>
      <span className="meter-label">{label}</span>
    </span>
  );
}

/** A shimmering placeholder block. */
export function Skeleton({ w, h = 12, r }: { w?: number | string; h?: number; r?: number }) {
  return <span className="skeleton" style={{ width: w ?? "100%", height: h, borderRadius: r }} aria-hidden="true" />;
}
