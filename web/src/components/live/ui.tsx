import { Link } from "react-router-dom";
import type { Health, LiveApp, LiveSummary } from "../../api";
import type { Tone } from "../ui/Pill";

/** Health in Forge's status language: colour means status, nothing else. */
export const HEALTH_TONE: Record<Health, Tone> = {
  Healthy: "success",
  Progressing: "accent",
  Degraded: "danger",
  Suspended: "neutral",
  Missing: "warning",
  Unknown: "neutral",
};

/** Worst first — the order lists and filters use. */
export const HEALTH_ORDER: Health[] = ["Degraded", "Missing", "Progressing", "Suspended", "Unknown", "Healthy"];

const SYNC_TONE: Record<string, Tone> = { Synced: "success", OutOfSync: "warning", Unknown: "neutral" };

/** 16px stroke glyphs for health and sync (currentColor). */
const GLYPH: Record<string, string> = {
  Healthy: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM5.5 8.2l1.7 1.7 3.3-3.6",
  Degraded: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM8 4.8v3.8M8 10.9v.01",
  Suspended: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM6.5 5.8v4.4M9.5 5.8v4.4",
  Missing: "M8 2.5 14 13H2L8 2.5ZM8 6.5v3M8 11.2v.01",
  Unknown: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM6.3 6.2a1.8 1.8 0 1 1 2.4 1.7c-.5.2-.7.6-.7 1.1v.3M8 11.2v.01",
  Synced: "M3 8a5 5 0 0 1 8.6-3.5L13 6M13 3v3h-3M13 8a5 5 0 0 1-8.6 3.5L3 10M3 13v-3h3",
  OutOfSync: "M3 8a5 5 0 0 1 8.6-3.5L13 6M13 3v3h-3M13 8a5 5 0 0 1-8.6 3.5L3 10M3 13v-3h3M8 5.8v2.6M8 10.3v.01",
};

function Glyph({ d, size = 14, spin }: { d?: string; size?: number; spin?: boolean }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.5}
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className={spin ? "lv-spin" : undefined}>
      {spin ? <path d="M8 2a6 6 0 1 0 6 6" /> : <path d={d} />}
    </svg>
  );
}

/** A health icon alone, coloured (Progressing spins). */
export function HealthIcon({ health, size = 14, title }: { health: Health; size?: number; title?: string }) {
  return (
    <span className={`lv-ic lv-tone-${HEALTH_TONE[health] ?? "neutral"}`} title={title ?? health}>
      <Glyph d={GLYPH[health] ?? GLYPH.Unknown} size={size} spin={health === "Progressing"} />
    </span>
  );
}

/** Health as icon + word. */
export function HealthPill({ health, title }: { health: Health; title?: string }) {
  return (
    <span className={`pill2 pill2-${HEALTH_TONE[health] ?? "neutral"}`} title={title}>
      <Glyph d={GLYPH[health] ?? GLYPH.Unknown} size={12} spin={health === "Progressing"} />
      {health}
    </span>
  );
}

/** Sync as icon + word. Until desired state is compared (phase B) it is Unknown. */
export function SyncPill({ sync }: { sync: string }) {
  const title = sync === "Unknown" ? "Not compared: the agent has no source for this app, or it could not render it" : undefined;
  return (
    <span className={`pill2 pill2-${SYNC_TONE[sync] ?? "neutral"}`} title={title}>
      <Glyph d={GLYPH[sync] ?? GLYPH.Unknown} size={12} />
      {sync === "OutOfSync" ? "Out of sync" : sync}
    </span>
  );
}

/** A sync icon alone: green synced, amber out of sync. */
export function SyncIcon({ sync, size = 14 }: { sync: string; size?: number }) {
  return (
    <span className={`lv-ic lv-tone-${SYNC_TONE[sync] ?? "neutral"}`} title={sync === "OutOfSync" ? "Out of sync with Git" : sync}>
      <Glyph d={GLYPH[sync] ?? GLYPH.Unknown} size={size} />
    </span>
  );
}

/** Kind glyphs, Forge's own: one recognisable shape per kind. */
const KIND: Record<string, string> = {
  App: "M2.5 2.5h4.5v4.5H2.5zM9 2.5h4.5v4.5H9zM2.5 9h4.5v4.5H2.5zM9 9h4.5v4.5H9z",
  Deployment: "M3 5.5h8v8H3zM5 3.5h8v8M7 1.5h7v7",
  StatefulSet: "M3 3.5h10v3H3zM3 9.5h10v3H3zM5.5 5h.01M5.5 11h.01",
  DaemonSet: "M8 2.5 13 5.5v5L8 13.5 3 10.5v-5L8 2.5ZM8 2.5v11M3 5.5l10 5M13 5.5l-10 5",
  ReplicaSet: "M2.5 4.5h7v7h-7zM6.5 2.5h7v7",
  Pod: "M8 2 13.2 5v6L8 14 2.8 11V5L8 2ZM2.8 5 8 8l5.2-3M8 8v6",
  Service: "M8 6.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3ZM3 3.5h.01M13 3.5h.01M3 12.5h.01M13 12.5h.01M4 4.5 6.9 7M12 4.5 9.1 7M4 11.5 6.9 9M12 11.5 9.1 9",
  Ingress: "M9.5 2.5h4v11h-4M2 8h8M7 5l3 3-3 3",
  ConfigMap: "M4 2h5.5L12 4.5V14H4zM6 7h4M6 9.5h4M6 12h2",
  HorizontalPodAutoscaler: "M8 2.5v11M5.5 5 8 2.5 10.5 5M5.5 11 8 13.5 10.5 11M2.5 8h3M10.5 8h3",
  PersistentVolumeClaim: "M3 4c0-1 2.2-1.8 5-1.8s5 .8 5 1.8v8c0 1-2.2 1.8-5 1.8S3 13 3 12zM3 4c0 1 2.2 1.8 5 1.8S13 5 13 4M3 8c0 1 2.2 1.8 5 1.8S13 9 13 8",
  Job: "M8 14A6 6 0 1 0 8 2a6 6 0 0 0 0 12ZM5.5 8.2l1.7 1.7 3.3-3.6",
  Revision: "M3 3h10v10H3zM5.5 6h5M5.5 8.5h5M5.5 11h3",
};

/** Short labels for the node's kind tag. */
export const KIND_SHORT: Record<string, string> = {
  Deployment: "deploy",
  StatefulSet: "sts",
  DaemonSet: "ds",
  ReplicaSet: "rs",
  Pod: "pod",
  Service: "svc",
  Ingress: "ing",
  ConfigMap: "cm",
  HorizontalPodAutoscaler: "hpa",
  PersistentVolumeClaim: "pvc",
  Job: "job",
  Revision: "rev",
};

export function KindIcon({ kind, size = 16 }: { kind: string; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.4}
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={KIND[kind] ?? KIND.ConfigMap} />
    </svg>
  );
}

/** Where to see what runs in one repo × environment. */
export function liveLink(env: string, repo?: string): string {
  const qs = new URLSearchParams({ environment: env });
  if (repo) qs.set("repo", repo);
  return `/applications?${qs}`;
}

/** What runs in one repo × environment, in one badge: worst health and the count. */
export function LiveBadge({ summary, env, repo }: { summary: LiveSummary; env: string; repo?: string }) {
  const bad = summary.apps - (summary.counts.Healthy ?? 0);
  const title = Object.entries(summary.counts)
    .map(([h, n]) => `${n} ${h}`)
    .join(", ");
  return (
    <Link to={liveLink(env, repo)} className={`lv-badge lv-tone-${HEALTH_TONE[summary.health]}`} title={`Live: ${title}`}>
      <Glyph d={GLYPH[summary.health]} size={12} spin={summary.health === "Progressing"} />
      {summary.apps === 1 ? "1 app" : `${summary.apps} apps`}
      {bad > 0 && <span className="lv-badge-bad">{bad} {summary.health.toLowerCase()}</span>}
    </Link>
  );
}

/** The first image's name:tag, without the registry path. */
export function shortImage(img?: string): string {
  if (!img) return "";
  const at = img.indexOf("@");
  const base = at >= 0 ? img.slice(0, at + 8) : img;
  return base.slice(base.lastIndexOf("/") + 1);
}

/** "2/3 pods" or "" when the app has none. */
export function podsLabel(a: Pick<LiveApp, "pods" | "pods_ready">): string {
  return a.pods ? `${a.pods_ready}/${a.pods} pods` : "";
}
