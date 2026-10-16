import { Fragment, useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { CodeBlock } from "../components/DocBlocks";
import {
  deleteCatalogTemplate,
  getIsAdmin,
  listCatalog,
  publishCatalogTemplate,
  type CatalogTemplate,
} from "../api";

const EXAMPLE = `spec:
  inputs:
    environment: {default: staging, options: [staging, production]}
    cluster: {description: "Where to deploy"}
---
jobs:
  deploy:
    stage: deploy
    environment: $[[ inputs.environment ]]
    script:
      - ./deploy.sh --cluster $[[ inputs.cluster ]]
`;

/** The include snippet a repo pastes to use this template version. */
function includeSnippet(t: CatalogTemplate): string {
  const required = t.inputs.filter((i) => i.required);
  const lines = ["include:", `  - template: ${t.name}@${t.version}`];
  if (required.length > 0) {
    lines.push("    inputs:");
    for (const i of required) lines.push(`      ${i.name}: ${i.options?.[0] ?? "..."}`);
  }
  return lines.join("\n");
}

/**
 * Org-wide, versioned pipeline templates. Everyone can browse and copy the
 * include line; admins publish versions (immutable once published).
 */
export default function TemplateCatalog() {
  const [isAdmin, setIsAdmin] = useState(false);
  const [all, setAll] = useState<CatalogTemplate[] | null>(null);
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<string | null>(null); // name@version
  const [form, setForm] = useState({ name: "", version: "", description: "", yaml: EXAMPLE });
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const reload = useCallback(() => {
    void listCatalog()
      .then(setAll)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);

  // Group versions under their template name (the API returns newest first).
  const groups = useMemo(() => {
    const q = filter.trim().toLowerCase();
    const by = new Map<string, CatalogTemplate[]>();
    for (const t of all ?? []) {
      if (q && !t.name.includes(q) && !t.description.toLowerCase().includes(q)) continue;
      by.set(t.name, [...(by.get(t.name) ?? []), t]);
    }
    return [...by.entries()];
  }, [all, filter]);

  const publish = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      const t = await publishCatalogTemplate({ ...form, name: form.name.trim(), version: form.version.trim() });
      setMsg({ ok: true, text: `Published ${t.name}@${t.version}.` });
      setForm({ name: form.name, version: "", description: form.description, yaml: form.yaml });
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  const remove = async (t: CatalogTemplate) => {
    if (!window.confirm(`Remove ${t.name}@${t.version}? Pipelines pinned to this version will stop compiling.`)) return;
    try {
      await deleteCatalogTemplate(t.name, t.version);
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Template catalog</h1>
      </div>
      <p className="page-lede">
        Shared, versioned pipeline templates for every repo. Include one with{" "}
        <code>template: name@version</code> (or just <code>name</code> for the newest) and pass its{" "}
        <code>inputs:</code>. Versions never change once published, so a pinned pipeline always
        builds the same way. Templates can also come from another repo (<code>project:</code>) or a
        URL (<code>remote:</code>) — see the pipeline docs.
      </p>

      {all && all.length > 0 && (
        <div className="filter-bar" style={{ marginBottom: 12 }}>
          <input
            type="search"
            placeholder="Filter templates"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Filter templates"
          />
        </div>
      )}

      {groups.length > 0 && (
        <div className="card table-card" style={{ marginBottom: 16 }}>
          <table className="dense-table">
            <thead>
              <tr>
                <th>Template</th>
                <th>Latest</th>
                <th>Inputs</th>
                <th>Description</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {groups.map(([name, versions]) => {
                const latest = versions[0];
                const key = `${name}@${latest.version}`;
                const shown = versions.find((v) => `${name}@${v.version}` === open) ?? (open?.startsWith(`${name}@`) ? latest : null);
                return (
                  <Fragment key={name}>
                    <tr>
                      <td className="mono">{name}</td>
                      <td className="mono">
                        {latest.version}
                        {versions.length > 1 && <span className="muted"> (+{versions.length - 1} older)</span>}
                      </td>
                      <td className="mono muted">
                        {latest.inputs.length === 0
                          ? "—"
                          : latest.inputs.map((i) => (i.required ? `${i.name}*` : i.name)).join(", ")}
                      </td>
                      <td className="muted">{latest.description}</td>
                      <td style={{ textAlign: "right" }}>
                        <button type="button" className="btn btn-icon" onClick={() => setOpen(shown ? null : key)}>
                          {shown ? "Hide" : "Use"}
                        </button>
                      </td>
                    </tr>
                    {shown && (
                      <tr>
                        <td colSpan={5}>
                          <div className="form-row" style={{ alignItems: "flex-start" }}>
                            <label className="field" style={{ maxWidth: 200 }}>
                              <span>Version</span>
                              <select value={shown.version} onChange={(e) => setOpen(`${name}@${e.target.value}`)}>
                                {versions.map((v) => (
                                  <option key={v.version} value={v.version}>
                                    {v.version}
                                  </option>
                                ))}
                              </select>
                            </label>
                            <div className="field" style={{ flex: 1 }}>
                              <span>Add to .forge-ci.yml</span>
                              <CodeBlock code={includeSnippet(shown)} />
                            </div>
                          </div>
                          {shown.inputs.length > 0 && (
                            <table className="dense-table" style={{ marginBottom: 12 }}>
                              <thead>
                                <tr>
                                  <th>Input</th>
                                  <th>Default</th>
                                  <th>Allowed</th>
                                  <th>Description</th>
                                </tr>
                              </thead>
                              <tbody>
                                {shown.inputs.map((i) => (
                                  <tr key={i.name}>
                                    <td className="mono">{i.name}</td>
                                    <td className="mono">{i.required ? <span className="muted">required</span> : i.default}</td>
                                    <td className="mono muted">{i.options?.join(", ") ?? "any"}</td>
                                    <td className="muted">{i.description}</td>
                                  </tr>
                                ))}
                              </tbody>
                            </table>
                          )}
                          <details>
                            <summary>Template source</summary>
                            <CodeBlock code={shown.yaml} />
                          </details>
                          <div className="muted" style={{ marginTop: 8 }}>
                            Published by {shown.published_by || "unknown"} on {new Date(shown.created_at).toLocaleDateString()}
                            {isAdmin && (
                              <>
                                {" · "}
                                <button type="button" className="btn btn-icon" onClick={() => void remove(shown)}>
                                  Remove this version
                                </button>
                              </>
                            )}
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {all && all.length === 0 && (
        <div className="empty card">
          No templates published yet.{isAdmin ? " Publish the first one below." : " Ask an admin to publish one."}
        </div>
      )}

      {isAdmin && (
        <form className="card settings-form" onSubmit={(e) => void publish(e)}>
          <h2>Publish a version</h2>
          <div className="form-row">
            <label className="field">
              <span>Name</span>
              <input className="mono" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="deploy/kubernetes" required />
            </label>
            <label className="field" style={{ maxWidth: 160 }}>
              <span>Version</span>
              <input className="mono" value={form.version} onChange={(e) => setForm({ ...form, version: e.target.value })} placeholder="1.0.0" required />
            </label>
            <label className="field">
              <span>Description</span>
              <input value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} placeholder="Rolling deploy to any cluster" />
            </label>
          </div>
          <label className="field">
            <span>Template YAML (optional spec: inputs: header, then ---, then jobs)</span>
            <textarea className="mono" rows={14} value={form.yaml} onChange={(e) => setForm({ ...form, yaml: e.target.value })} />
          </label>
          <div className="form-actions">
            <button type="submit" className="btn btn-primary">
              Publish
            </button>
          </div>
          {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
        </form>
      )}
      {!isAdmin && msg && !msg.ok && <div className="error-banner">{msg.text}</div>}
    </div>
  );
}
