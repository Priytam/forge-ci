import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { createLiveTarget, deleteLiveTarget, listLiveTargets, relativeTime, updateLiveTarget, type LiveTarget } from "../api";
import ConfirmDialog from "../components/ui/ConfirmDialog";
import { Pill, Skeleton } from "../components/ui/Pill";
import { usePoll } from "../hooks/usePoll";

/** An agent is connected when it reported within twice its heartbeat. */
const CONNECTED_MS = 75_000;

/** "namespace = environment" lines ⇄ the target's override map. */
function formatNsEnvs(m: Record<string, string> | null): string {
  return Object.entries(m ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([ns, env]) => `${ns} = ${env}`)
    .join("\n");
}

function parseNsEnvs(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const i = line.indexOf("=");
    if (i < 0) throw new Error(`"${line}": write namespace = environment`);
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

function bytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

/**
 * Settings → Live agents: the clusters and cloud projects Forge Live watches.
 * Each has one forge-agent holding a token scoped to it; the agent dials out
 * to Forge, so Forge never holds cluster or cloud credentials.
 */
export default function AdminLive() {
  const fetcher = useCallback(() => listLiveTargets(), []);
  const { data, error, loading, refresh } = usePoll(fetcher, 10_000);
  const [form, setForm] = useState({ name: "", environment: "", repo: "" });
  const [created, setCreated] = useState<{ target: LiveTarget; token: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [removing, setRemoving] = useState<LiveTarget | null>(null);
  type Policy = { auto_sync: boolean; prune: boolean; self_heal: boolean };
  const [editing, setEditing] = useState<{ target: LiveTarget; environment: string; nsEnvs: string; policy: Policy | null } | null>(null);
  const [editError, setEditError] = useState<string | null>(null);

  const saveEdit = async () => {
    if (!editing) return;
    setBusy(true);
    setEditError(null);
    try {
      await updateLiveTarget(editing.target.id, {
        environment: editing.environment,
        namespace_environments: parseNsEnvs(editing.nsEnvs),
        default_policy: editing.policy,
      });
      setEditing(null);
      refresh();
    } catch (err) {
      setEditError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFormError(null);
    try {
      setCreated(await createLiveTarget(form));
      setForm({ name: "", environment: "", repo: "" });
      refresh();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  const server = window.location.origin;

  return (
    <div>
      <div className="page-head">
        <h1>Live agents</h1>
      </div>
      <p className="muted page-lede">
        Clusters and cloud projects whose applications show on{" "}
        <Link to="/applications">Applications</Link>. Each runs a forge-agent that reports to Forge over an outbound
        connection with a token scoped to it — Forge never holds cluster or cloud credentials.
      </p>

      <form className="card lv-admin-form" onSubmit={(e) => void create(e)}>
        <label>
          Name
          <input required placeholder="prod-asia (cluster) or acme-prod (project)" value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })} />
        </label>
        <label>
          Environment
          <input placeholder="production" value={form.environment} onChange={(e) => setForm({ ...form, environment: e.target.value })} />
        </label>
        <label>
          Repo it deploys from
          <input placeholder="acme/platform-gitops" value={form.repo} onChange={(e) => setForm({ ...form, repo: e.target.value })} />
        </label>
        <button type="submit" className="btn btn-primary" disabled={busy || !form.name.trim()}>Add</button>
        {formError && <div className="error-banner">{formError}</div>}
      </form>

      {created && (
        <div className="card runner-token-reveal">
          <div className="runner-token-reveal-head">
            <strong>{created.target.name} added</strong>
            <span className="badge badge-blocked">token shown once — copy it now</span>
          </div>
          <p className="muted">Kubernetes: run the agent in the cluster with a read-only ServiceAccount.</p>
          <pre className="lv-snippet">{`kubectl create namespace forge-live
kubectl -n forge-live create secret generic forge-agent \\
  --from-literal=FORGE_URL=${server} \\
  --from-literal=FORGE_AGENT_TOKEN=${created.token}
kubectl apply -f deploy/live/agent.yaml   # from a Forge checkout`}</pre>
          <p className="muted">Cloud Run: run it anywhere with a Cloud Run Viewer credential.</p>
          <pre className="lv-snippet">{`FORGE_URL=${server} FORGE_AGENT_TOKEN=${created.token} \\
  forge-agent --provider cloudrun --project <project> --regions <region,…>`}</pre>
          <button type="button" className="btn" onClick={() => setCreated(null)}>Done</button>
        </div>
      )}

      {error && <div className="error-banner">Failed to load: {error}</div>}
      {loading && !data && <Skeleton h={120} />}
      {data && data.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Agent</th>
                <th>Environment · repo</th>
                <th className="num">Apps</th>
                <th className="num">Objects</th>
                <th className="num">Agent memory</th>
                <th className="num">Sent</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.map((t) => {
                const seen = t.last_seen_at ? Date.now() - Date.parse(t.last_seen_at) : Infinity;
                const up = t.stats.uptime_seconds || 0;
                return (
                  <tr key={t.id}>
                    <td>
                      <Link to={`/applications?target=${encodeURIComponent(t.name)}`}>{t.name}</Link>
                      <div className="muted">{t.provider || "no agent yet"}{t.agent_version && ` · ${t.agent_version}`}</div>
                    </td>
                    <td>
                      {seen < CONNECTED_MS ? (
                        <Pill tone={t.synced ? "success" : "accent"} icon="dot">{t.synced ? "Connected" : "Reporting…"}</Pill>
                      ) : (
                        <Pill tone="neutral" icon="offline">{t.last_seen_at ? `Last seen ${relativeTime(t.last_seen_at)}` : "Never connected"}</Pill>
                      )}
                    </td>
                    <td className="muted">
                      {[t.environment, t.repo].filter(Boolean).join(" · ") || "—"}
                      {Object.entries(t.namespace_environments ?? {}).map(([ns, env]) => (
                        <div key={ns}>
                          {ns} → {env}
                        </div>
                      ))}
                    </td>
                    <td className="num">{t.apps}</td>
                    <td className="num">{t.stats.objects?.toLocaleString() ?? "—"}</td>
                    <td className="num">{t.stats.heap_bytes ? bytes(t.stats.heap_bytes) : "—"}</td>
                    <td className="num" title={`${bytes(t.stats.bytes_sent || 0)} since the agent started`}>
                      {up > 0 ? `${bytes(Math.round((t.stats.bytes_sent || 0) / up))}/s` : "—"}
                    </td>
                    <td>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => {
                          setEditError(null);
                          setEditing({
                            target: t,
                            environment: t.environment,
                            nsEnvs: formatNsEnvs(t.namespace_environments),
                            policy: t.default_policy,
                          });
                        }}
                      >
                        Edit
                      </button>{" "}
                      <button type="button" className="btn btn-sm" onClick={() => setRemoving(t)}>Remove</button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {data && data.length === 0 && !created && <div className="empty card">No clusters or projects yet.</div>}

      {editing && (
        <ConfirmDialog
          title={`Edit ${editing.target.name}`}
          confirmLabel="Save"
          busy={busy}
          onCancel={() => setEditing(null)}
          onConfirm={() => void saveEdit()}
        >
          <label className="lv-admin-field">
            Environment (apps in any other namespace)
            <input value={editing.environment} onChange={(e) => setEditing({ ...editing, environment: e.target.value })} />
          </label>
          <label className="lv-admin-field">
            Per namespace, one per line: <code>namespace = environment</code>
            <textarea
              rows={5}
              placeholder={"travafa-staging = staging\ntravafa-prod = production"}
              value={editing.nsEnvs}
              onChange={(e) => setEditing({ ...editing, nsEnvs: e.target.value })}
            />
          </label>
          <fieldset className="lv-admin-field">
            <legend>Default sync policy for new apps (existing apps keep theirs; bulk-change them from Applications → Sync policy…)</legend>
            {(["auto_sync", "prune", "self_heal"] as const).map((k) => (
              <label key={k} className="check-row">
                <input
                  type="checkbox"
                  checked={editing.policy?.[k] ?? false}
                  disabled={k !== "auto_sync" && !editing.policy?.auto_sync}
                  onChange={(e) => {
                    const p = { auto_sync: false, prune: false, self_heal: false, ...editing.policy, [k]: e.target.checked };
                    setEditing({ ...editing, policy: p.auto_sync ? p : null });
                  }}
                />
                {k === "auto_sync" ? "Auto-sync" : k === "prune" ? "Prune" : "Self-heal"}
              </label>
            ))}
          </fieldset>
          {editError && <div className="error-banner">{editError}</div>}
        </ConfirmDialog>
      )}

      {removing && (
        <ConfirmDialog
          title={`Remove ${removing.name}?`}
          confirmLabel="Remove"
          danger
          busy={busy}
          onCancel={() => setRemoving(null)}
          onConfirm={() => {
            setBusy(true);
            deleteLiveTarget(removing.id)
              .then(() => {
                setRemoving(null);
                refresh();
              })
              .catch((e) => setFormError(String(e?.message ?? e)))
              .finally(() => setBusy(false));
          }}
        >
          Its {removing.apps} applications leave Forge and its agent's token stops working at once.
        </ConfirmDialog>
      )}
    </div>
  );
}
