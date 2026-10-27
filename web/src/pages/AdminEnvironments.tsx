import Skeleton from "../components/ui/Skeleton";
import { useEffect, useState } from "react";
import {
  getEnvRegistry,
  getIsAdmin,
  putEnvRegistry,
  type EnvPolicy,
  type EnvTier,
  type RegisteredEnv,
} from "../api";

/** Form row: aliases and branches are edited as comma-separated text. */
interface Row {
  name: string;
  tier: EnvTier;
  aliases: string;
  allowed_refs: string;
  description: string;
  cancel_superseded: boolean;
}

const split = (s: string) =>
  s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);

const toRow = (e: RegisteredEnv): Row => ({
  name: e.name,
  tier: e.tier,
  aliases: e.aliases.join(", "),
  allowed_refs: e.allowed_refs.join(", "),
  description: e.description,
  cancel_superseded: !!e.cancel_superseded,
});

/** A sensible starting point for a server with no registry yet. */
const STARTER: Row[] = [
  { name: "dev", tier: "dev", aliases: "development", allowed_refs: "", description: "", cancel_superseded: false },
  { name: "staging", tier: "staging", aliases: "stage, stg", allowed_refs: "main, release/*", description: "", cancel_superseded: false },
  {
    name: "production",
    tier: "prod",
    aliases: "prod, prd",
    allowed_refs: "main, release/*",
    description: "",
    cancel_superseded: true,
  },
];

/**
 * Admin: this server's environments — the real names, their order, tier
 * (colour), aliases that mean the same environment, and which branches may
 * deploy to each. Saved per server, so each company configures its own.
 */
export default function AdminEnvironments() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [rows, setRows] = useState<Row[] | null>(null);
  const [unknown, setUnknown] = useState<EnvPolicy["unknown"]>("allow");
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    void getEnvRegistry(true).then((p) => {
      setRows(p.environments.map(toRow));
      setUnknown(p.unknown);
    });
  }, []);

  if (isAdmin === false) {
    return <div className="error-banner">Environments are managed by admins.</div>;
  }
  if (!rows) return <Skeleton />;

  const update = (i: number, patch: Partial<Row>) =>
    setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const move = (i: number, d: number) => {
    const next = [...rows];
    const [r] = next.splice(i, 1);
    next.splice(i + d, 0, r);
    setRows(next);
  };

  const save = async () => {
    setSaving(true);
    setMsg(null);
    try {
      const saved = await putEnvRegistry({
        unknown,
        environments: rows.map((r) => ({
          name: r.name.trim(),
          tier: r.tier,
          aliases: split(r.aliases),
          allowed_refs: split(r.allowed_refs),
          description: r.description.trim(),
          cancel_superseded: r.cancel_superseded,
        })),
      });
      setRows(saved.environments.map(toRow));
      setMsg({ ok: true, text: "Saved. New pipelines follow these rules." });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Environments &amp; branches</h1>
      </div>
      <div className="muted page-lede">
        <p>
          The environments this server deploys to, in promotion order. Pipelines
          name an environment with <code>environment:</code> on a job; an alias
          (e.g. <code>prd</code>) is stored under the real name. A job whose branch
          is not allowed for its environment fails without running, and says why.
        </p>
        <p>
          Saving renames aliases already stored in past jobs, deployments,
          approval rules, freezes and scoped variables. Branch rules apply to new
          pipelines. With no environments listed, nothing is enforced.
        </p>
      </div>

      {rows.length === 0 && (
        <div className="empty card">
          No environments yet — any name is accepted and any branch may deploy
          anywhere.{" "}
          <button type="button" className="btn" onClick={() => setRows(STARTER)}>
            Start from dev / staging / production
          </button>
        </div>
      )}

      {rows.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Order</th>
                <th>Name</th>
                <th>Tier</th>
                <th>Aliases</th>
                <th>Branches allowed to deploy</th>
                <th>Description</th>
                <th title="Cancel a deploy that is still waiting once a newer pipeline has deployed here">Cancel superseded</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={i}>
                  <td>
                    <button type="button" className="btn btn-icon" disabled={i === 0} onClick={() => move(i, -1)} aria-label="Move up">
                      ↑
                    </button>
                    <button
                      type="button"
                      className="btn btn-icon"
                      disabled={i === rows.length - 1}
                      onClick={() => move(i, 1)}
                      aria-label="Move down"
                    >
                      ↓
                    </button>
                  </td>
                  <td>
                    <input value={r.name} onChange={(e) => update(i, { name: e.target.value })} placeholder="production" />
                  </td>
                  <td>
                    <select value={r.tier} onChange={(e) => update(i, { tier: e.target.value as EnvTier })}>
                      <option value="dev">dev</option>
                      <option value="staging">staging</option>
                      <option value="prod">prod</option>
                      <option value="other">other</option>
                    </select>
                  </td>
                  <td>
                    <input value={r.aliases} onChange={(e) => update(i, { aliases: e.target.value })} placeholder="prod, prd" />
                  </td>
                  <td>
                    <input
                      value={r.allowed_refs}
                      onChange={(e) => update(i, { allowed_refs: e.target.value })}
                      placeholder="any branch (e.g. main, release/*)"
                    />
                  </td>
                  <td>
                    <input value={r.description} onChange={(e) => update(i, { description: e.target.value })} />
                  </td>
                  <td style={{ textAlign: "center" }}>
                    <input
                      type="checkbox"
                      checked={r.cancel_superseded}
                      onChange={(e) => update(i, { cancel_superseded: e.target.checked })}
                      aria-label={`Cancel superseded deploys to ${r.name || "this environment"}`}
                      title="Cancel a deploy that is still waiting once a newer pipeline has deployed here"
                    />
                  </td>
                  <td>
                    <button type="button" className="btn btn-icon" onClick={() => setRows(rows.filter((_, j) => j !== i))}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="filter-bar" style={{ marginTop: 12 }}>
        <button
          type="button"
          className="btn"
          onClick={() => setRows([...rows, { name: "", tier: "other", aliases: "", allowed_refs: "", description: "", cancel_superseded: false }])}
        >
          + Add environment
        </button>
        <label className="field">
          <span>Jobs naming an unlisted environment</span>
          <select value={unknown} onChange={(e) => setUnknown(e.target.value as EnvPolicy["unknown"])}>
            <option value="allow">Allow (use the name as written)</option>
            <option value="deny">Refuse (fail the job, catches typos)</option>
          </select>
        </label>
        <button type="button" className="btn btn-primary" disabled={saving} onClick={() => void save()}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
      {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      <p className="muted">
        Patterns: <code>*</code> matches anything, including <code>/</code> —{" "}
        <code>release/*</code> covers <code>release/1.2</code>. Leave branches
        empty to allow any branch.
      </p>
    </div>
  );
}
