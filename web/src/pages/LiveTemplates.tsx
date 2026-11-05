import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  deleteLiveTemplate,
  getFleet,
  listLiveTargets,
  listLiveTemplates,
  relativeTime,
  saveLiveTemplate,
  type FleetPage,
  type LiveTarget,
  type LiveTemplate,
  type LiveTemplateInput,
} from "../api";
import { HealthIcon, SyncIcon } from "../components/live/ui";
import Drawer from "../components/ui/Drawer";
import { Skeleton } from "../components/ui/Pill";

const EMPTY: LiveTemplateInput = {
  name: "", app_name: "", namespace: "", group: "", path: "apps/{name}/overlays/{target}", targets: [], environments: [],
  create_namespace: true, auto_sync: true, prune: false, self_heal: false,
};

/** Which targets a template declares its app on. */
export function matchingTargets<T extends Pick<LiveTarget, "name" | "environment">>(t: Pick<LiveTemplate, "targets" | "environments">, targets: T[]): T[] {
  return targets.filter((x) => (t.targets.length === 0 || t.targets.includes(x.name)) &&
    (t.environments.length === 0 || t.environments.includes(x.environment)));
}

/**
 * App templates: one application declared on many clusters. Forge fills in
 * {target}, {environment} and {name} per cluster, its agents render the app
 * before anything of it exists, and the template's sync policy brings it up.
 */
export default function LiveTemplates() {
  const [templates, setTemplates] = useState<LiveTemplate[] | null>(null);
  const [targets, setTargets] = useState<LiveTarget[]>([]);
  const [editing, setEditing] = useState<LiveTemplate | "new" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(() => {
    listLiveTemplates().then(setTemplates, (e) => setError(String(e?.message ?? e)));
    listLiveTargets().then(setTargets, () => undefined);
  }, []);
  useEffect(load, [load]);

  return (
    <div className="lv-templates">
      <div className="page-head">
        <h1>Templates</h1>
        <div className="page-head-actions">
          <Link className="btn" to="/applications/fleet">Fleet</Link>
          <button type="button" className="btn btn-primary" onClick={() => setEditing("new")}>New template</button>
        </div>
      </div>
      <p className="muted lv-templates-intro">
        A template declares one application on every cluster it matches. Its path and namespace may use{" "}
        <code>{"{target}"}</code>, <code>{"{environment}"}</code> and <code>{"{name}"}</code> — a Kustomize overlay per cluster is the usual shape.
        Each app gets the template&apos;s sync policy once; a change made to one app by hand stands.
      </p>
      {error && <div className="error-banner">{error}</div>}
      {!templates && !error && <Skeleton h={160} />}
      {templates && templates.length === 0 && <div className="empty card">No templates yet.</div>}
      {templates?.map((t) => <TemplateCard key={t.id} t={t} targets={targets} onEdit={() => setEditing(t)} />)}
      {editing && (
        <TemplateForm template={editing === "new" ? null : editing} targets={targets}
          onClose={() => setEditing(null)} onSaved={() => { setEditing(null); load(); }} />
      )}
    </div>
  );
}

function TemplateCard({ t, targets, onEdit }: { t: LiveTemplate; targets: LiveTarget[]; onEdit: () => void }) {
  const [fleet, setFleet] = useState<FleetPage | null>(null);
  useEffect(() => {
    getFleet({ template: t.name }).then(setFleet, () => undefined);
  }, [t.name, t.updated_at]);
  const on = matchingTargets(t, targets);
  const row = fleet?.rows[0];
  return (
    <section className="card lv-template">
      <div className="lv-template-head">
        <h2>{t.name}</h2>
        <span className="mono muted">{t.path}</span>
        <span className="lv-template-chips">
          {t.auto_sync ? <span className="lv-auto">auto-sync{t.prune ? " · prune" : ""}{t.self_heal ? " · self-heal" : ""}</span> : <span className="lv-auto">manual sync</span>}
          {t.create_namespace && <span className="lv-auto">creates namespace</span>}
        </span>
        <button type="button" className="btn btn-sm" onClick={onEdit}>Edit</button>
      </div>
      <div className="muted lv-template-sel">
        app <strong>{t.app_name}</strong> in <span className="mono">{t.namespace}</span> on{" "}
        {t.targets.length ? t.targets.join(", ") : "every cluster"}
        {t.environments.length ? ` in ${t.environments.join(", ")}` : ""}
      </div>
      <ul className="lv-template-targets">
        {on.length === 0 && <li className="muted">No cluster matches yet.</li>}
        {on.map((x) => {
          const c = row?.cells[String(x.id)];
          return (
            <li key={x.id}>
              <span className="lv-template-target">{x.name}</span>
              <span className="muted">{x.environment}</span>
              {c ? (
                <Link to={`/applications/${c.app_id}`} className="lv-template-status">
                  <HealthIcon health={c.health} size={13} /> {c.health} <SyncIcon sync={c.sync} size={13} /> {c.sync === "OutOfSync" ? "Out of sync" : c.sync}
                </Link>
              ) : (
                <span className="muted">waiting for the agent{x.last_seen_at ? ` (seen ${relativeTime(x.last_seen_at)})` : ""}</span>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function TemplateForm({ template, targets, onClose, onSaved }: {
  template: LiveTemplate | null;
  targets: LiveTarget[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [t, setT] = useState<LiveTemplateInput>(template ?? EMPTY);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const envs = [...new Set(targets.map((x) => x.environment).filter(Boolean))].sort();
  const field = <K extends keyof LiveTemplateInput>(k: K, v: LiveTemplateInput[K]) => setT((x) => ({ ...x, [k]: v }));
  const toggle = (k: "targets" | "environments", v: string) =>
    field(k, t[k].includes(v) ? t[k].filter((x) => x !== v) : [...t[k], v]);
  const on = matchingTargets(t, targets);
  const preview = on[0] && t.path.replace(/\{target\}/g, on[0].name).replace(/\{environment\}/g, on[0].environment).replace(/\{name\}/g, t.app_name || t.name);
  const save = () => {
    setBusy(true);
    setError(null);
    saveLiveTemplate(t, template?.id).then(onSaved, (e) => setError(String(e?.message ?? e))).finally(() => setBusy(false));
  };
  const remove = () => {
    if (!template || !window.confirm(`Delete template ${template.name}? Its applications keep running; Forge stops declaring them.`)) return;
    deleteLiveTemplate(template.id).then(onSaved, (e) => setError(String(e?.message ?? e)));
  };
  return (
    <Drawer wide title={template ? `Edit ${template.name}` : "New template"} onClose={onClose}>
      <div className="lv-template-form">
        <label className="lv-field">Name<input value={t.name} onChange={(e) => field("name", e.target.value)} placeholder="catalog-edge" /></label>
        <label className="lv-field">Application name<input value={t.app_name} onChange={(e) => field("app_name", e.target.value)} placeholder={t.name || "same as the name"} /></label>
        <label className="lv-field">Namespace<input className="mono" value={t.namespace} onChange={(e) => field("namespace", e.target.value)} placeholder="edge-{environment}" /></label>
        <label className="lv-field">Part of<input value={t.group} onChange={(e) => field("group", e.target.value)} placeholder="optional" /></label>
        <label className="lv-field lv-field-wide">Path in each cluster&apos;s source
          <input className="mono" value={t.path} onChange={(e) => field("path", e.target.value)} />
          {preview && <span className="muted">on {on[0].name}: <span className="mono">{preview}</span></span>}
        </label>
      </div>
      <h3 className="drawer-h">Clusters</h3>
      <p className="muted lv-template-help">None ticked means every one.</p>
      <div className="lv-template-pick">
        <div>
          <div className="muted">Environments</div>
          {envs.map((e) => (
            <label key={e} className="check-row"><input type="checkbox" checked={t.environments.includes(e)} onChange={() => toggle("environments", e)} /> {e}</label>
          ))}
        </div>
        <div>
          <div className="muted">Clusters and projects</div>
          {targets.map((x) => (
            <label key={x.id} className="check-row"><input type="checkbox" checked={t.targets.includes(x.name)} onChange={() => toggle("targets", x.name)} /> {x.name} <span className="muted">{x.environment}</span></label>
          ))}
        </div>
      </div>
      <p className="lv-template-on">Declares the app on <strong>{on.length}</strong> of {targets.length}: {on.map((x) => x.name).join(", ") || "none"}</p>
      <h3 className="drawer-h">Sync policy</h3>
      <div className="lv-options lv-policy">
        <label className="check-row"><input type="checkbox" checked={t.create_namespace} onChange={(e) => field("create_namespace", e.target.checked)} /> Create the namespace when it is missing</label>
        <label className="check-row"><input type="checkbox" checked={t.auto_sync} onChange={(e) => setT((x) => ({ ...x, auto_sync: e.target.checked, prune: e.target.checked && x.prune, self_heal: e.target.checked && x.self_heal }))} /> <span><strong>Auto-sync</strong> — bring the app up, and keep it at Git&apos;s revision</span></label>
        <label className={`check-row lv-policy-sub${t.auto_sync ? "" : " lv-off"}`}><input type="checkbox" disabled={!t.auto_sync} checked={t.prune} onChange={(e) => field("prune", e.target.checked)} /> <span><strong>Prune</strong></span></label>
        <label className={`check-row lv-policy-sub${t.auto_sync ? "" : " lv-off"}`}><input type="checkbox" disabled={!t.auto_sync} checked={t.self_heal} onChange={(e) => field("self_heal", e.target.checked)} /> <span><strong>Self-heal</strong></span></label>
      </div>
      {error && <div className="error-banner">{error}</div>}
      <div className="lv-dialog-actions">
        {template && <button type="button" className="btn btn-danger lv-left" onClick={remove}>Delete</button>}
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button type="button" className="btn btn-primary" disabled={busy || !t.name || !t.namespace || !t.path} onClick={save}>{template ? "Save" : "Create"}</button>
      </div>
    </Drawer>
  );
}
