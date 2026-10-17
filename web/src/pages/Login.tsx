import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { getAuthProviders, ldapLogin, loginUrl, type AuthProviderInfo, type AuthProviders } from "../api";

const LABELS: Record<string, string> = {
  google: "Continue with Google",
  microsoft: "Continue with Microsoft",
  github: "Continue with GitHub",
};

/** Username/password sign-in against the directory (LDAP / AD). */
function DirectoryForm({ info }: { info: AuthProviderInfo }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await ldapLogin(username.trim(), password);
      window.location.href = "/";
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };
  return (
    <form className="login-form" onSubmit={(e) => void submit(e)}>
      <div className="muted login-form-title">{info.label}</div>
      <label className="field">
        <span>Username</span>
        <input autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required />
      </label>
      <label className="field">
        <span>Password</span>
        <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
      </label>
      {error && <div className="error-banner">{error}</div>}
      <button type="submit" className="btn btn-primary login-btn" disabled={busy}>
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}

export default function Login() {
  const [data, setData] = useState<AuthProviders | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    getAuthProviders()
      .then(setData)
      .catch((err) =>
        setError(err instanceof Error ? err.message : String(err))
      );
  }, []);

  // Older servers only send names.
  const infos: AuthProviderInfo[] =
    data?.provider_info ?? (data?.providers ?? []).map((p) => ({ name: p, label: p, kind: "redirect" as const }));
  const buttons = infos.filter((i) => i.kind === "redirect");
  const directory = infos.find((i) => i.kind === "password");

  return (
    <div className="login-wrap">
      <div className="card login-card glow glow-neutral">
        <div className="login-brand">
          <span className="brand-mark">⚙</span> Forge CI
        </div>
        <p className="muted login-sub">Sign in to continue</p>

        {error && <div className="error-banner">{error}</div>}
        {!data && !error && <div className="muted">Loading…</div>}

        {buttons.length > 0 && (
          <div className="login-buttons">
            {buttons.map((p) => (
              <button
                key={p.name}
                type="button"
                className={`btn login-btn login-${p.name}`}
                onClick={() => {
                  window.location.href = loginUrl(p.name);
                }}
              >
                {LABELS[p.name] ?? `Continue with ${p.label}`}
              </button>
            ))}
          </div>
        )}
        {buttons.length > 0 && directory && <div className="login-or muted">or</div>}
        {directory && <DirectoryForm info={directory} />}

        {data && !data.enforced && (
          <p className="muted login-open">
            Sign-in is not enabled — open access.{" "}
            <Link to="/admin/sso">Configure sign-in</Link>
          </p>
        )}
      </div>
    </div>
  );
}
