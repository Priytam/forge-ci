import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { registerRepo, type RepoProvider } from "../api";

function derivedCloneUrl(
  provider: RepoProvider,
  repo: string,
  region: string
): string {
  const name = repo.trim();
  if (!name) return "";
  if (provider === "github") return `https://github.com/${name}.git`;
  if (provider === "bitbucket") return `https://bitbucket.org/${name}.git`;
  // git-remote-codecommit form; the runner signs it with SigV4 at clone time.
  if (provider === "codecommit") {
    const r = region.trim();
    return r ? `codecommit::${r}://${name}` : "";
  }
  return "";
}

const PROVIDER_LABEL: Record<RepoProvider, string> = {
  github: "GitHub",
  bitbucket: "Bitbucket",
  codecommit: "CodeCommit",
  other: "Other",
};

export default function AddRepo() {
  const navigate = useNavigate();
  const [provider, setProvider] = useState<RepoProvider>("github");
  const [repo, setRepo] = useState("");
  const [cloneUrl, setCloneUrl] = useState("");
  const [token, setToken] = useState("");
  const [branch, setBranch] = useState("main");
  const [authMethod, setAuthMethod] = useState<"pat" | "app">("pat");
  const [appId, setAppId] = useState("");
  const [installationId, setInstallationId] = useState("");
  const [appPrivateKey, setAppPrivateKey] = useState("");
  const [awsRegion, setAwsRegion] = useState("");
  const [awsRoleArn, setAwsRoleArn] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [connected, setConnected] = useState<string | null>(null);

  const autoUrl = provider !== "other";
  const effectiveCloneUrl = autoUrl
    ? derivedCloneUrl(provider, repo, awsRegion)
    : cloneUrl;
  // GitHub App auth only applies to GitHub repos.
  const appMode = provider === "github" && authMethod === "app";
  // CodeCommit holds no credential at all: it is reached with an AWS identity,
  // so the token field is replaced by the region and an optional role.
  const isCodeCommit = provider === "codecommit";

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await registerRepo({
        repo: repo.trim(),
        provider,
        clone_url: autoUrl ? undefined : cloneUrl.trim(),
        token: appMode || isCodeCommit ? undefined : token || undefined,
        default_branch: branch.trim() || "main",
        ...(isCodeCommit
          ? {
              aws_region: awsRegion.trim(),
              aws_role_arn: awsRoleArn.trim() || undefined,
            }
          : {}),
        ...(appMode
          ? {
              github_app_id: appId.trim(),
              github_installation_id: installationId.trim(),
              github_app_private_key: appPrivateKey || undefined,
            }
          : {}),
      });
      setConnected(repo.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (connected) {
    return (
      <div>
        <div className="breadcrumbs">
          <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
          <span>add repository</span>
        </div>
        <div className="card connect-success">
          <div className="connect-success-title">
            ✓ Repository connected and verified
          </div>
          <p className="muted">
            <span className="mono">{connected}</span> is registered — runners
            will clone the source at the pipeline's SHA before running jobs.
          </p>
          <h3>Next steps</h3>
          <ul className="next-steps">
            <li>
              <Link to={`/repos/${encodeURIComponent(connected)}/settings`}>
                Register the pipeline YAML
              </Link>{" "}
              <span className="muted">(repo Settings → Pipeline config)</span>
            </li>
            <li>
              <Link to="/docs/add-a-repo">Set up the push webhook</Link>{" "}
              <span className="muted">(guide, incl. WEBHOOK_SECRET)</span>
            </li>
            <li>
              <Link to={`/new?repo=${encodeURIComponent(connected)}`}>
                Run first pipeline
              </Link>
            </li>
          </ul>
        </div>
      </div>
    );
  }

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>add repository</span>
      </div>

      <div className="page-head">
        <h1>Add repository</h1>
      </div>

      <form
        className="card form glow glow-neutral"
        onSubmit={(e) => void onSubmit(e)}
      >
        <h3 className="form-section-title">Connect</h3>
        <div className="form-row">
          <label className="field">
            <span>Provider</span>
            <select
              value={provider}
              onChange={(e) => setProvider(e.target.value as RepoProvider)}
            >
              <option value="github">GitHub</option>
              <option value="bitbucket">Bitbucket</option>
              <option value="codecommit">AWS CodeCommit</option>
              <option value="other">Other</option>
            </select>
          </label>
          <label className="field">
            <span>{isCodeCommit ? "Repository name" : "Repository full name"}</span>
            <input
              className="mono"
              placeholder={isCodeCommit ? "my-service" : "owner/name"}
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              required
            />
            {isCodeCommit && (
              <span className="field-hint">
                The CodeCommit repository name alone — no owner/ prefix. It must
                match the repositoryName in the EventBridge event.
              </span>
            )}
          </label>
        </div>

        <label className="field">
          <span>Clone URL</span>
          <input
            className="mono"
            value={effectiveCloneUrl}
            onChange={(e) => setCloneUrl(e.target.value)}
            disabled={autoUrl}
            placeholder={autoUrl ? "" : "https://git.example.com/owner/name.git"}
            required={!autoUrl}
          />
          {autoUrl && (
            <span className="field-hint">
              Derived from the repo {isCodeCommit ? "name and region" : "full name"}{" "}
              for {PROVIDER_LABEL[provider]}.
            </span>
          )}
        </label>

        {isCodeCommit && (
          <div className="form-row">
            <label className="field">
              <span>AWS region</span>
              <input
                className="mono"
                placeholder="ap-south-1"
                value={awsRegion}
                onChange={(e) => setAwsRegion(e.target.value)}
                required
              />
            </label>
            <label className="field">
              <span>Role ARN (optional)</span>
              <input
                className="mono"
                placeholder="arn:aws:iam::123456789012:role/forge-codecommit-read"
                value={awsRoleArn}
                onChange={(e) => setAwsRoleArn(e.target.value)}
              />
              <span className="field-hint">
                Assumed by the control plane to read .forge-ci.yml. Leave empty to
                use its own identity. Runners authenticate separately and
                keylessly via per-job OIDC.
              </span>
            </label>
          </div>
        )}

        {provider === "github" && (
          <div className="field">
            <span>Authentication</span>
            <div className="segmented">
              <button
                type="button"
                className={authMethod === "pat" ? "seg-btn seg-active" : "seg-btn"}
                onClick={() => setAuthMethod("pat")}
              >
                Personal Access Token
              </button>
              <button
                type="button"
                className={authMethod === "app" ? "seg-btn seg-active" : "seg-btn"}
                onClick={() => setAuthMethod("app")}
              >
                GitHub App
              </button>
            </div>
          </div>
        )}

        {appMode ? (
          <>
            <div className="form-row">
              <label className="field">
                <span>App ID</span>
                <input
                  className="mono"
                  value={appId}
                  onChange={(e) => setAppId(e.target.value)}
                  required
                />
              </label>
              <label className="field">
                <span>Installation ID</span>
                <input
                  className="mono"
                  value={installationId}
                  onChange={(e) => setInstallationId(e.target.value)}
                  required
                />
              </label>
            </div>
            <label className="field">
              <span>App private key (PEM)</span>
              <textarea
                rows={5}
                className="mono"
                autoComplete="off"
                value={appPrivateKey}
                onChange={(e) => setAppPrivateKey(e.target.value)}
                placeholder="-----BEGIN RSA PRIVATE KEY-----"
                required
              />
              <span className="field-hint">
                Write-only — stored server-side, never displayed. The App needs
                Contents: read and Commit statuses: write.
              </span>
            </label>
            <label className="field">
              <span>Default branch</span>
              <input
                className="mono"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
              />
            </label>
          </>
        ) : isCodeCommit ? (
          <div className="form-row">
            <label className="field">
              <span>Default branch</span>
              <input
                className="mono"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
              />
              <span className="field-hint">
                CodeCommit stores no token: every call is IAM-signed, so there is
                no credential for Forge to hold.
              </span>
            </label>
          </div>
        ) : (
          <div className="form-row">
            <label className="field">
              <span>Access token</span>
              <input
                type="password"
                autoComplete="off"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
              <span className="field-hint">
                Optional for public repos. Use a fine-grained PAT (GitHub) or
                repository access token (Bitbucket) with read access. Stored
                server-side, never shown again.
              </span>
            </label>
            <label className="field">
              <span>Default branch</span>
              <input
                className="mono"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
              />
            </label>
          </div>
        )}

        {error && (
          <div className="error-banner">
            <strong>Could not verify repository access:</strong> {error}
          </div>
        )}

        <div className="form-actions">
          <button type="button" className="btn" onClick={() => navigate("/")}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            {busy ? "Verifying access…" : "Connect repository"}
          </button>
        </div>
      </form>
    </div>
  );
}
