import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  addPlatformAdmin,
  getIsAdmin,
  listPlatformAdmins,
  relativeTime,
  removePlatformAdmin,
  type PlatformAdmin,
  type PlatformAdmins,
} from "../api";

/**
 * Admin: who is a platform admin. Admins come from ADMIN_EMAILS (server
 * config, read-only here), from grants made on this page, and from IdP groups
 * mapped to platform admin on Sign-in & identity.
 */
export default function AdminPlatformAdmins() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [data, setData] = useState<PlatformAdmins | null>(null);
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const reload = useCallback(() => {
    void listPlatformAdmins()
      .then(setData)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    reload();
  }, [reload]);

  if (isAdmin === false) return <div className="error-banner">Platform admins are managed by admins.</div>;

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    setBusy(true);
    try {
      const a = await addPlatformAdmin(email.trim());
      setEmail("");
      setNotice(`${a.email} is now a platform admin.`);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  const remove = async (a: PlatformAdmin) => {
    if (!window.confirm(`Remove platform admin from ${a.email}? It takes effect on their next request.`)) return;
    setError(null);
    setNotice(null);
    try {
      await removePlatformAdmin(a.email);
      setNotice(`${a.email} is no longer a platform admin.`);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>Platform admins</h1>
      </div>
      <p className="page-lede">
        Platform admins manage variables and secrets, runners, members, approval rules, sign-in and
        tokens on every repo. Add people here by email; the change applies on their next request.
        People listed in the server's <code>ADMIN_EMAILS</code> are always admins and can only be
        changed in the server config.
      </p>

      {data && !data.sso_enforced && (
        <div className="doc-note doc-note-warn">
          <div className="doc-note-title">Sign-in is off</div>
          Until a sign-in provider is enabled, everyone is treated as an admin. Add your admins here
          first, then turn on sign-in under <Link to="/admin/sso">Sign-in &amp; identity</Link>.
        </div>
      )}

      {error && <div className="error-banner">{error}</div>}
      {notice && <div className="saved-note">{notice}</div>}

      <form className="card settings-form" onSubmit={(e) => void add(e)}>
        <h2>Add an admin</h2>
        <div className="inline-form">
          <label className="field admin-email-field">
            <span>Email</span>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="alice@acme.io"
              autoComplete="off"
              required
            />
          </label>
          <button type="submit" className="btn btn-primary" disabled={busy || !email.trim()}>
            Add admin
          </button>
        </div>
      </form>

      {data && (
        <div className="card">
          <table className="dense-table">
            <thead>
              <tr>
                <th>Email</th>
                <th>Source</th>
                <th>Added by</th>
                <th>Added</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {data.admins.length === 0 && (
                <tr>
                  <td colSpan={5} className="muted">
                    No platform admins yet.
                  </td>
                </tr>
              )}
              {data.admins.map((a) => (
                <tr key={a.email}>
                  <td>
                    {a.email}
                    {a.email === data.me && <span className="muted"> (you)</span>}
                  </td>
                  <td>
                    {a.source === "env" ? (
                      <span className="badge badge-blocked" title="Set in the server's ADMIN_EMAILS">
                        server config
                      </span>
                    ) : (
                      <span className="badge badge-success">added here</span>
                    )}
                  </td>
                  <td className="muted">{a.added_by || "—"}</td>
                  <td className="muted">{a.created_at ? relativeTime(a.created_at) : "—"}</td>
                  <td>
                    {a.source === "ui" && a.email !== data.me && (
                      <button type="button" className="btn btn-danger-quiet" onClick={() => void remove(a)}>
                        Remove
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {data && data.admin_groups.length > 0 && (
        <p className="muted">
          Members of{" "}
          {data.admin_groups.map((g, i) => (
            <span key={`${g.provider}/${g.group}`}>
              {i > 0 && ", "}
              <code>{g.group}</code>
            </span>
          ))}{" "}
          are also admins through <Link to="/admin/sso">group mappings</Link>.
        </p>
      )}
    </div>
  );
}
