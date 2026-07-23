import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getAuthProviders, loginUrl, type AuthProviders } from "../api";

const LABELS: Record<string, string> = {
  google: "Continue with Google",
  microsoft: "Continue with Microsoft",
  github: "Continue with GitHub",
};

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

  return (
    <div className="login-wrap">
      <div className="card login-card glow glow-neutral">
        <div className="login-brand">
          <span className="brand-mark">⚙</span> Forge CI
        </div>
        <p className="muted login-sub">Sign in to continue</p>

        {error && <div className="error-banner">{error}</div>}
        {!data && !error && <div className="muted">Loading…</div>}

        {data && data.providers.length > 0 && (
          <div className="login-buttons">
            {data.providers.map((p) => (
              <button
                key={p}
                type="button"
                className={`btn login-btn login-${p}`}
                onClick={() => {
                  window.location.href = loginUrl(p);
                }}
              >
                {LABELS[p] ?? `Continue with ${p}`}
              </button>
            ))}
          </div>
        )}

        {data && !data.enforced && (
          <p className="muted login-open">
            SSO is not enabled — open access.{" "}
            <Link to="/admin/sso">Configure providers</Link>
          </p>
        )}
      </div>
    </div>
  );
}
