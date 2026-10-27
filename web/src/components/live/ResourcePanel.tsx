import { useEffect, useState } from "react";
import { getLiveEvents, getLiveManifest, relativeTime, type LiveCapabilities, type LiveEvent, type LiveResource } from "../../api";
import Drawer from "../ui/Drawer";
import { Skeleton } from "../ui/Pill";
import { HealthPill, KindIcon } from "./ui";
import { toYaml } from "./yaml";

type Tab = "summary" | "events" | "manifest";

/**
 * A resource in the slide-over: Summary, Events and Live manifest. Tabs
 * follow what the provider can show (capabilities), never its name.
 */
export default function ResourcePanel({
  appId,
  resource,
  byUid,
  caps,
  onSelect,
  onClose,
}: {
  appId: number;
  resource: LiveResource;
  byUid: Map<string, LiveResource>;
  caps: LiveCapabilities;
  onSelect: (uid: string) => void;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>("summary");
  const tabs: [Tab, string][] = [["summary", "Summary"]];
  if (caps.events) tabs.push(["events", "Events"]);
  if (caps.manifests) tabs.push(["manifest", "Live manifest"]);
  const r = resource;
  return (
    <Drawer
      wide
      onClose={onClose}
      title={
        <span className="lv-panel-title">
          <KindIcon kind={r.kind} size={18} /> {r.name}
        </span>
      }
      subtitle={
        <>
          <span>{r.kind}</span>
          {r.namespace && <span className="muted">· {r.namespace}</span>}
          {r.health && <HealthPill health={r.health.status} title={r.health.message} />}
        </>
      }
    >
      <div className="cr-tabs" role="tablist">
        {tabs.map(([id, label]) => (
          <button key={id} type="button" role="tab" aria-selected={tab === id}
            className={`cr-tab${tab === id ? " cr-tab-on" : ""}`} onClick={() => setTab(id)}>
            {label}
          </button>
        ))}
      </div>
      {tab === "summary" && <Summary r={r} byUid={byUid} onSelect={onSelect} />}
      {tab === "events" && <Events appId={appId} uid={r.uid} />}
      {tab === "manifest" && <Manifest appId={appId} uid={r.uid} />}
    </Drawer>
  );
}

function Summary({ r, byUid, onSelect }: { r: LiveResource; byUid: Map<string, LiveResource>; onSelect: (uid: string) => void }) {
  const s = r.summary;
  const parents = (r.parents ?? []).map((p) => byUid.get(p)).filter(Boolean) as LiveResource[];
  return (
    <>
      {r.health?.message && (
        <div className={`lv-health-note lv-tone-${r.health.status === "Degraded" ? "danger" : "neutral"}`}>{r.health.message}</div>
      )}
      <dl className="facts">
        <div><dt>Kind</dt><dd>{r.group ? `${r.kind} (${r.group}/${r.version})` : `${r.kind} (${r.version ?? "v1"})`}</dd></div>
        {r.namespace && <div><dt>Namespace</dt><dd>{r.namespace}</dd></div>}
        <div><dt>Created</dt><dd title={r.created_at}>{relativeTime(r.created_at)}</dd></div>
        {s.status && <div><dt>Status</dt><dd>{s.status}</dd></div>}
        {s.ready && <div><dt>Ready</dt><dd>{s.ready}</dd></div>}
        {s.restarts !== undefined && <div><dt>Restarts</dt><dd>{s.restarts}</dd></div>}
        {(s.info ?? []).map((kv) => (
          <div key={kv.name}><dt>{kv.name}</dt><dd className="lv-wrap">{kv.value}</dd></div>
        ))}
        {parents.map((p) => (
          <div key={p.uid}>
            <dt>Owned by</dt>
            <dd><button type="button" className="link-btn" onClick={() => onSelect(p.uid)}>{p.kind} {p.name}</button></dd>
          </div>
        ))}
        <div><dt>UID</dt><dd className="mono lv-wrap">{r.uid}</dd></div>
      </dl>
      {(s.images?.length ?? 0) > 0 && (
        <>
          <h3 className="drawer-h">Images</h3>
          <ul className="lv-images">
            {s.images!.map((img) => (
              <li key={img} className="mono">{img}</li>
            ))}
          </ul>
        </>
      )}
    </>
  );
}

function Events({ appId, uid }: { appId: number; uid: string }) {
  const [events, setEvents] = useState<LiveEvent[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setEvents(null);
    getLiveEvents(appId, uid).then(setEvents, (e) => setError(String(e?.message ?? e)));
  }, [appId, uid]);
  if (error) return <div className="error-banner">{error}</div>;
  if (!events) return <Skeleton h={60} />;
  if (events.length === 0) return <div className="empty">No events. The platform keeps them for about an hour.</div>;
  return (
    <ul className="lv-events">
      {events.map((e) => (
        <li key={e.uid} className={e.type === "Warning" ? "lv-event-warn" : undefined}>
          <div className="lv-event-head">
            <span className="lv-event-reason">{e.reason}</span>
            {e.count > 1 && <span className="badge2">×{e.count}</span>}
            <span className="muted" title={`first ${e.first_seen}, last ${e.last_seen}`}>{relativeTime(e.last_seen)}</span>
          </div>
          <div className="lv-event-msg">{e.message}</div>
        </li>
      ))}
    </ul>
  );
}

function Manifest({ appId, uid }: { appId: number; uid: string }) {
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setText(null);
    setError(null);
    getLiveManifest(appId, uid).then((m) => setText(toYaml(m)), (e) => setError(String(e?.message ?? e)));
  }, [appId, uid]);
  if (error) return <div className="error-banner">{error}</div>;
  if (text === null) return <Skeleton h={200} />;
  return (
    <>
      <div className="lv-manifest-bar">
        <span className="muted">As last reported by the agent. Managed fields and large data values are left out.</span>
        <button type="button" className="btn btn-sm" onClick={() => void navigator.clipboard?.writeText(text)}>Copy</button>
      </div>
      <pre className="lv-manifest">{text}</pre>
    </>
  );
}
