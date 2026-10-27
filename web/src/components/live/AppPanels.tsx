import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  createLiveOp,
  listLiveOps,
  relativeTime,
  resourceKey,
  shortSha,
  type LiveAppView,
  type LiveOp,
  type LiveResource,
} from "../../api";
import Drawer from "../ui/Drawer";
import { Pill, Skeleton } from "../ui/Pill";
import { DEFAULT_DIFF, DiffToggles, ResourceDiff, type DiffOptions } from "./ManifestDiff";
import OpProgress from "./OpProgress";
import { HealthIcon, KindIcon } from "./ui";

/** Resources Git describes (in sync, drifted or missing from the cluster). */
export function desiredResources(rs: LiveResource[]): LiveResource[] {
  return rs.filter((r) => r.sync !== undefined || r.uid.startsWith("desired:"));
}

/** Top-level resources Git no longer has: what Prune removes. */
export function prunable(rs: LiveResource[]): LiveResource[] {
  return rs.filter((r) => (r.parents ?? []).length === 0 && r.sync === undefined && !r.uid.startsWith("desired:"));
}

function SyncMark({ r }: { r: LiveResource }) {
  if (r.uid.startsWith("desired:")) return <Pill tone="warning" icon="alert">Missing</Pill>;
  if (r.sync === "OutOfSync") return <Pill tone="warning" icon="alert">Out of sync</Pill>;
  return <Pill tone="success" icon="check">Synced</Pill>;
}

/** Diff: every resource that differs from Git, live against desired. */
export function DiffPanel({ view, onClose }: { view: LiveAppView; onClose: () => void }) {
  const [opts, setOpts] = useState<DiffOptions>(DEFAULT_DIFF);
  const drift = view.tree.resources.filter((r) => r.sync === "OutOfSync");
  const app = view.app;
  return (
    <Drawer wide title="Diff" subtitle={<>{app.name} · live against <span className="mono">{shortSha(app.revision)}</span> {app.revision_message}</>} onClose={onClose}>
      <DiffToggles opts={opts} onChange={setOpts} />
      {drift.length === 0 && <div className="empty">Everything matches Git at {shortSha(app.revision)}.</div>}
      {drift.map((r) => (
        <section key={r.uid} className="lv-diff-section">
          <h3 className="lv-diff-title">
            <KindIcon kind={r.kind} size={14} /> {r.kind} <strong>{r.name}</strong> <SyncMark r={r} />
          </h3>
          <ResourceDiff appId={app.id} resource={r} opts={opts} />
        </section>
      ))}
    </Drawer>
  );
}

/**
 * Sync: choose the revision, the resources and the options, see what will
 * change, confirm. Protected environments wait for their approvals; a
 * frozen one refuses.
 */
export function SyncDialog({ view, onClose }: { view: LiveAppView; onClose: () => void }) {
  const { app, target } = view;
  const git = target.capabilities.diff;
  const desired = useMemo(() => desiredResources(view.tree.resources), [view]);
  const extra = useMemo(() => prunable(view.tree.resources), [view]);
  const [revision, setRevision] = useState("");
  const [picked, setPicked] = useState<Set<string>>(() => new Set(desired.map(resourceKey)));
  const [prune, setPrune] = useState(false);
  const [dryRun, setDryRun] = useState(false);
  const [force, setForce] = useState(false);
  const [op, setOp] = useState<LiveOp | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const all = picked.size === desired.length;
  const toggle = (k: string) => setPicked((s) => { const n = new Set(s); if (n.has(k)) n.delete(k); else n.add(k); return n; });

  const start = () => {
    setBusy(true);
    setError(null);
    createLiveOp(app.id, {
      kind: "sync", revision: revision.trim() || undefined, prune, dry_run: dryRun, force,
      resources: git && !all ? [...picked, ...(prune ? extra.map(resourceKey) : [])] : undefined,
    }).then(setOp, (e) => setError(String(e?.message ?? e))).finally(() => setBusy(false));
  };

  return (
    <Drawer wide title={`Sync ${app.name}`} subtitle={<>{target.name} · {app.namespace}{app.environment && ` · ${app.environment}`}</>} onClose={onClose}>
      {op ? (
        <>
          <OpProgress op={op} />
          <div className="lv-dialog-actions">
            <button type="button" className="btn" onClick={() => setOp(null)}>Sync again</button>
            <button type="button" className="btn btn-primary" onClick={onClose}>Done</button>
          </div>
        </>
      ) : (
        <>
          {git ? (
            <>
              <label className="lv-field">
                Revision
                <input className="mono" placeholder={`latest — ${shortSha(app.revision)} ${app.revision_message}`} value={revision} onChange={(e) => setRevision(e.target.value)} />
              </label>
              <h3 className="drawer-h">
                Resources <button type="button" className="link-btn" onClick={() => setPicked(all ? new Set() : new Set(desired.map(resourceKey)))}>{all ? "none" : "all"}</button>
              </h3>
              <ul className="lv-pick">
                {desired.map((r) => (
                  <li key={r.uid}>
                    <label className="check-row">
                      <input type="checkbox" checked={picked.has(resourceKey(r))} onChange={() => toggle(resourceKey(r))} />
                      <KindIcon kind={r.kind} size={14} /> <span className="muted">{r.kind}</span> {r.name}
                    </label>
                    <SyncMark r={r} />
                  </li>
                ))}
              </ul>
              {prune && extra.length > 0 && (
                <div className="lv-prune-note">
                  Prune removes {extra.length} resource{extra.length === 1 ? "" : "s"} Git no longer has: {extra.map((r) => `${r.kind} ${r.name}`).join(", ")}
                </div>
              )}
            </>
          ) : (
            <p>
              Sets the service to the image Forge last deployed
              {app.deployed ? <> (<Link to={`/pipelines/${app.deployed.pipeline_id}`} className="mono">{shortSha(app.deployed.sha)}</Link>, {relativeTime(app.deployed.at)})</> : ""}.
            </p>
          )}
          <h3 className="drawer-h">Options</h3>
          <div className="lv-options">
            {git && (
              <label className="check-row"><input type="checkbox" checked={prune} onChange={(e) => setPrune(e.target.checked)} /> Prune — delete what Git no longer has</label>
            )}
            <label className="check-row"><input type="checkbox" checked={dryRun} onChange={(e) => setDryRun(e.target.checked)} /> Dry run — check, change nothing</label>
            {git && (
              <label className="check-row" title="Server-side apply refuses fields another manager owns (say, a kubectl scale); force takes them over">
                <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> Force — take over fields others changed
              </label>
            )}
          </div>
          <p className="muted lv-sync-how">
            {git ? "Applied server-side as field manager forge, in dependency order. " : ""}
            Protected environments wait for their approvals; frozen ones refuse.
          </p>
          {error && <div className="error-banner">{error}</div>}
          <div className="lv-dialog-actions">
            <button type="button" className="btn" onClick={onClose}>Cancel</button>
            <button type="button" className="btn btn-primary" disabled={busy || (git && picked.size === 0)} onClick={start}>
              {dryRun ? "Dry run" : "Sync"}
            </button>
          </div>
        </>
      )}
    </Drawer>
  );
}

/** History & rollback: every sync, rollback and refresh, and a way back. */
export function HistoryPanel({ view, onClose }: { view: LiveAppView; onClose: () => void }) {
  const { app, target } = view;
  const [ops, setOps] = useState<LiveOp[] | null>(null);
  const [active, setActive] = useState<LiveOp | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    listLiveOps(app.id).then(setOps, (e) => setError(String(e?.message ?? e)));
  }, [app.id, active]);
  const rollback = (revision: string) =>
    createLiveOp(app.id, { kind: "rollback", revision }).then(setActive, (e) => setError(String(e?.message ?? e)));
  // Cloud Run rolls back by traffic: any earlier revision.
  const revisions = target.capabilities.diff ? [] : view.tree.resources.filter((r) => r.kind === "Revision");

  return (
    <Drawer wide title="History & rollback" subtitle={app.name} onClose={onClose}>
      {active && <OpProgress op={active} />}
      {error && <div className="error-banner">{error}</div>}
      {revisions.length > 0 && (
        <>
          <h3 className="drawer-h">Revisions</h3>
          <ul className="lv-history">
            {revisions.map((r) => (
              <li key={r.uid}>
                {r.health && <HealthIcon health={r.health.status} />}
                <span className="mono">{r.name}</span>
                <span className="muted">{r.summary.status} · {relativeTime(r.created_at)}</span>
                {target.capabilities.rollback && !r.summary.status?.startsWith("Serving 100%") && (
                  <button type="button" className="btn btn-sm" onClick={() => void rollback(r.name)}>Send all traffic here</button>
                )}
              </li>
            ))}
          </ul>
        </>
      )}
      <h3 className="drawer-h">Operations</h3>
      {!ops && <Skeleton h={80} />}
      {ops && ops.length === 0 && <div className="empty">Nothing yet: syncs, rollbacks and refreshes show here.</div>}
      {ops && ops.length > 0 && (
        <ul className="lv-history">
          {ops.map((o) => (
            <li key={o.id}>
              <Pill tone={o.state === "succeeded" ? "success" : o.state === "failed" ? "danger" : o.state === "canceled" ? "neutral" : "accent"}>{o.kind}</Pill>
              {o.revision && <span className="mono">{shortSha(o.revision)}</span>}
              <span className="lv-history-msg">{o.result?.message || o.message || o.state}</span>
              <span className="muted">{o.requested_by || "someone"}, {relativeTime(o.created_at)}</span>
              {target.capabilities.rollback && target.capabilities.diff && o.revision && o.state === "succeeded" && o.revision !== app.revision && (
                <button type="button" className="btn btn-sm" onClick={() => void rollback(o.revision)}>Roll back to this</button>
              )}
            </li>
          ))}
        </ul>
      )}
      {app.deployed && (
        <p className="muted lv-history-forge">
          Forge deploys to {app.environment}: the latest is <Link to={`/pipelines/${app.deployed.pipeline_id}`} className="mono">{shortSha(app.deployed.sha)}</Link> by{" "}
          {app.deployed.by || "someone"}, {relativeTime(app.deployed.at)} — <Link to={`/repos/${encodeURIComponent(app.repo)}/environments`}>all deploys and promotion</Link>.
        </p>
      )}
    </Drawer>
  );
}
