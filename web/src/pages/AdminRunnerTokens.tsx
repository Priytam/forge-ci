import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  ApiError,
  createRunnerToken,
  listRunnerTokens,
  relativeTime,
  revokeRunnerToken,
  type RunnerToken,
} from "../api";
import { usePoll } from "../hooks/usePoll";

/** A one-liner plus its own copy button — the unit repeated for the raw
 *  token and for each OS's install command below it. */
function CopyLine({ value, display }: { value: string; display?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard API can be unavailable (older browsers, some embeds) — the
      // value is still selectable text below, so copying by hand still works.
    }
  };
  return (
    <div className="runner-token-value">
      <code>{display ?? value}</code>
      <button type="button" className="btn" onClick={() => void copy()}>
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

/** Shown exactly once, right after creation — the server never returns the raw
 *  token again. Mirrors how a PAT page on GitHub/GitLab handles this. Gives a
 *  real copy-paste install line per OS instead of assuming a git checkout: a
 *  QA machine downloads the binary and registers in one command. */
function RevealPanel({ token, onDismiss }: { token: RunnerToken; onDismiss: () => void }) {
  const [tags, setTags] = useState("qa-laptop");
  const server = window.location.origin;
  const t = token.token ?? "";

  const macLinux = `curl -fsSL ${server}/api/v1/downloads/install-runner.sh | SERVER=${server} TOKEN=${t} TAGS=${tags} sh`;
  const windows = `$env:SERVER="${server}"; $env:TOKEN="${t}"; $env:TAGS="${tags}"; iwr ${server}/api/v1/downloads/install-runner.ps1 -UseBasicParsing | iex`;

  return (
    <div className="card runner-token-reveal">
      <div className="runner-token-reveal-head">
        <strong>Token created</strong>
        <span className="badge badge-blocked">shown once — copy it now</span>
      </div>
      <p className="muted">
        This is the only time the full token is shown. If you lose it, revoke
        this row and generate a new one.
      </p>
      <CopyLine value={t} />

      <label className="field runner-token-tags">
        <span>Tags for this runner</span>
        <input value={tags} onChange={(e) => setTags(e.target.value)} />
      </label>

      <p className="muted" style={{ marginBottom: 4 }}>
        macOS / Linux — downloads <code>forge-runner</code>, registers, and
        starts it (no git clone, no Go toolchain needed):
      </p>
      <CopyLine value={macLinux} />

      <p className="muted" style={{ marginBottom: 4 }}>
        Windows (PowerShell) — needs{" "}
        <a href="https://git-scm.com/download/win" target="_blank" rel="noreferrer">
          Git for Windows
        </a>{" "}
        installed first (ships <code>sh.exe</code>, which jobs run under):
      </p>
      <CopyLine value={windows} />

      <button type="button" className="btn" onClick={onDismiss}>
        Done
      </button>
    </div>
  );
}

export default function AdminRunnerTokens() {
  const fetcher = useCallback(() => listRunnerTokens(), []);
  const { data: tokens, error, loading, refresh } = usePoll(fetcher, 0, false);

  const [description, setDescription] = useState("");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [revealed, setRevealed] = useState<RunnerToken | null>(null);
  const [denied, setDenied] = useState(false);

  const onCreate = async (e: FormEvent) => {
    e.preventDefault();
    setCreating(true);
    setCreateError(null);
    try {
      const created = await createRunnerToken(description.trim());
      setRevealed(created);
      setDescription("");
      refresh();
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setDenied(true);
      } else {
        setCreateError(err instanceof Error ? err.message : String(err));
      }
    } finally {
      setCreating(false);
    }
  };

  const onRevoke = async (t: RunnerToken) => {
    if (
      !window.confirm(
        `Revoke the token "${t.description || "(no description)"}" (…${t.token_suffix})? Any runner still using it will start failing to authenticate.`
      )
    ) {
      return;
    }
    try {
      await revokeRunnerToken(t.id);
      refresh();
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setDenied(true);
      } else {
        setCreateError(err instanceof Error ? err.message : String(err));
      }
    }
  };

  if (denied) {
    return (
      <div>
        <div className="page-head">
          <h1>Runner tokens</h1>
        </div>
        <div className="error-banner">
          You do not have permission to manage runner tokens (admin only).
        </div>
      </div>
    );
  }

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>admin</span> <span className="crumb-sep">/</span> <span>runner tokens</span>
      </div>

      <div className="page-head">
        <h1>Runner tokens</h1>
      </div>

      <p className="muted">
        A runner needs one of these when the server is running with{" "}
        <code>RUNNER_AUTH=on</code> — pass it as <code>--token</code> (or
        <code> RUNNER_TOKEN</code>) when registering it. See{" "}
        <Link to="/docs/runners">Runners</Link>.
      </p>

      {revealed && (
        <RevealPanel token={revealed} onDismiss={() => setRevealed(null)} />
      )}

      <form className="inline-form" onSubmit={(e) => void onCreate(e)}>
        <label className="field">
          <span>Description</span>
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder="e.g. priya's laptop — qa-laptop"
          />
        </label>
        <button type="submit" className="btn btn-primary" disabled={creating}>
          {creating ? "Generating…" : "Generate token"}
        </button>
      </form>

      {createError && <div className="error-banner">{createError}</div>}
      {error && <div className="error-banner">Failed to load tokens: {error}</div>}
      {loading && !tokens && <div className="muted">Loading…</div>}

      {tokens && tokens.length === 0 && (
        <div className="empty card">No runner tokens yet.</div>
      )}

      {tokens && tokens.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Description</th>
                <th>Token</th>
                <th>Created</th>
                <th>Last used</th>
                <th>Status</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td>{t.description || <span className="muted">(none)</span>}</td>
                  <td className="mono">…{t.token_suffix}</td>
                  <td className="muted">{relativeTime(t.created_at)}</td>
                  <td className="muted">
                    {t.last_used_at ? relativeTime(t.last_used_at) : "never"}
                  </td>
                  <td>
                    {t.revoked ? (
                      <span className="badge badge-canceled">revoked</span>
                    ) : (
                      <span className="badge badge-success">active</span>
                    )}
                  </td>
                  <td>
                    {!t.revoked && (
                      <button
                        type="button"
                        className="btn btn-icon"
                        onClick={() => void onRevoke(t)}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
