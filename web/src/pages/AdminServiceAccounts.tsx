import Menu from "../components/ui/Menu";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  createServiceAccount,
  createServiceToken,
  deleteServiceAccount,
  getIsAdmin,
  getTokenPolicy,
  listServiceAccounts,
  listServiceTokens,
  relativeTime,
  revokeApiToken,
  updateServiceAccount,
  type ApiToken,
  type ServiceAccount,
  type TokenPolicy,
} from "../api";

function fmtDate(iso: string | null): string {
  return iso ? new Date(iso).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }) : "never";
}

/** Tokens of one service account, with a one-time reveal on create. */
function AccountTokens({ account, policy }: { account: ServiceAccount; policy: TokenPolicy | null }) {
  const [tokens, setTokens] = useState<ApiToken[] | null>(null);
  const [desc, setDesc] = useState("");
  const [lifetime, setLifetime] = useState("365");
  const [revealed, setRevealed] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const reload = useCallback(() => {
    void listServiceTokens(account.id).then(setTokens);
  }, [account.id]);
  useEffect(() => reload(), [reload]);
  const allowNever = (policy?.allow_no_expiry ?? true) && (policy?.max_lifetime_days ?? 0) === 0;

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      const t = await createServiceToken(
        account.id,
        lifetime === "never" ? { description: desc, no_expiry: true } : { description: desc, expires_in_days: Number(lifetime) }
      );
      setRevealed(t.token ?? "");
      setDesc("");
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="sa-tokens">
      {revealed && (
        <div className="card runner-token-reveal">
          <div className="runner-token-reveal-head">
            <strong>Token for {account.identity}</strong>
            <span className="badge badge-blocked">shown once — copy it now</span>
          </div>
          <div className="runner-token-value">
            <code>{revealed}</code>
            <button type="button" className="btn" onClick={() => void navigator.clipboard?.writeText(revealed)}>
              Copy
            </button>
          </div>
          <button type="button" className="btn" onClick={() => setRevealed(null)}>
            Done
          </button>
        </div>
      )}
      {tokens && tokens.length > 0 && (
        <table className="dense-table">
          <thead>
            <tr>
              <th>Description</th>
              <th>Token</th>
              <th>Created</th>
              <th>Last used</th>
              <th>Expires</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((t) => (
              <tr key={t.id}>
                <td>{t.description || <span className="muted">(none)</span>}</td>
                <td className="mono">fci_…{t.token_suffix}</td>
                <td className="muted">{fmtDate(t.created_at)}</td>
                <td className="muted">{t.last_used_at ? relativeTime(t.last_used_at) : "never"}</td>
                <td className="muted">{fmtDate(t.expires_at)}</td>
                <td>
                  <span className={`badge ${t.status === "active" ? "badge-success" : "badge-canceled"}`}>{t.status}</span>
                </td>
                <td>
                  {!t.revoked && (
                    <button
                      type="button"
                      className="btn btn-danger-quiet"
                      onClick={() => {
                        if (window.confirm(`Revoke fci_…${t.token_suffix}?`)) void revokeApiToken(t.id).then(reload);
                      }}
                    >
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {!account.disabled && (
        <form className="inline-form" onSubmit={(e) => void create(e)}>
          <label className="field">
            <span>New token</span>
            <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="what uses it, e.g. github-actions deploy" />
          </label>
          <label className="field">
            <span>Expires</span>
            <select value={lifetime} onChange={(e) => setLifetime(e.target.value)}>
              <option value="30">30 days</option>
              <option value="90">90 days</option>
              <option value="365">1 year</option>
              {allowNever && <option value="never">Never</option>}
            </select>
          </label>
          <button type="submit" className="btn">
            Generate token
          </button>
        </form>
      )}
      {error && <div className="error-banner">{error}</div>}
    </div>
  );
}

/**
 * Admin: service accounts — automation identities owned by the org, so CI
 * scripts don't depend on one person's token. Each acts as svc:<name>.
 */
export default function AdminServiceAccounts() {
  const [isAdmin, setIsAdmin] = useState<boolean | null>(null);
  const [accounts, setAccounts] = useState<ServiceAccount[] | null>(null);
  const [policy, setPolicy] = useState<TokenPolicy | null>(null);
  const [open, setOpen] = useState<number | null>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [admin, setAdmin] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(() => {
    void listServiceAccounts()
      .then(setAccounts)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    void getTokenPolicy().then(setPolicy).catch(() => setPolicy(null));
    reload();
  }, [reload]);

  if (isAdmin === false) return <div className="error-banner">Service accounts are managed by admins.</div>;

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      const a = await createServiceAccount({ name: name.trim(), description: description.trim(), is_admin: admin });
      setName("");
      setDescription("");
      setAdmin(false);
      setOpen(a.id);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };
  const patch = async (a: ServiceAccount, b: { is_admin?: boolean; disabled?: boolean }) => {
    setError(null);
    try {
      await updateServiceAccount(a.id, b);
      reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };
  const remove = async (a: ServiceAccount) => {
    if (!window.confirm(`Delete ${a.identity} and all its tokens?`)) return;
    await deleteServiceAccount(a.id);
    reload();
  };

  return (
    <div>
      <div className="page-head">
        <h1>Service accounts</h1>
      </div>
      <p className="page-lede">
        For automation that shouldn't depend on one person: each account acts as{" "}
        <code>svc:&lt;name&gt;</code>. Give it repo access by adding that identity as a repo member
        (Repo → Settings → Members), or tick admin for platform-wide rights. Its tokens follow the
        API token policy; disabling the account stops every token at once.
      </p>
      {error && <div className="error-banner">{error}</div>}

      {accounts && accounts.length === 0 && <div className="empty card">No service accounts yet.</div>}
      {accounts?.map((a) => (
        <div key={a.id} className="card sa-card">
          <div className="sa-head">
            <div>
              <span className="mono sa-identity">{a.identity}</span>
              {a.is_admin && <span className="badge badge-blocked">admin</span>}{" "}
              {a.disabled && <span className="badge badge-canceled">disabled</span>}
              <div className="muted">
                {a.description || "No description"} · {a.active_tokens} active token{a.active_tokens === 1 ? "" : "s"} · created by{" "}
                {a.created_by} {relativeTime(a.created_at)}
              </div>
            </div>
            <div className="page-head-actions">
              <button type="button" className="btn" onClick={() => setOpen(open === a.id ? null : a.id)}>
                {open === a.id ? "Hide tokens" : `Tokens (${a.active_tokens})`}
              </button>
              <Menu
                label={`Actions for ${a.identity}`}
                items={[
                  { label: a.is_admin ? "Remove admin" : "Make admin", icon: "shield", onSelect: () => void patch(a, { is_admin: !a.is_admin }) },
                  { label: a.disabled ? "Enable" : "Disable", icon: a.disabled ? "play" : "pause", onSelect: () => void patch(a, { disabled: !a.disabled }) },
                  { label: "Delete", icon: "close", danger: true, onSelect: () => void remove(a) },
                ]}
              />
            </div>
          </div>
          {open === a.id && <AccountTokens account={a} policy={policy} />}
        </div>
      ))}

      <form className="card settings-form" onSubmit={(e) => void create(e)}>
        <h2>New service account</h2>
        <div className="form-row">
          <label className="field">
            <span>Name</span>
            <input className="mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="deployer" required />
          </label>
          <label className="field">
            <span>Description</span>
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="GitHub Actions → Forge deploy trigger" />
          </label>
        </div>
        <label className="check-row">
          <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
          Platform admin (only if the automation manages Forge itself)
        </label>
        <div className="card-foot">
          <button type="submit" className="btn btn-primary">
            Create account
          </button>
        </div>
      </form>
    </div>
  );
}
