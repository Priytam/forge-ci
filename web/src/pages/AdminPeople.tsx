import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  addPlatformAdmin,
  getIsAdmin,
  listPeople,
  listPlatformAdmins,
  relativeTime,
  setPlatformRole,
  type PeoplePage,
  type Person,
  type PlatformAdmins,
} from "../api";

const PAGE = 50;
const MAX_REPO_CHIPS = 3;

/** Why a role can't be changed here, or null when it can. */
function lockedReason(p: Person, me: string): string | null {
  if (p.admin_source === "env") return "Set in the server's ADMIN_EMAILS — change it in the server config";
  if (p.admin_source === "group")
    return `Admin through the IdP group ${p.admin_groups.join(", ")} — change it in Sign-in & identity`;
  if (p.admin_source === "ui" && p.email === me) return "You can't remove your own admin role; ask another admin";
  return null;
}

function RepoRoles({ roles }: { roles: Person["repo_roles"] }) {
  if (roles.length === 0) return <span className="muted">—</span>;
  const shown = roles.slice(0, MAX_REPO_CHIPS);
  const rest = roles.length - shown.length;
  return (
    <span className="people-repos" title={roles.map((r) => `${r.role} · ${r.repo}`).join("\n")}>
      {shown.map((r) => (
        <span key={r.repo} className="chip">
          {r.role} · {r.repo}
        </span>
      ))}
      {rest > 0 && <span className="chip chip-more">+{rest}</span>}
    </span>
  );
}

/**
 * Admin → People & roles: everyone who has signed in, with their platform
 * role and repo roles. Search by name or email and switch a person between
 * Admin and Member. Admins from ADMIN_EMAILS or an IdP group are shown but
 * locked; repo roles are changed on each repo's Members settings.
 */
export default function AdminPeople() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const adminsOnly = params.get("role") === "admin";
  const page = Math.max(0, Number(params.get("page") ?? 0) || 0);

  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [meta, setMeta] = useState<PlatformAdmins | null>(null);
  const [data, setData] = useState<PeoplePage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [draft, setDraft] = useState(q);
  const [adding, setAdding] = useState(false);
  const [newEmail, setNewEmail] = useState("");
  const searchRef = useRef<HTMLInputElement>(null);

  // Any change other than paging goes back to page 1.
  const setParam = (key: string, value: string, replace = false) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== "page") next.delete("page");
    setParams(next, { replace });
  };

  const reload = useCallback(() => {
    void listPeople(q, adminsOnly, page * PAGE, PAGE)
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [q, adminsOnly, page]);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    void listPlatformAdmins()
      .then(setMeta)
      .catch(() => setMeta(null));
  }, []);
  useEffect(() => reload(), [reload]);

  // The box updates as you type; the URL (and the request) follow a beat later.
  useEffect(() => setDraft(q), [q]);
  useEffect(() => {
    if (draft.trim() === q) return;
    const t = window.setTimeout(() => setParam("q", draft.trim(), true), 250);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft]);

  // "/" jumps to the search box.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement;
      if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
      if (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName)) return;
      e.preventDefault();
      searchRef.current?.focus();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (isAdmin === false) return <div className="error-banner">People and roles are managed by admins.</div>;

  const me = meta?.me ?? "";
  const display = (p: Person) => p.name || p.email;

  const changeRole = async (p: Person, role: "admin" | "member") => {
    const verb =
      role === "admin"
        ? `Make ${display(p)} a platform admin? They can change settings, secrets, runners and members on every repo.`
        : `Remove platform admin from ${display(p)}? It takes effect on their next request.`;
    if (!window.confirm(verb)) return;
    setError(null);
    setNotice(null);
    setBusy(p.email);
    try {
      await setPlatformRole(p.email, role);
      setNotice(role === "admin" ? `${display(p)} is now a platform admin.` : `${display(p)} is now a member.`);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  };

  const addByEmail = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setNotice(null);
    try {
      const a = await addPlatformAdmin(newEmail.trim());
      setNotice(`${a.email} is now a platform admin; they'll have it from their first sign-in.`);
      setNewEmail("");
      setAdding(false);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const total = data?.total ?? 0;
  const filtered = q !== "" || adminsOnly;

  return (
    <div>
      <div className="page-head">
        <h1>People &amp; roles</h1>
        <div className="page-head-actions">
          <button type="button" className="btn" onClick={() => setAdding((v) => !v)}>
            {adding ? "Cancel" : "Add admin by email"}
          </button>
        </div>
      </div>
      <p className="page-lede">
        Everyone who has signed in. <strong>Admins</strong> manage settings, secrets, runners and members on every
        repo; <strong>members</strong> get access through their repo roles, which you change on each repo's{" "}
        <em>Settings → Members</em>. Role changes apply on the person's next request.
      </p>

      {meta && !meta.sso_enforced && (
        <div className="doc-note doc-note-warn">
          <div className="doc-note-title">Sign-in is off</div>
          Until a sign-in provider is enabled, everyone is treated as an admin. Set up your admins first, then turn
          on sign-in under <Link to="/admin/sso">Sign-in &amp; identity</Link>.
        </div>
      )}

      {adding && (
        <form className="card settings-form" onSubmit={(e) => void addByEmail(e)}>
          <h2>Add an admin who hasn't signed in yet</h2>
          <div className="inline-form">
            <label className="field admin-email-field">
              <span>Email</span>
              <input
                type="email"
                value={newEmail}
                onChange={(e) => setNewEmail(e.target.value)}
                placeholder="alice@acme.io"
                autoComplete="off"
                autoFocus
                required
              />
            </label>
            <button type="submit" className="btn btn-primary" disabled={!newEmail.trim()}>
              Add admin
            </button>
          </div>
        </form>
      )}

      <div className="filter-bar people-toolbar">
        <input
          ref={searchRef}
          type="search"
          className="people-search"
          placeholder="Search by name or email…  ( / )"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              setDraft("");
              e.currentTarget.blur();
            }
          }}
          aria-label="Search people"
        />
        <select
          value={adminsOnly ? "admin" : ""}
          onChange={(e) => setParam("role", e.target.value)}
          aria-label="Filter by role"
        >
          <option value="">All people</option>
          <option value="admin">Admins only</option>
        </select>
        {data && <span className="muted people-count">{total === 1 ? "1 person" : `${total} people`}</span>}
      </div>

      {error && <div className="error-banner">{error}</div>}
      {notice && <div className="saved-note">{notice}</div>}
      {!data && !error && <div className="muted">Loading people…</div>}

      {data && data.items.length === 0 && (
        <div className="empty card">
          {filtered ? (
            <>
              No one matches.{" "}
              <button type="button" className="btn" onClick={() => setParams(new URLSearchParams())}>
                Show everyone
              </button>
            </>
          ) : (
            "No one has signed in yet."
          )}
        </div>
      )}

      {data && data.items.length > 0 && (
        <div className="card people-card">
          <table className="dense-table people-table">
            <thead>
              <tr>
                <th>Person</th>
                <th>Platform role</th>
                <th>Repo roles</th>
                <th>Last sign-in</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((p) => {
                const isAdminRow = p.admin_source !== "";
                const locked = lockedReason(p, me);
                return (
                  <tr key={p.email}>
                    <td>
                      <div className="people-name">
                        {p.name || p.email}
                        {p.email === me && <span className="muted"> (you)</span>}
                      </div>
                      {p.name && <div className="muted people-email">{p.email}</div>}
                    </td>
                    <td>
                      <select
                        className="people-role"
                        value={isAdminRow ? "admin" : "member"}
                        disabled={locked !== null || busy === p.email}
                        title={locked ?? undefined}
                        aria-label={`Platform role for ${p.email}`}
                        onChange={(e) => void changeRole(p, e.target.value as "admin" | "member")}
                      >
                        <option value="member">Member</option>
                        <option value="admin">Admin</option>
                      </select>
                      {p.admin_source === "env" && <span className="badge badge-blocked people-src">server config</span>}
                      {p.admin_source === "group" && (
                        <span className="badge badge-blocked people-src" title={p.admin_groups.join(", ")}>
                          via group
                        </span>
                      )}
                    </td>
                    <td>
                      <RepoRoles roles={p.repo_roles} />
                    </td>
                    <td className="muted">
                      {p.last_sign_in_at ? relativeTime(p.last_sign_in_at) : "never"}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {data && total > PAGE && (
        <div className="filter-pager">
          <span className="muted">
            {page * PAGE + 1}–{page * PAGE + data.items.length} of {total}
          </span>
          <button type="button" className="btn" disabled={page === 0} onClick={() => setParam("page", String(page - 1))}>
            ← Previous
          </button>
          <button type="button" className="btn" disabled={!data.hasMore} onClick={() => setParam("page", String(page + 1))}>
            Next →
          </button>
        </div>
      )}

      {meta && meta.admin_groups.length > 0 && (
        <p className="muted">
          Members of{" "}
          {meta.admin_groups.map((g, i) => (
            <span key={`${g.provider}/${g.group}`}>
              {i > 0 && ", "}
              <code>{g.group}</code>
            </span>
          ))}{" "}
          are admins through <Link to="/admin/sso">group mappings</Link>.
        </p>
      )}
    </div>
  );
}
