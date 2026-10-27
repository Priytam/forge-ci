import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  createGroupMapping,
  deleteGroupMapping,
  getSessionSettings,
  getSsoConfig,
  listGroupMappings,
  putSessionSettings,
  putSsoProvider,
  type GroupMapping,
  type SessionSettings,
  type SsoProviderConfig,
} from "../api";
import { usePoll } from "../hooks/usePoll";

interface SettingField {
  key: string;
  label: string;
  hint?: string;
  placeholder?: string;
  checkbox?: boolean;
}

interface ProviderMeta {
  key: string;
  label: string;
  guide: string;
  hasTenant: boolean;
  hasAllowedDomain: boolean;
  allowedDomainHint?: string;
  /** false for LDAP: it signs in with a form, not a client app */
  hasClientId?: boolean;
  secretLabel?: string;
  /** no redirect URI to paste (LDAP) */
  noRedirect?: boolean;
  settings?: SettingField[];
  blurb?: string;
}

const PROVIDERS: ProviderMeta[] = [
  {
    key: "google",
    label: "Google",
    guide: "/docs/sso-google",
    hasTenant: false,
    hasAllowedDomain: true,
    allowedDomainHint:
      "e.g. meesho.com — hints Google's account picker and is enforced server-side",
  },
  {
    key: "microsoft",
    label: "Microsoft (Entra ID)",
    guide: "/docs/sso-microsoft",
    hasTenant: true,
    hasAllowedDomain: false,
  },
  {
    key: "github",
    label: "GitHub",
    guide: "/docs/sso-github",
    hasTenant: false,
    hasAllowedDomain: true,
    allowedDomainHint: "restricts sign-ins by email domain, enforced server-side",
  },
  {
    key: "oidc",
    label: "OpenID Connect (any identity provider)",
    guide: "/docs/sso-oidc",
    blurb: "Okta, Keycloak, Auth0, Entra ID, Authentik, Dex, Zitadel, Google Workspace… anything that speaks OIDC. Group claims map to Forge roles below.",
    hasTenant: false,
    hasAllowedDomain: true,
    allowedDomainHint: "optional: only emails in this domain may sign in",
    settings: [
      { key: "issuer", label: "Issuer URL", placeholder: "https://acme.okta.com/oauth2/default", hint: "Forge reads <issuer>/.well-known/openid-configuration" },
      { key: "display_name", label: "Button label", placeholder: "Okta" },
      { key: "scopes", label: "Scopes", placeholder: "openid email profile", hint: "add groups (or your IdP's group scope) to receive group claims" },
      { key: "groups_claim", label: "Groups claim", placeholder: "groups", hint: "dotted paths work, e.g. realm_access.roles for Keycloak" },
    ],
  },
  {
    key: "ldap",
    label: "LDAP / Active Directory",
    guide: "/docs/sso-ldap",
    blurb: "Users sign in with their directory username and password. Group membership (memberOf, or a group search) maps to Forge roles below.",
    hasTenant: false,
    hasAllowedDomain: true,
    allowedDomainHint: "optional: only emails in this domain may sign in",
    hasClientId: false,
    secretLabel: "Bind password",
    noRedirect: true,
    settings: [
      { key: "url", label: "Server URL", placeholder: "ldaps://ldap.acme.io:636" },
      { key: "start_tls", label: "Use StartTLS (for ldap:// URLs)", checkbox: true },
      { key: "bind_dn", label: "Bind DN (service account)", placeholder: "cn=forge,ou=services,dc=acme,dc=io" },
      { key: "base_dn", label: "User search base", placeholder: "ou=people,dc=acme,dc=io" },
      { key: "user_filter", label: "User filter", placeholder: "(|(uid={username})(sAMAccountName={username})(mail={username}))" },
      { key: "email_attribute", label: "Email attribute", placeholder: "mail" },
      { key: "name_attribute", label: "Name attribute", placeholder: "displayName" },
      { key: "group_attribute", label: "Group attribute", placeholder: "memberOf" },
      { key: "group_base_dn", label: "Group search base (optional)", placeholder: "ou=groups,dc=acme,dc=io", hint: "for directories without memberOf" },
    ],
  },
];

function ProviderCard({
  meta,
  config,
  redirectUri,
  onSaved,
}: {
  meta: ProviderMeta;
  config: SsoProviderConfig | null;
  redirectUri: string;
  onSaved: () => void;
}) {
  const [enabled, setEnabled] = useState(config?.enabled ?? false);
  const [clientId, setClientId] = useState(config?.client_id ?? "");
  const [clientSecret, setClientSecret] = useState("");
  const [tenant, setTenant] = useState(config?.tenant ?? "");
  const [allowedDomain, setAllowedDomain] = useState(
    config?.allowed_domain ?? ""
  );
  const [settings, setSettings] = useState<Record<string, string>>(config?.settings ?? {});
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  const hasSecret = config?.has_secret ?? false;

  const copyUri = async () => {
    try {
      await navigator.clipboard.writeText(redirectUri);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable
    }
  };

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      await putSsoProvider({
        provider: meta.key,
        enabled,
        client_id: clientId.trim(),
        ...(clientSecret ? { client_secret: clientSecret } : {}),
        ...(meta.hasTenant ? { tenant: tenant.trim() } : {}),
        ...(meta.hasAllowedDomain
          ? { allowed_domain: allowedDomain.trim() }
          : {}),
        ...(meta.settings ? { settings } : {}),
      });
      setSaved(true);
      setClientSecret("");
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="card sso-card" onSubmit={(e) => void onSubmit(e)}>
      <div className="sso-card-head">
        <h2>{meta.label}</h2>
        <Link to={meta.guide} className="guide-link">
          Setup guide →
        </Link>
      </div>

      {meta.blurb && <p className="muted">{meta.blurb}</p>}

      <label className="check-row">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
        />
        <span>Enabled</span>
      </label>

      {meta.settings?.map((f) =>
        f.checkbox ? (
          <label key={f.key} className="check-row">
            <input
              type="checkbox"
              checked={settings[f.key] === "true"}
              onChange={(e) => setSettings({ ...settings, [f.key]: e.target.checked ? "true" : "" })}
            />
            <span>{f.label}</span>
          </label>
        ) : (
          <label key={f.key} className="field">
            <span>{f.label}</span>
            <input
              className="mono"
              value={settings[f.key] ?? ""}
              placeholder={f.placeholder}
              onChange={(e) => setSettings({ ...settings, [f.key]: e.target.value })}
            />
            {f.hint && <span className="field-hint">{f.hint}</span>}
          </label>
        )
      )}

      <div className="form-row">
        {meta.hasClientId !== false && (
          <label className="field">
            <span>Client ID</span>
            <input
              className="mono"
              value={clientId}
              onChange={(e) => setClientId(e.target.value)}
            />
          </label>
        )}
        <label className="field">
          <span>{meta.secretLabel ?? "Client secret"}</span>
          <input
            type="password"
            autoComplete="off"
            placeholder={hasSecret ? "unchanged" : ""}
            value={clientSecret}
            onChange={(e) => setClientSecret(e.target.value)}
          />
          {hasSecret && (
            <span className="field-hint">
              A secret is stored — leave blank to keep it.
            </span>
          )}
        </label>
      </div>

      {meta.hasTenant && (
        <label className="field">
          <span>Tenant</span>
          <input
            className="mono"
            value={tenant}
            onChange={(e) => setTenant(e.target.value)}
          />
          <span className="field-hint">Directory (tenant) ID, or common</span>
        </label>
      )}

      {meta.hasAllowedDomain && (
        <label className="field">
          <span>Allowed domain</span>
          <input
            className="mono"
            value={allowedDomain}
            onChange={(e) => setAllowedDomain(e.target.value)}
          />
          {meta.allowedDomainHint && (
            <span className="field-hint">{meta.allowedDomainHint}</span>
          )}
        </label>
      )}

      {!meta.noRedirect && (
      <div className="field">
        <span>Redirect URI</span>
        <div className="redirect-row">
          <input className="mono" value={redirectUri} readOnly />
          <button type="button" className="btn" onClick={() => void copyUri()}>
            {copied ? "Copied ✓" : "Copy"}
          </button>
        </div>
        <span className="field-hint">Paste this into the provider console.</span>
      </div>
      )}

      {error && <div className="error-banner">{error}</div>}
      {saved && <div className="saved-note">Saved.</div>}

      <div className="form-actions">
        <button type="submit" className="btn btn-primary" disabled={busy}>
          Save {meta.label}
        </button>
      </div>
    </form>
  );
}

/** IdP group → Forge role. */
function GroupMappings() {
  const [items, setItems] = useState<GroupMapping[] | null>(null);
  const [form, setForm] = useState({ group: "", provider: "", kind: "repo", repo_pattern: "*", role: "developer" });
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const reload = useCallback(() => {
    void listGroupMappings()
      .then(setItems)
      .catch((e) => setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }));
  }, []);
  useEffect(reload, [reload]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      await createGroupMapping({
        group: form.group.trim(),
        provider: form.provider,
        platform_admin: form.kind === "admin",
        repo_pattern: form.kind === "admin" ? "" : form.repo_pattern.trim(),
        role: form.kind === "admin" ? "" : form.role,
      });
      setForm({ ...form, group: "" });
      setMsg({ ok: true, text: "Added. It applies from each member's next sign-in." });
      reload();
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };
  const remove = async (m: GroupMapping) => {
    if (!window.confirm(`Remove the mapping for ${m.group}? Its members lose that role immediately.`)) return;
    await deleteGroupMapping(m.id).catch((e) => setMsg({ ok: false, text: String(e) }));
    reload();
  };

  return (
    <div className="card sso-card">
      <div className="sso-card-head">
        <h2>Group → role mappings</h2>
        <Link to="/docs/sso-groups" className="guide-link">
          How groups work →
        </Link>
      </div>
      <p className="muted">
        Turn identity-provider groups into Forge roles, so access follows your directory: someone who
        leaves a group loses the role at their next sign-in. Groups come from the OIDC groups claim or
        LDAP membership. A role granted here counts like a repo membership for running jobs and approving
        deploys.
      </p>
      {items && items.length > 0 && (
        <div className="table-card" style={{ marginBottom: 12 }}>
          <table className="dense-table">
            <thead>
              <tr>
                <th>Group</th>
                <th>Grants</th>
                <th>Provider</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {items.map((m) => (
                <tr key={m.id}>
                  <td className="mono">{m.group}</td>
                  <td>
                    {m.platform_admin ? (
                      <span className="badge badge-blocked">platform admin</span>
                    ) : (
                      <>
                        <span className="mono">{m.role}</span> on <span className="mono">{m.repo_pattern}</span>
                      </>
                    )}
                  </td>
                  <td className="muted">{m.provider || "any"}</td>
                  <td style={{ textAlign: "right" }}>
                    <button type="button" className="btn btn-icon" onClick={() => void remove(m)}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <form className="form-row" onSubmit={(e) => void add(e)} style={{ alignItems: "flex-end", flexWrap: "wrap" }}>
        <label className="field">
          <span>IdP group</span>
          <input className="mono" value={form.group} onChange={(e) => setForm({ ...form, group: e.target.value })} placeholder="release-managers" required />
        </label>
        <label className="field">
          <span>Grants</span>
          <select value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
            <option value="repo">a role on repos</option>
            <option value="admin">platform admin</option>
          </select>
        </label>
        {form.kind === "repo" && (
          <>
            <label className="field">
              <span>Role</span>
              <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
                <option value="developer">developer</option>
                <option value="owner">owner</option>
                <option value="admin">admin</option>
              </select>
            </label>
            <label className="field">
              <span>Repos</span>
              <input className="mono" value={form.repo_pattern} onChange={(e) => setForm({ ...form, repo_pattern: e.target.value })} placeholder="* or acme/* or acme/shop" />
            </label>
          </>
        )}
        <label className="field">
          <span>From provider</span>
          <select value={form.provider} onChange={(e) => setForm({ ...form, provider: e.target.value })}>
            <option value="">any</option>
            <option value="oidc">OpenID Connect</option>
            <option value="ldap">LDAP</option>
          </select>
        </label>
        <div className="form-actions">
          <button type="submit" className="btn btn-primary">
            Add mapping
          </button>
        </div>
      </form>
      {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
    </div>
  );
}

/** How long a sign-in lasts. */
function SessionLifetime() {
  const [s, setS] = useState<SessionSettings | null>(null);
  const [hours, setHours] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    void getSessionSettings().then((v) => {
      setS(v);
      setHours(String(v.lifetime_hours));
    });
  }, []);
  if (!s) return null;
  const save = async (e: FormEvent) => {
    e.preventDefault();
    try {
      const v = await putSessionSettings(Number(hours));
      setS(v);
      setMsg({ ok: true, text: `New sign-ins last ${v.lifetime_hours} hours.` });
    } catch (err) {
      setMsg({ ok: false, text: err instanceof Error ? err.message : String(err) });
    }
  };
  return (
    <form className="card sso-card" onSubmit={(e) => void save(e)}>
      <h2>Session lifetime</h2>
      <div className="form-row" style={{ alignItems: "flex-end" }}>
        <label className="field" style={{ maxWidth: 220 }}>
          <span>Hours a sign-in lasts</span>
          <input type="number" min={s.min_hours} max={s.max_hours} value={hours} onChange={(e) => setHours(e.target.value)} />
          <span className="field-hint">
            {s.min_hours}–{s.max_hours} hours (up to {Math.round(s.max_hours / 24)} days). Applies to new sign-ins.
          </span>
        </label>
        <div className="form-actions">
          <button type="submit" className="btn btn-primary">
            Save
          </button>
        </div>
      </div>
      {msg && <div className={msg.ok ? "muted" : "error-banner"}>{msg.text}</div>}
    </form>
  );
}

export default function AdminSso() {
  const fetcher = useCallback(() => getSsoConfig(), []);
  const { data, error, loading, refresh } = usePoll(fetcher, 0, false);

  return (
    <div>
      <div className="page-head">
        <h1>Sign-in &amp; identity</h1>
      </div>

      <div className="doc-note doc-note-warn sso-warning">
        <div className="doc-note-title">Enforcement is immediate</div>
        Enabling a provider turns on login enforcement for everyone
        immediately. If you misconfigure and lock yourself out:{" "}
        <code>UPDATE sso_providers SET enabled=false;</code> via SQL.{" "}
        <Link to="/docs/sso-overview">Overview guide →</Link>
      </div>

      {error && <div className="error-banner">{error}</div>}
      {loading && !data && <div className="muted">Loading…</div>}

      {data &&
        PROVIDERS.map((meta) => (
          <ProviderCard
            key={meta.key}
            meta={meta}
            config={
              data.providers.find((p) => p.provider === meta.key) ?? null
            }
            redirectUri={data.redirect_uris[meta.key] ?? ""}
            onSaved={refresh}
          />
        ))}

      {data && <GroupMappings />}
      {data && <SessionLifetime />}
    </div>
  );
}
