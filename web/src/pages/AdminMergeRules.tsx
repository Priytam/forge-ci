import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  deleteHostIdentity,
  deleteMergePolicy,
  getIsAdmin,
  listApproverGroups,
  listHostIdentities,
  listMergePolicies,
  putHostIdentity,
  putMergePolicy,
  type ApproverGroup,
  type HostIdentity,
  type MergePolicy,
} from "../api";

const ROLES = ["admin", "owner", "developer"];

const BLANK: MergePolicy = {
  repo: "",
  branch_pattern: "main",
  required_approvals: 1,
  approver_roles: [...ROLES],
  required_group: "",
  required_group_approvals: 0,
  allow_self_approval: false,
  dismiss_on_push: true,
  block_on_changes_requested: true,
  require_ci: true,
  required_jobs: [],
  required_labels: [],
  blocked_labels: [],
  ticket_pattern: "",
  freeze_windows: [],
  timezone: "UTC",
  require_code_owners: false,
  merge_queue: false,
};

const list = (s: string) => s.split(/[\s,]+/).filter(Boolean);

/** "fri 18:00 - mon 09:00", one window per line. */
const parseWindows = (s: string) =>
  s
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [start, end] = l.split(/\s*[-–→]\s*/);
      return { start: start ?? "", end: end ?? "" };
    });

function describe(p: MergePolicy): string {
  const parts = [`${p.required_approvals} approval${p.required_approvals === 1 ? "" : "s"}`];
  if (p.required_group) parts.push(`${p.required_group_approvals} from ${p.required_group}`);
  if (p.require_ci) parts.push("CI green");
  if (p.required_jobs.length) parts.push(`jobs ${p.required_jobs.join(", ")}`);
  if (p.ticket_pattern) parts.push(`ticket ${p.ticket_pattern}`);
  if (p.freeze_windows.length) parts.push(`${p.freeze_windows.length} freeze window(s)`);
  if (!p.allow_self_approval) parts.push("no self-approval");
  if (p.dismiss_on_push) parts.push("re-approve after push");
  if (p.require_code_owners) parts.push("code owners");
  if (p.merge_queue) parts.push("merge queue");
  return parts.join(" · ");
}

/**
 * Admin: the rules between a change and its merge, per repo and target
 * branch, and which host login is which Forge user. A change no rule matches
 * has no gate (nothing is published to its host).
 */
export default function AdminMergeRules() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [rules, setRules] = useState<MergePolicy[] | null>(null);
  const [groups, setGroups] = useState<ApproverGroup[]>([]);
  const [ids, setIds] = useState<HostIdentity[]>([]);
  const [form, setForm] = useState<MergePolicy>(BLANK);
  const [text, setText] = useState({ jobs: "", required: "", blocked: "", windows: "" });
  const [ident, setIdent] = useState<HostIdentity>({ provider: "github", login: "", email: "" });
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const fail = (e: unknown) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
  const reload = useCallback(() => {
    void listMergePolicies().then(setRules).catch(fail);
    void listHostIdentities().then(setIds).catch(fail);
    void listApproverGroups().then(setGroups).catch(() => undefined);
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);
  if (isAdmin === false) return <div className="error-banner">Merge rules are managed by admins.</div>;

  const set = <K extends keyof MergePolicy>(k: K, v: MergePolicy[K]) => setForm((f) => ({ ...f, [k]: v }));
  const edit = (p: MergePolicy) => {
    setForm(p);
    setText({
      jobs: p.required_jobs.join(", "),
      required: p.required_labels.join(", "),
      blocked: p.blocked_labels.join(", "),
      windows: p.freeze_windows.map((w) => `${w.start} - ${w.end}`).join("\n"),
    });
    setMsg(null);
  };
  const save = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      const saved = await putMergePolicy({
        ...form,
        required_jobs: list(text.jobs),
        required_labels: list(text.required),
        blocked_labels: list(text.blocked),
        freeze_windows: parseWindows(text.windows),
      });
      setMsg({ ok: true, text: `Saved the rule for ${saved.repo || "every repo"} → ${saved.branch_pattern}. Open changes are re-evaluated now.` });
      setForm(BLANK);
      setText({ jobs: "", required: "", blocked: "", windows: "" });
      reload();
    } catch (err) {
      fail(err);
    }
  };
  const remove = async (p: MergePolicy) => {
    if (!p.id || !window.confirm(`Delete the merge rule for ${p.repo || "every repo"} → ${p.branch_pattern}?`)) return;
    try {
      await deleteMergePolicy(p.id);
      reload();
    } catch (err) {
      fail(err);
    }
  };
  const link = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await putHostIdentity(ident);
      setIdent({ ...ident, login: "", email: "" });
      reload();
    } catch (err) {
      fail(err);
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Merge rules</h1>
      </div>
      <p className="page-lede">
        Forge's rules between a change and its merge. Each open change matching a rule gets a merge gate, published to its
        host as the <code>forge/merge-gate</code> check: require that check in the host's branch protection and nobody can
        merge there until the rules pass. A repo's own rule beats a rule for every repo; an exact branch beats a pattern.
      </p>

      {rules && rules.length > 0 && (
        <div className="card table-card" style={{ marginBottom: 16 }}>
          <table className="dense-table">
            <thead>
              <tr>
                <th>Repo</th>
                <th>Target branch</th>
                <th>Rules</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rules.map((p) => (
                <tr key={p.id}>
                  <td>{p.repo || <span className="muted">every repo</span>}</td>
                  <td className="mono">{p.branch_pattern}</td>
                  <td className="muted">{describe(p)}</td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    <button className="btn" onClick={() => edit(p)}>
                      Edit
                    </button>{" "}
                    <button className="btn btn-reject" onClick={() => void remove(p)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <form className="card settings-form" onSubmit={(e) => void save(e)}>
        <h2>{form.id ? "Edit rule" : "New rule"}</h2>
        <label className="field">
          <span>Repo (empty = every repo)</span>
          <input value={form.repo} onChange={(e) => set("repo", e.target.value)} placeholder="Letztrip/cos" />
        </label>
        <label className="field">
          <span>Target branch (glob)</span>
          <input value={form.branch_pattern} onChange={(e) => set("branch_pattern", e.target.value)} placeholder="main, release/*" />
        </label>
        <label className="field">
          <span>Approvals required</span>
          <input type="number" min={0} value={form.required_approvals} onChange={(e) => set("required_approvals", Number(e.target.value))} />
        </label>
        <div className="field">
          <span>Who may approve (repo roles)</span>
          <div>
            {ROLES.map((r) => (
              <label key={r} style={{ marginRight: 12 }}>
                <input
                  type="checkbox"
                  checked={form.approver_roles.includes(r)}
                  onChange={(e) =>
                    set("approver_roles", e.target.checked ? [...form.approver_roles, r] : form.approver_roles.filter((x) => x !== r))
                  }
                />{" "}
                {r}
              </label>
            ))}
          </div>
        </div>
        <label className="field">
          <span>…of which at least</span>
          <input type="number" min={0} value={form.required_group_approvals} onChange={(e) => set("required_group_approvals", Number(e.target.value))} />
        </label>
        <label className="field">
          <span>from approver group</span>
          <select value={form.required_group} onChange={(e) => set("required_group", e.target.value)}>
            <option value="">(none)</option>
            {groups.map((g) => (
              <option key={g.name} value={g.name}>
                {g.name}
              </option>
            ))}
          </select>
        </label>
        {(
          [
            ["require_ci", "The Forge pipeline on the head commit must pass"],
            ["dismiss_on_push", "A push needs fresh approvals"],
            ["block_on_changes_requested", "\"Changes requested\" blocks the merge"],
            ["allow_self_approval", "Authors may approve their own changes"],
            ["require_code_owners", "Code owners (CODEOWNERS on the target) of every touched file must approve"],
            ["merge_queue", "Merge through the merge queue (each change is tested on top of the latest target)"],
          ] as const
        ).map(([k, label]) => (
          <label key={k} className="field-inline">
            <input type="checkbox" checked={form[k]} onChange={(e) => set(k, e.target.checked)} /> {label}
          </label>
        ))}
        <label className="field">
          <span>Required jobs (must pass on the head)</span>
          <input value={text.jobs} onChange={(e) => setText({ ...text, jobs: e.target.value })} placeholder="ci-build, ci-test" />
        </label>
        <label className="field">
          <span>Required labels</span>
          <input value={text.required} onChange={(e) => setText({ ...text, required: e.target.value })} placeholder="qa-ok" />
        </label>
        <label className="field">
          <span>Blocking labels</span>
          <input value={text.blocked} onChange={(e) => setText({ ...text, blocked: e.target.value })} placeholder="do-not-merge" />
        </label>
        <label className="field">
          <span>Linked ticket (regular expression, matched in title, description or branch)</span>
          <input value={form.ticket_pattern} onChange={(e) => set("ticket_pattern", e.target.value)} placeholder="TRV-[0-9]+" />
        </label>
        <label className="field">
          <span>Freeze windows, one per line</span>
          <textarea rows={2} value={text.windows} onChange={(e) => setText({ ...text, windows: e.target.value })} placeholder="fri 18:00 - mon 09:00" />
        </label>
        <label className="field">
          <span>Timezone for the windows</span>
          <input value={form.timezone} onChange={(e) => set("timezone", e.target.value)} placeholder="Asia/Kolkata" />
        </label>
        <div className="form-actions">
          <button className="btn btn-primary" type="submit">
            Save rule
          </button>
          {form.id && (
            <button className="btn" type="button" onClick={() => edit(BLANK)}>
              New rule instead
            </button>
          )}
        </div>
        {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      </form>

      <div className="card settings-form" style={{ marginTop: 16 }}>
        <h2>Host logins</h2>
        <p className="muted">
          Who a reviewer on the host is in Forge. A review given on the host counts towards a gate only when its login is
          linked here (or the repo has no members yet), and links make "no self-approval" and approver groups work for
          host reviews.
        </p>
        {ids.length > 0 && (
          <table className="dense-table">
            <tbody>
              {ids.map((h) => (
                <tr key={h.provider + h.login}>
                  <td className="muted">{h.provider}</td>
                  <td className="mono">{h.login}</td>
                  <td>{h.email}</td>
                  <td style={{ textAlign: "right" }}>
                    <button
                      className="btn"
                      onClick={() => void deleteHostIdentity(h.provider, h.login).then(reload).catch(fail)}
                    >
                      Unlink
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <form className="form-actions" onSubmit={(e) => void link(e)}>
          <select value={ident.provider} onChange={(e) => setIdent({ ...ident, provider: e.target.value })}>
            {["github", "gitlab", "bitbucket", "gitea", "codecommit", "git"].map((p) => (
              <option key={p}>{p}</option>
            ))}
          </select>
          <input value={ident.login} onChange={(e) => setIdent({ ...ident, login: e.target.value })} placeholder="host login" />
          <input value={ident.email} onChange={(e) => setIdent({ ...ident, email: e.target.value })} placeholder="forge user email" />
          <button className="btn" type="submit">
            Link
          </button>
        </form>
      </div>
    </div>
  );
}
