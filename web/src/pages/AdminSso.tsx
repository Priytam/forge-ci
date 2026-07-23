import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  getSsoConfig,
  putSsoProvider,
  type SsoProviderConfig,
} from "../api";
import { usePoll } from "../hooks/usePoll";

interface ProviderMeta {
  key: string;
  label: string;
  guide: string;
  hasTenant: boolean;
  hasAllowedDomain: boolean;
  allowedDomainHint?: string;
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

      <label className="check-row">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => setEnabled(e.target.checked)}
        />
        <span>Enabled</span>
      </label>

      <div className="form-row">
        <label className="field">
          <span>Client ID</span>
          <input
            className="mono"
            value={clientId}
            onChange={(e) => setClientId(e.target.value)}
          />
        </label>
        <label className="field">
          <span>Client secret</span>
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

export default function AdminSso() {
  const fetcher = useCallback(() => getSsoConfig(), []);
  const { data, error, loading, refresh } = usePoll(fetcher, 0, false);

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>admin</span> <span className="crumb-sep">/</span> <span>sso</span>
      </div>

      <div className="page-head">
        <h1>SSO</h1>
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
    </div>
  );
}
