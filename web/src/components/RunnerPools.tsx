import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  createRunnerPool,
  deleteRunnerPool,
  evaluateRunnerPools,
  listRunnerPools,
  relativeTime,
  updateRunnerPool,
  type RunnerPool,
  type RunnerPoolsResponse,
} from "../api";

const PROVIDER_LABELS: Record<string, string> = {
  kubernetes: "Kubernetes (Deployment / StatefulSet)",
  docker: "Docker host (containers)",
  exec: "Command (any cloud: VM groups, Terraform…)",
};

const SETTINGS_EXAMPLE: Record<string, string> = {
  kubernetes: '{"namespace": "ci", "deployment": "forge-runners"}',
  docker: '{"image": "ghcr.io/you/forge-runner:1.9", "server_url": "https://forge.example.com", "runner_token": "frt_…", "executor": "docker", "docker_sock": "true"}',
  exec: '{"count_cmd": "aws autoscaling describe-auto-scaling-groups … --query \'AutoScalingGroups[0].DesiredCapacity\' --output text", "scale_cmd": "aws autoscaling set-desired-capacity … --desired-capacity $FORGE_DESIRED"}',
};

const blank = { name: "", provider: "kubernetes", tags: "", min_runners: 0, max_runners: 5, jobs_per_runner: 1, scale_down_after_s: 600, settings: "", enabled: true };

/** Admin: autoscaled runner pools — what the queue wants, what each pool
 *  has, and the form to add or edit one. */
export default function RunnerPools() {
  const [data, setData] = useState<RunnerPoolsResponse | null>(null);
  const [editing, setEditing] = useState<RunnerPool | null>(null);
  const [form, setForm] = useState(blank);
  const [open, setOpen] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const reload = useCallback(() => {
    void listRunnerPools()
      .then(setData)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(() => {
    reload();
    const t = window.setInterval(reload, 10000);
    return () => window.clearInterval(t);
  }, [reload]);
  if (!data) return null;

  const startEdit = (p: RunnerPool) => {
    setEditing(p);
    setForm({ name: p.name, provider: p.provider, tags: p.tags.join(", "), min_runners: p.min_runners, max_runners: p.max_runners,
      jobs_per_runner: p.jobs_per_runner, scale_down_after_s: p.scale_down_after_s, settings: p.settings ?? "", enabled: p.enabled });
    setOpen(true);
    setMsg(null);
  };
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    const input = { ...form, tags: form.tags.split(",").map((s) => s.trim()).filter(Boolean), settings: form.settings.trim() || undefined };
    try {
      if (editing) await updateRunnerPool(editing.id, input);
      else await createRunnerPool(input);
      setMsg({ ok: true, text: editing ? "Pool updated." : "Pool added. The first pass runs within 30 seconds." });
      setEditing(null);
      setForm(blank);
      setOpen(false);
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };
  const remove = async (p: RunnerPool) => {
    if (!window.confirm(`Delete pool "${p.name}"? Its runners are left as they are.`)) return;
    await deleteRunnerPool(p.id).catch((e) => setMsg({ ok: false, text: String(e) }));
    reload();
  };
  const evaluate = async () => {
    setMsg(null);
    try {
      setData(await evaluateRunnerPools());
      setMsg({ ok: true, text: "Evaluated." });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  return (
    <div className="card settings-form" style={{ marginTop: 16 }}>
      <div className="sso-card-head">
        <h2>Autoscaled pools</h2>
        <div>
          {data.pools.length > 0 && (
            <button type="button" className="btn" onClick={() => void evaluate()}>
              Evaluate now
            </button>
          )}{" "}
          <button type="button" className="btn btn-primary" onClick={() => { setEditing(null); setForm(blank); setOpen((o) => !o); }}>
            {open && !editing ? "Close" : "Add pool"}
          </button>
        </div>
      </div>
      <p className="muted">
        A pool is a set of identical runners Forge grows and shrinks with the queue — a Kubernetes
        Deployment, containers on a Docker host, or anything a command can resize (cloud VM groups,
        Terraform). Scale-up is immediate; scale-down waits for an idle window and removes one runner
        per pass, letting it drain. See <code>docs/autoscaling.md</code>.
      </p>
      {data.pools.length > 0 && (
        <div className="table-card">
          <table className="dense-table">
            <thead>
              <tr>
                <th>Pool</th>
                <th>Provider</th>
                <th>Tags</th>
                <th>Runners</th>
                <th>Pending</th>
                <th>Last action</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {data.pools.map((p) => (
                <tr key={p.id} style={p.enabled ? undefined : { opacity: 0.55 }}>
                  <td>
                    {p.name}
                    {!p.enabled && <span className="muted"> (off)</span>}
                  </td>
                  <td>{p.provider}</td>
                  <td className="mono">{p.tags.join(", ") || "—"}</td>
                  <td className="mono" title={`min ${p.min_runners}, max ${p.max_runners}, ${p.jobs_per_runner} job(s) each`}>
                    {p.current_runners}
                    {p.desired_runners !== p.current_runners && <span className="muted"> → {p.desired_runners}</span>}
                    <span className="muted"> / {p.max_runners}</span>
                  </td>
                  <td className="mono">{p.pending_jobs}</td>
                  <td className="muted" style={{ maxWidth: 280 }}>
                    {p.last_error ? (
                      <span className="error-text" title={p.last_error}>error: {p.last_error.slice(0, 80)}</span>
                    ) : (
                      <span title={p.last_action}>{p.last_action.slice(0, 60) || "—"}</span>
                    )}
                    {p.checked_at && <span> · {relativeTime(p.checked_at)}</span>}
                  </td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    <button type="button" className="btn btn-icon" onClick={() => startEdit(p)}>
                      Edit
                    </button>{" "}
                    <button type="button" className="btn btn-danger-quiet" onClick={() => void remove(p)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {open && (
        <form onSubmit={(e) => void save(e)} style={{ marginTop: 12 }}>
          <h3>{editing ? `Edit ${editing.name}` : "New pool"}</h3>
          <div className="form-row">
            <label className="field">
              <span>Name</span>
              <input className="mono" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="linux" required />
            </label>
            <label className="field">
              <span>Provider</span>
              <select value={form.provider} onChange={(e) => setForm({ ...form, provider: e.target.value })}>
                {data.providers.map((p) => (
                  <option key={p} value={p}>
                    {PROVIDER_LABELS[p] ?? p}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Runner tags</span>
              <input className="mono" value={form.tags} onChange={(e) => setForm({ ...form, tags: e.target.value })} placeholder="linux, docker" />
            </label>
          </div>
          <div className="form-row">
            <label className="field">
              <span>Min</span>
              <input type="number" min={0} value={form.min_runners} onChange={(e) => setForm({ ...form, min_runners: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>Max</span>
              <input type="number" min={1} value={form.max_runners} onChange={(e) => setForm({ ...form, max_runners: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>Jobs per runner</span>
              <input type="number" min={1} value={form.jobs_per_runner} onChange={(e) => setForm({ ...form, jobs_per_runner: Number(e.target.value) })} />
            </label>
            <label className="field">
              <span>Scale down after (s)</span>
              <input type="number" min={60} value={form.scale_down_after_s} onChange={(e) => setForm({ ...form, scale_down_after_s: Number(e.target.value) })} />
            </label>
          </div>
          <label className="field">
            <span>Provider settings (JSON)</span>
            <textarea className="mono" rows={4} value={form.settings} onChange={(e) => setForm({ ...form, settings: e.target.value })} placeholder={SETTINGS_EXAMPLE[form.provider]} />
            <span className="field-hint">Secret keys (tokens) are stored encrypted and shown masked; leave a masked value to keep it.</span>
          </label>
          <label className="check-row">
            <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
            Enabled
          </label>
          <div className="form-actions">
            <button type="submit" className="btn btn-primary">
              {editing ? "Save pool" : "Add pool"}
            </button>
            <button type="button" className="btn" onClick={() => { setOpen(false); setEditing(null); setForm(blank); }}>
              Cancel
            </button>
          </div>
        </form>
      )}
      {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
    </div>
  );
}
