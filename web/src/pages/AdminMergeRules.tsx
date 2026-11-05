import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  deleteHostIdentity,
  getIsAdmin,
  listHostIdentities,
  listMergePolicies,
  putHostIdentity,
  putMergePolicy,
  type HostIdentity,
  type MergePolicy,
} from "../api";
import Icon from "../components/Icon";
import RuleCard from "../components/review/RuleCard";
import RuleEditor, { RULE_SWITCHES } from "../components/review/RuleEditor";
import { Skeleton } from "../components/ui/Pill";
import { useToast } from "../components/ui/Toast";

/** A rule in one line, for lists that have no room for its card. */
export function describe(p: MergePolicy): string {
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

const PROVIDERS = ["github", "gitlab", "bitbucket", "gitea", "codecommit", "git"];

/**
 * Admin: the rules between a change and its merge, per repo and target
 * branch, and which host login is which Forge user. A change no rule matches
 * has no gate (nothing is published to its host).
 */
export default function AdminMergeRules() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [rules, setRules] = useState<MergePolicy[] | null>(null);
  const [ids, setIds] = useState<HostIdentity[]>([]);
  const [editing, setEditing] = useState<MergePolicy | "new" | null>(null);
  const [ident, setIdent] = useState<HostIdentity>({ provider: "github", login: "", email: "" });
  const toast = useToast();
  const fail = useCallback((e: unknown) => toast(e instanceof Error ? e.message : String(e), "danger"), [toast]);

  const reload = useCallback(() => {
    void listMergePolicies().then(setRules).catch(fail);
    void listHostIdentities()
      .then(setIds)
      .catch(() => undefined);
  }, [fail]);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);
  if (isAdmin === false) return <div className="error-banner">Merge rules are managed by admins.</div>;

  const flip = async (p: MergePolicy, key: keyof MergePolicy, on: boolean) => {
    try {
      await putMergePolicy({ ...p, [key]: on });
      const what = RULE_SWITCHES.find((s) => s.key === key)?.label.toLowerCase();
      toast(`${p.repo || "Every repo"} → ${p.branch_pattern}: ${what} ${on ? "on" : "off"}.`, "success");
      reload();
    } catch (e) {
      fail(e);
    }
  };
  const link = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await putHostIdentity(ident);
      toast(`${ident.login} on ${ident.provider} is now ${ident.email}.`, "success");
      setIdent({ ...ident, login: "", email: "" });
      reload();
    } catch (err) {
      fail(err);
    }
  };

  return (
    <div className="cr-page">
      <div className="page-head">
        <h1>Merge rules</h1>
        {rules !== null && rules.length > 0 && (
          <div className="page-head-actions">
            <button className="btn btn-primary" onClick={() => setEditing("new")}>
              <Icon name="plus" size={14} /> New rule
            </button>
          </div>
        )}
      </div>
      <p className="page-lede">
        Forge's rules between a change and its merge. Each open change matching a rule gets a merge gate, published to
        its host as the <code>forge/merge-gate</code> check: require that check in the host's branch protection and
        nobody can merge there until the rules pass. A repo's own rule beats a rule for every repo; an exact branch
        beats a pattern.
      </p>

      {rules === null && <Skeleton h={140} />}
      {rules !== null && rules.length === 0 && (
        <div className="card cr-empty">
          <Icon name="shield" size={24} />
          <div className="cr-empty-title">No merge rules yet</div>
          <div>
            Changes merge as their hosts allow. A rule adds required approvals, CI, code owners or the merge queue.
          </div>
          <button className="btn btn-primary" onClick={() => setEditing("new")}>
            <Icon name="plus" size={14} /> New rule
          </button>
        </div>
      )}
      {rules !== null && rules.length > 0 && (
        <div className="cr-rules" style={{ marginBottom: 24 }}>
          {rules.map((p) => (
            <RuleCard
              key={p.id}
              rule={p}
              scope={p.repo || "every repo"}
              canEdit
              onEdit={() => setEditing(p)}
              onFlip={(k, v) => void flip(p, k, v)}
            />
          ))}
        </div>
      )}

      <section className="cr-card">
        <div className="cr-card-head">
          <b>Host logins</b> · who a reviewer on the host is in Forge
        </div>
        <div style={{ padding: 12 }}>
          <p className="muted" style={{ marginTop: 0 }}>
            A review given on the host counts towards a gate only when its login is linked here (or the repo has no
            members yet). Links also make "no self-approval", approver groups and code owners work for host reviews.
            People who link their own account (Linked accounts) appear here by themselves.
          </p>
          {ids.length > 0 && (
            <table className="dense-table" style={{ marginBottom: 12 }}>
              <tbody>
                {ids.map((h) => (
                  <tr key={h.provider + h.login}>
                    <td className="muted" style={{ width: 110 }}>
                      {h.provider}
                    </td>
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
            <select
              value={ident.provider}
              onChange={(e) => setIdent({ ...ident, provider: e.target.value })}
              aria-label="Host"
            >
              {PROVIDERS.map((p) => (
                <option key={p}>{p}</option>
              ))}
            </select>
            <input
              value={ident.login}
              onChange={(e) => setIdent({ ...ident, login: e.target.value })}
              placeholder="octocat"
              aria-label="Host login"
            />
            <input
              value={ident.email}
              onChange={(e) => setIdent({ ...ident, email: e.target.value })}
              placeholder="name@company.com"
              aria-label="Forge user"
            />
            <button className="btn" type="submit" disabled={!ident.login.trim() || !ident.email.trim()}>
              Link login
            </button>
          </form>
        </div>
      </section>

      {editing && (
        <RuleEditor
          rule={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(m) => {
            setEditing(null);
            toast(m, "success");
            reload();
          }}
        />
      )}
    </div>
  );
}
