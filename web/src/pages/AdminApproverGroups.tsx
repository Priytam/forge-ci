import { useCallback, useEffect, useState, type FormEvent } from "react";
import { deleteApproverGroup, getIsAdmin, listApproverGroups, putApproverGroup, type ApproverGroup } from "../api";

/**
 * Admin: named groups of people an approval rule can require — "2 approvals,
 * at least 1 from release-managers". Members are emails; they may approve
 * environments whose rule names their group even without the repo role.
 */
export default function AdminApproverGroups() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [groups, setGroups] = useState<ApproverGroup[] | null>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [members, setMembers] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const reload = useCallback(() => {
    void listApproverGroups()
      .then(setGroups)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);

  if (isAdmin === false) return <div className="error-banner">Approver groups are managed by admins.</div>;

  const edit = (g: ApproverGroup) => {
    setName(g.name);
    setDescription(g.description);
    setMembers(g.members.join("\n"));
    setMsg(null);
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      await putApproverGroup(name.trim(), {
        description: description.trim(),
        members: members.split(/[\s,]+/).filter(Boolean),
      });
      setMsg({ ok: true, text: `Saved ${name.trim().toLowerCase()}.` });
      setName("");
      setDescription("");
      setMembers("");
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  const remove = async (g: ApproverGroup) => {
    if (!window.confirm(`Delete approver group ${g.name}?`)) return;
    try {
      await deleteApproverGroup(g.name);
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Approver groups</h1>
      </div>
      <p className="page-lede">
        Name a group here, then require it on an environment's approval rule (Repo → Settings →
        Members &amp; approval rules): for example 2 approvals, at least 1 from{" "}
        <code>release-managers</code>. Group members can approve that environment even without a
        repo role, and they're included in approval notifications.
      </p>

      {groups && groups.length > 0 && (
        <div className="card table-card" style={{ marginBottom: 16 }}>
          <table className="dense-table">
            <thead>
              <tr>
                <th>Group</th>
                <th>Members</th>
                <th>Description</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => (
                <tr key={g.name}>
                  <td className="mono">{g.name}</td>
                  <td>{g.members.join(", ") || <span className="muted">(empty)</span>}</td>
                  <td className="muted">{g.description}</td>
                  <td style={{ textAlign: "right" }}>
                    <button type="button" className="btn btn-icon" onClick={() => edit(g)}>
                      Edit
                    </button>{" "}
                    <button type="button" className="btn btn-danger-quiet" onClick={() => void remove(g)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {groups && groups.length === 0 && <div className="empty card">No approver groups yet.</div>}

      <form className="card settings-form" onSubmit={(e) => void save(e)}>
        <h2>{groups?.some((g) => g.name === name.trim().toLowerCase()) ? "Edit group" : "New group"}</h2>
        <div className="form-row">
          <label className="field">
            <span>Name</span>
            <input className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="release-managers" required />
          </label>
          <label className="field">
            <span>Description</span>
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Who signs off production" />
          </label>
        </div>
        <label className="field">
          <span>Members (emails, one per line or comma-separated)</span>
          <textarea rows={4} value={members} onChange={(e) => setMembers(e.target.value)} placeholder={"harish@company.com\nashwin@company.com"} />
        </label>
        <div className="form-actions">
          <button type="submit" className="btn btn-primary">
            Save group
          </button>
        </div>
        {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
      </form>
    </div>
  );
}
