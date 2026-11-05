import type { RunnerEvent } from "../../api";
import type { ChartAnnotation } from "../charts/TimeChart";

/** "paused by harish@acme.io" — the event in words (the runner is said by the caller). */
export function describeEvent(e: RunnerEvent): { text: string; tone: ChartAnnotation["tone"] } {
  const by = e.actor && e.actor !== "system" ? ` by ${e.actor}` : "";
  switch (e.action) {
    case "server.upgrade":
      return { text: `Forge deployed ${e.target}`, tone: "accent" };
    case "runner.pause":
      return { text: `${e.target} paused${by}`, tone: "warning" };
    case "runner.resume":
      return { text: `${e.target} resumed${by}`, tone: "neutral" };
    case "runner.drain":
      return { text: `${e.target} drained${by}`, tone: "attention" };
    case "runner.upgrade":
      return { text: `${e.target} upgrade requested${by}`, tone: "accent" };
    case "runner.remove":
      return { text: `${e.target} removed${by}`, tone: "neutral" };
    case "runner.rollout.start":
      return { text: `Rolling upgrade to ${e.target} started${by}`, tone: "accent" };
    case "runner.rollout.stop":
      return { text: `Rolling upgrade stopped${by}`, tone: "neutral" };
    case "maintenance.set":
      return { text: e.detail?.on ? `Maintenance on${by}${e.detail?.message ? `: ${String(e.detail.message)}` : ""}` : `Maintenance off${by}`, tone: "attention" };
    default:
      return { text: `${e.action} ${e.target}${by}`, tone: "neutral" };
  }
}

/** Events as chart marks; maintenance on → off becomes a span. */
export function toAnnotations(events: RunnerEvent[], now = Date.now()): ChartAnnotation[] {
  const out: ChartAnnotation[] = [];
  let maint: { at: number; label: string } | null = null;
  for (const e of events) {
    const at = Date.parse(e.at);
    const d = describeEvent(e);
    if (e.action === "maintenance.set") {
      if (e.detail?.on) maint = maint ?? { at, label: d.text };
      else if (maint) {
        out.push({ at: maint.at, until: at, label: maint.label, tone: "attention" });
        maint = null;
      }
      continue;
    }
    out.push({ at, label: d.text, tone: d.tone });
  }
  if (maint) out.push({ at: maint.at, until: now, label: maint.label, tone: "attention" });
  return out;
}
