import { useEffect, useState } from "react";
import { createLiveOp, streamOpOutput, type LiveOp, type LiveResource } from "../../api";
import { HealthIcon, KindIcon } from "./ui";

/** The rollouts in progress: each canary Deployment and its stable one. */
export function canaries(rs: LiveResource[]): { canary: LiveResource; stable?: LiveResource }[] {
  const top = rs.filter((r) => !(r.parents ?? []).length);
  return top
    .filter((r) => r.track === "canary")
    .map((canary) => ({
      canary,
      stable: top.find((r) => r.kind === canary.kind && r.namespace === canary.namespace && `${r.name}-canary` === canary.name),
    }));
}

function ready(r?: LiveResource): number {
  return Number((r?.summary.ready ?? "0/0").split("/")[0]) || 0;
}

/** The share of the app's ready pods the canary has: what traffic it gets. */
export function canaryWeight(canary: LiveResource, stable?: LiveResource): number {
  const c = ready(canary);
  const total = c + ready(stable);
  return total ? Math.round((100 * c) / total) : 0;
}

/**
 * A canary rollout as it happens: the share of pods (and so of traffic) on
 * the canary, what the analysis says, and the controls — Resume a manual
 * pause, Promote to 100% now, or Abort (the canary goes, the stable version
 * stays). Every control is an operation, in the history and the audit log.
 */
export default function RolloutPanel({ appId, resources, op, canOperate }: {
  appId: number;
  resources: LiveResource[];
  op: LiveOp | null;
  canOperate: boolean;
}) {
  const running = op && op.kind === "sync" && op.state === "running" ? op : null;
  const [lines, setLines] = useState<string[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setLines([]);
    if (!running) return;
    let buf = "";
    return streamOpOutput(running.id, (t) => {
      buf = (buf + t).slice(-20_000);
      setLines(buf.trimEnd().split("\n"));
    }, () => undefined);
  }, [running?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  const list = canaries(resources);
  if (list.length === 0) return null;
  const paused = lines.length > 0 && /paused until someone promotes/.test(lines[lines.length - 1]);
  const analysis = [...lines].reverse().find((l) => l.startsWith("Analysis"));
  const step = [...lines].reverse().find((l) => l.startsWith("Step"));
  const act = (kind: "promote" | "abort", full = false) => {
    const label = kind === "abort" ? "abort" : full ? "promote" : "resume";
    if (kind === "abort" && !window.confirm("Abort the rollout? The canary is removed; the stable version keeps serving.")) return;
    setBusy(label);
    setError(null);
    createLiveOp(appId, { kind, full })
      .catch((e) => setError(String(e?.message ?? e)))
      .finally(() => setBusy(null));
  };

  return (
    <section className="card lv-rollout">
      {list.map(({ canary, stable }) => {
        const w = canaryWeight(canary, stable);
        return (
          <div key={canary.uid} className="lv-rollout-row">
            <div className="lv-rollout-head">
              <h2>Canary rollout</h2>
              <span className="lv-rollout-name"><KindIcon kind={canary.kind} size={14} /> {stable?.name ?? canary.name}</span>
              <span className="muted mono">{canary.summary.images?.[0]?.split("/").pop()}</span>
              {canOperate && (
                <span className="lv-rollout-actions">
                  {paused && <button type="button" className="btn btn-sm" disabled={!!busy} onClick={() => act("promote")}>{busy === "resume" ? "Resuming…" : "Resume"}</button>}
                  <button type="button" className="btn btn-sm btn-primary" disabled={!!busy} onClick={() => act("promote", true)}>{busy === "promote" ? "Promoting…" : "Promote to 100%"}</button>
                  <button type="button" className="btn btn-sm btn-danger" disabled={!!busy} onClick={() => act("abort")}>{busy === "abort" ? "Aborting…" : "Abort"}</button>
                </span>
              )}
            </div>
            <div className="lv-rollout-bar" role="img" aria-label={`${w}% of pods on the canary`}>
              <span className="lv-rollout-canary" style={{ width: `${w}%` }} />
            </div>
            <div className="lv-rollout-legend">
              <span><span className="lv-dot lv-dot-canary" /> canary {canary.summary.ready ?? "0/0"} · {w}%</span>
              <span><span className="lv-dot lv-dot-stable" /> stable {stable?.summary.ready ?? "–"} · {100 - w}%</span>
              {canary.health && <span><HealthIcon health={canary.health.status} size={12} /> {canary.health.status}</span>}
            </div>
            {(step || analysis) && (
              <div className="lv-rollout-status">
                {step && <div>{step}</div>}
                {analysis && <div className={/failed/.test(analysis) ? "lv-bad" : "muted"}>{analysis}</div>}
              </div>
            )}
          </div>
        );
      })}
      {error && <div className="error-banner">{error}</div>}
    </section>
  );
}
