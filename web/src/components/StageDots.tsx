import { Fragment } from "react";
import type { StageStatus } from "../api";

function symbolFor(status: string): string {
  switch (status) {
    case "success":
      return "✓";
    case "failed":
      return "✕";
    case "blocked":
      return "⏸";
    case "canceled":
      return "–";
    default:
      return "";
  }
}

const KNOWN = new Set([
  "success",
  "failed",
  "running",
  "blocked",
  "pending",
  "created",
  "canceled",
]);

/**
 * Small status circle (GitLab-style): colored ring with a glyph.
 * Used inside job pills and reusable anywhere a compact icon is needed.
 */
export function StatusIcon({ status }: { status: string }) {
  const cls = KNOWN.has(status) ? status : "created";
  const spin = status === "running" ? " dot-spin" : "";
  return <span className={`stage-dot dot-${cls}${spin}`}>{symbolFor(status)}</span>;
}

/**
 * GitLab-style mini stage indicators: one small circle per stage,
 * connected by short line segments. Hover shows "<stage>: <status>".
 */
export default function StageDots({ stages }: { stages: StageStatus[] }) {
  if (!stages || stages.length === 0) return null;
  return (
    <span className="stage-dots">
      {stages.map((s, i) => {
        const cls = KNOWN.has(s.status) ? s.status : "created";
        return (
          <Fragment key={`${s.name}-${i}`}>
            {i > 0 && <span className="stage-dot-link" />}
            <span
              className={`stage-dot dot-${cls}`}
              title={`${s.name}: ${s.status}`}
            >
              {symbolFor(s.status)}
            </span>
          </Fragment>
        );
      })}
    </span>
  );
}
