import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { commitSubject, createLiveOp, getLiveApp, listLiveOps, relativeTime, shortSha, subscribeLive, type Health, type LiveAppView, type LiveOp, type LivePatch } from "../api";
import { DiffPanel, HistoryPanel, SyncDialog } from "../components/live/AppPanels";
import { networkTree, PodsView } from "../components/live/views";
import Avatar, { displayName } from "../components/Avatar";
import ResourceList from "../components/live/ResourceList";
import ResourcePanel from "../components/live/ResourcePanel";
import ResourceTree from "../components/live/ResourceTree";
import { HEALTH_ORDER, HEALTH_TONE, HealthIcon, KindIcon, shortImage, SyncPill } from "../components/live/ui";
import { Skeleton } from "../components/ui/Pill";

/** Applies a stream patch; null when the page's copy is not its base (refetch). */
function applyPatch(v: LiveAppView, p: LivePatch): LiveAppView | null {
  if (p.base !== v.tree.version) return null;
  const byUid = new Map(v.tree.resources.map((r) => [r.uid, r]));
  for (const uid of p.deletes) byUid.delete(uid);
  for (const r of p.upserts) byUid.set(r.uid, r);
  return { ...v, app: p.row, tree: { version: p.version, resources: [...byUid.values()] } };
}

/**
 * One application, live: status panels (health, sync, last deploy), then its
 * resources as a tree or a list; click one for its summary, events and live
 * manifest. Changes arrive as patches on the stream — no polling, no full
 * reloads — and everything is read from Forge, never from the cluster.
 */
export default function ApplicationDetail() {
  const id = Number(useParams<{ id: string }>().id);
  const [params, setParams] = useSearchParams();
  const asked = params.get("view");
  const mode = asked === "list" || asked === "pods" || asked === "network" ? asked : "tree";
  const panel = params.get("panel");
  const selected = params.get("node");
  const [view, setView] = useState<LiveAppView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [gone, setGone] = useState(false);
  const [q, setQ] = useState("");
  const [health, setHealth] = useState<Health[]>([]);
  const [lastOp, setLastOp] = useState<LiveOp | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const loadOps = useCallback(() => {
    listLiveOps(id).then((ops) => setLastOp(ops.find((o) => o.kind === "sync" || o.kind === "rollback") ?? null), () => undefined);
  }, [id]);
  useEffect(loadOps, [loadOps]);

  const load = useCallback(() => {
    getLiveApp(id).then(
      (v) => {
        setView(v);
        setError(null);
      },
      (e) => setError(String(e?.message ?? e))
    );
  }, [id]);
  useEffect(load, [load]);
  useEffect(
    () =>
      subscribeLive(id, {
        ready: load,
        patch: (p) => {
          if (p.app !== id) return;
          setView((v) => {
            const next = v && applyPatch(v, p);
            if (!next) load();
            return next ?? v;
          });
        },
        gone: (g) => g === id && setGone(true),
        op: (o) => (o.kind === "sync" || o.kind === "rollback") && setLastOp(o),
      }),
    [id, load]
  );

  const set = (k: string, v: string | null) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  const byUid = useMemo(() => new Map((view?.tree.resources ?? []).map((r) => [r.uid, r])), [view]);
  const present = useMemo(() => {
    const s = new Set<Health>();
    for (const r of view?.tree.resources ?? []) if (r.health) s.add(r.health.status);
    return HEALTH_ORDER.filter((h) => s.has(h));
  }, [view]);

  if (error && !view) return <div className="error-banner">Failed to load the application: {error}</div>;
  if (!view) return <Skeleton h={240} />;
  const { app, target } = view;
  const caps = target.capabilities;
  const node = selected ? byUid.get(selected) : undefined;
  const d = app.deployed;
  const refresh = () => {
    setRefreshing(true);
    createLiveOp(app.id, { kind: "refresh" })
      .catch(() => undefined)
      .finally(() => {
        setRefreshing(false);
        load();
      });
  };
  const shown = mode === "network" ? networkTree(view.tree.resources) : view.tree.resources;

  return (
    <div className="lv-page">
      <div className="page-head lv-head">
        <div className="lv-head-main">
          <h1 className="lv-title">
            <KindIcon kind="App" size={20} /> {app.name}
          </h1>
          <div className="lv-meta">
            <span title={`${target.provider} target`}>{target.name}</span>
            {app.namespace && <span>{app.namespace}</span>}
            {app.environment && <Link to={`/applications?environment=${encodeURIComponent(app.environment)}`}>{app.environment}</Link>}
            {app.repo && <Link to={`/repos/${encodeURIComponent(app.repo)}`}>{app.repo}</Link>}
            {app.images[0] && <span className="mono" title={app.images.join("\n")}>{shortImage(app.images[0])}{app.images.length > 1 ? ` +${app.images.length - 1}` : ""}</span>}
            <span className="lv-live" title={target.capabilities.watch ? "Changes arrive as they happen" : `Reconciled every ${target.capabilities.reconcile_seconds}s`}>
              <span className="lv-live-dot" /> {target.capabilities.watch ? "live" : `every ${target.capabilities.reconcile_seconds}s`}
            </span>
          </div>
        </div>
        <div className="page-head-actions">
          {caps.diff && <button type="button" className="btn" onClick={() => set("panel", "diff")}>Diff</button>}
          {caps.sync && <button type="button" className="btn btn-primary" onClick={() => set("panel", "sync")}>Sync</button>}
          <button type="button" className="btn" onClick={() => set("panel", "history")}>History</button>
          <button type="button" className="btn" onClick={refresh} disabled={refreshing} title="Ask the agent to read the source now">
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>
      </div>
      {gone && <div className="error-banner">This application no longer exists on {target.name}.</div>}

      <div className="lv-panels">
        <section className={`card lv-panel lv-panel-${HEALTH_TONE[app.health]}`}>
          <h2>App health</h2>
          <div className="lv-panel-value"><HealthIcon health={app.health} size={18} /> {app.health}</div>
          {app.health_message && <div className="lv-panel-detail">{app.health_message}</div>}
          <div className="lv-panel-foot muted">since {relativeTime(app.health_since)} · {app.resources} resources{app.pods ? ` · ${app.pods_ready}/${app.pods} pods ready` : ""}</div>
        </section>
        <section className={`card lv-panel${app.sync === "OutOfSync" ? " lv-panel-warning" : app.sync === "Synced" ? " lv-panel-success" : ""}`}>
          <h2>Sync status</h2>
          <div className="lv-panel-value"><SyncPill sync={app.sync} /></div>
          {app.revision ? (
            <div className="lv-panel-detail">
              to <span className="mono">{shortSha(app.revision)}</span> — {app.revision_message}
              {app.revision_author && <span className="muted"> by {app.revision_author}</span>}
            </div>
          ) : d ? (
            <div className="lv-panel-detail">
              to <Link to={`/pipelines/${d.pipeline_id}`} className="mono">{shortSha(d.sha)}</Link>
              {d.message && <> — {commitSubject(d.message)}</>}
            </div>
          ) : (
            <div className="lv-panel-detail muted">No desired state for this app yet.</div>
          )}
          <div className="lv-panel-foot muted">
            {app.source_error ? (
              <span className="lv-bad">Could not render {app.source_path}: {app.source_error}</span>
            ) : app.source_path ? (
              <>{app.source_path} · {app.source_tool}</>
            ) : caps.diff ? (
              "Waiting for the agent to render the source"
            ) : (
              "Compared with the image Forge last deployed"
            )}
          </div>
        </section>
        <section className={`card lv-panel${lastOp?.state === "failed" ? " lv-panel-danger" : ""}`}>
          <h2>Last sync</h2>
          {lastOp ? (
            <>
              <div className="lv-panel-value">
                <HealthIcon health={lastOp.state === "succeeded" ? "Healthy" : lastOp.state === "failed" ? "Degraded" : "Progressing"} size={18} />
                {lastOp.state === "succeeded" ? "Succeeded" : lastOp.state === "failed" ? "Failed" : lastOp.state === "approval" ? "Waiting for approval" : "In progress"}
              </div>
              <div className="lv-panel-detail">
                {lastOp.kind === "rollback" ? "Rollback" : "Sync"}{lastOp.revision && <> to <span className="mono">{shortSha(lastOp.revision)}</span></>}
                {" "}— {lastOp.result?.message || lastOp.message || "started " + relativeTime(lastOp.created_at)}
              </div>
              <div className="lv-panel-foot actor">
                <Avatar identity={lastOp.requested_by} size={18} /> <span className="actor-name">{displayName(lastOp.requested_by) || "unknown"}</span>
                <span className="muted">{relativeTime(lastOp.finished_at ?? lastOp.created_at)}</span>
              </div>
            </>
          ) : d ? (
            <>
              <div className="lv-panel-value"><HealthIcon health="Healthy" size={18} /> Deployed by Forge</div>
              <div className="lv-panel-detail">
                <Link to={`/jobs/${d.job_id}?pipeline=${d.pipeline_id}`}>Deploy job #{d.job_id}</Link> succeeded {relativeTime(d.at)}
              </div>
              <div className="lv-panel-foot actor">
                <Avatar identity={d.by} size={18} /> <span className="actor-name">{displayName(d.by) || "unknown"}</span>
              </div>
            </>
          ) : (
            <div className="lv-panel-detail muted">Nothing synced or deployed through Forge yet.</div>
          )}
        </section>
      </div>

      <div className="table-toolbar lv-toolbar">
        <div className="segmented" role="group" aria-label="View">
          <button type="button" className={mode === "tree" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set("view", null)}>Tree</button>
          {caps.pods && <button type="button" className={mode === "pods" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set("view", "pods")}>Pods</button>}
          {caps.pods && <button type="button" className={mode === "network" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set("view", "network")}>Network</button>}
          <button type="button" className={mode === "list" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set("view", "list")}>List</button>
        </div>
        <div className="state-chips" role="group" aria-label="Health">
          {present.map((h) => (
            <button key={h} type="button" aria-pressed={health.includes(h)}
              className={`fchip${health.includes(h) ? " fchip-on" : ""}`}
              onClick={() => setHealth((x) => (x.includes(h) ? x.filter((y) => y !== h) : [...x, h]))}>
              <HealthIcon health={h} size={12} /> {h}{" "}
              <span className="num">{view.tree.resources.filter((r) => r.health?.status === h).length}</span>
            </button>
          ))}
        </div>
        <div className="table-toolbar-right">
          <input className="filter-search" placeholder="Find a resource…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Find a resource" />
        </div>
      </div>

      {mode === "tree" || mode === "network" ? (
        <ResourceTree key={mode} app={app} resources={shown} selected={selected} onSelect={(uid) => set("node", uid)} filter={{ q, health }} />
      ) : mode === "pods" ? (
        <PodsView resources={view.tree.resources} selected={selected} onSelect={(uid) => set("node", uid)} />
      ) : (
        <ResourceList
          resources={view.tree.resources.filter((r) => (!q || r.name.toLowerCase().includes(q.toLowerCase())) && (health.length === 0 || (r.health && health.includes(r.health.status))))}
          selected={selected} onSelect={(uid) => set("node", uid)} />
      )}

      {node && (
        <ResourcePanel appId={app.id} resource={node} byUid={byUid} caps={target.capabilities}
          onSelect={(uid) => set("node", uid)} onClose={() => set("node", null)} />
      )}
      {selected && !node && <div className="muted lv-gone-node">That resource is gone.</div>}
      {panel === "diff" && <DiffPanel view={view} onClose={() => set("panel", null)} />}
      {panel === "sync" && <SyncDialog view={view} onClose={() => set("panel", null)} />}
      {panel === "history" && <HistoryPanel view={view} onClose={() => set("panel", null)} />}
      <p className="muted lv-foot">
        Updated {relativeTime(app.updated_at)} · version {view.tree.version}
        {target.last_seen_at && <> · agent seen {relativeTime(target.last_seen_at)}</>}
      </p>
    </div>
  );
}
