import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import {
  createApiToken,
  getIsAdmin,
  getMe,
  getTokenPolicy,
  listApiTokens,
  putTokenPolicy,
  relativeTime,
  revokeApiToken,
  type ApiToken,
  type AuthUser,
  type TokenPolicy,
} from "../api";
import { usePoll } from "../hooks/usePoll";

/** Lifetime choices; "never" is offered only when the policy allows it. */
const LIFETIMES: { label: string; days: number }[] = [
  { label: "7 days", days: 7 },
  { label: "30 days", days: 30 },
  { label: "90 days", days: 90 },
  { label: "1 year", days: 365 },
];

function fmtDate(iso: string | null): string {
  if (!iso) return "never";
  return new Date(iso).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

const STATUS_BADGE: Record<ApiToken["status"], string> = {
  active: "badge-success",
  expired: "badge-canceled",
  revoked: "badge-canceled",
  disabled: "badge-blocked",
};

function CopyToken({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard unavailable — the token is still selectable text.
    }
  };
  return (
    <div className="runner-token-value">
      <code>{value}</code>
      <button type="button" className="btn" onClick={() => void copy()}>
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

function TokenTable({
  tokens,
  showOwner,
  onRevoke,
}: {
  tokens: ApiToken[];
  showOwner: boolean;
  onRevoke: (t: ApiToken) => void;
}) {
  return (
    <div className="card table-card">
      <table>
        <thead>
          <tr>
            {showOwner && <th>Owner</th>}
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
              {showOwner && <td>{t.owner}</td>}
              <td>{t.description || <span className="muted">(none)</span>}</td>
              <td className="mono">fci_…{t.token_suffix}</td>
              <td className="muted" title={t.created_at}>
                {fmtDate(t.created_at)}
              </td>
              <td className="muted" title={t.last_used_at ?? ""}>
                {t.last_used_at ? relativeTime(t.last_used_at) : "never"}
              </td>
              <td className="muted" title={t.expires_at ?? ""}>
                {fmtDate(t.expires_at)}
              </td>
              <td>
                <span
                  className={`badge ${STATUS_BADGE[t.status]}`}
                  title={
                    t.status === "disabled"
                      ? "Blocked by the admin token policy"
                      : undefined
                  }
                >
                  {t.status}
                </span>
              </td>
              <td>
                {!t.revoked && (
                  <button
                    type="button"
                    className="btn btn-icon"
                    onClick={() => onRevoke(t)}
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
  );
}

/** Admin-only: whether non-expiring tokens are allowed and a lifetime cap. */
function PolicyPanel({
  policy,
  onSaved,
}: {
  policy: TokenPolicy;
  onSaved: (p: TokenPolicy) => void;
}) {
  const [allow, setAllow] = useState(policy.allow_no_expiry);
  const [maxDays, setMaxDays] = useState(String(policy.max_lifetime_days || ""));
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setSaving(true);
    setMsg(null);
    try {
      const saved = await putTokenPolicy({
        allow_no_expiry: allow,
        max_lifetime_days: Number(maxDays) || 0,
      });
      onSaved(saved);
      setMsg("Saved.");
    } catch (err) {
      setMsg(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card" id="policy" style={{ marginBottom: 16 }}>
      <h2 style={{ marginTop: 0 }}>Token policy (admin)</h2>
      <p className="muted">
        Applies to everyone&apos;s tokens on every request: turning off
        non-expiring tokens disables existing ones straight away (they come back
        if you turn it on again), and a lifetime cap disables tokens older than
        the cap.
      </p>
      <form className="inline-form" onSubmit={(e) => void save(e)}>
        <label className="check-row">
          <input
            type="checkbox"
            checked={allow}
            onChange={(e) => setAllow(e.target.checked)}
          />
          Allow tokens that never expire
        </label>
        <label className="field">
          <span>Max lifetime (days, blank = no cap)</span>
          <input
            type="number"
            min={0}
            value={maxDays}
            onChange={(e) => setMaxDays(e.target.value)}
          />
        </label>
        <button type="submit" className="btn btn-primary" disabled={saving}>
          Save policy
        </button>
        {msg && <span className="muted">{msg}</span>}
      </form>
    </div>
  );
}

export default function ApiTokens() {
  const [me, setMe] = useState<AuthUser | null | undefined>(undefined);
  const [isAdmin, setIsAdmin] = useState(false);
  const [policy, setPolicy] = useState<TokenPolicy | null>(null);
  const [showAll, setShowAll] = useState(false);

  useEffect(() => {
    void getMe().then(setMe);
    void getIsAdmin().then(setIsAdmin);
    void getTokenPolicy()
      .then(setPolicy)
      .catch(() => setPolicy(null));
  }, []);

  const fetcher = useCallback(() => listApiTokens(showAll), [showAll]);
  const { data: tokens, error, loading, refresh } = usePoll(fetcher, 0, false);
  useEffect(() => refresh(), [showAll, refresh]);

  const [description, setDescription] = useState("");
  const [lifetime, setLifetime] = useState("90");
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [revealed, setRevealed] = useState<ApiToken | null>(null);

  const cap = policy?.max_lifetime_days ?? 0;
  const allowNever = (policy?.allow_no_expiry ?? true) && cap === 0;
  const options = LIFETIMES.filter((l) => cap === 0 || l.days <= cap);
  if (cap > 0 && !options.some((o) => o.days === cap)) {
    options.push({ label: `${cap} days (max)`, days: cap });
  }

  // Keep the selection valid when the policy loads or changes.
  const firstDays = options[0]?.days ?? 30;
  useEffect(() => {
    if (lifetime === "never" && !allowNever) setLifetime(String(firstDays));
    if (lifetime !== "never" && cap > 0 && Number(lifetime) > cap) setLifetime(String(cap));
  }, [allowNever, cap, lifetime, firstDays]);

  const onCreate = async (e: FormEvent) => {
    e.preventDefault();
    setCreating(true);
    setActionError(null);
    try {
      const created = await createApiToken(
        lifetime === "never"
          ? { description: description.trim(), no_expiry: true }
          : { description: description.trim(), expires_in_days: Number(lifetime) }
      );
      setRevealed(created);
      setDescription("");
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  };

  const onRevoke = async (t: ApiToken) => {
    if (
      !window.confirm(
        `Revoke "${t.description || "(no description)"}" (fci_…${t.token_suffix})? Anything still using it will get 401s.`
      )
    ) {
      return;
    }
    try {
      await revokeApiToken(t.id);
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  const signedIn = me !== null && me !== undefined && me.provider !== "api-token";

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>API tokens</span>
      </div>
      <div className="page-head">
        <h1>API tokens</h1>
      </div>

      <p className="muted">
        A personal token calls the Forge API on your behalf — same identity and
        permissions as you have in the UI. Send it as{" "}
        <code>Authorization: Bearer fci_…</code>. Revoke it here at any time.
      </p>
      <pre className="mono" style={{ whiteSpace: "pre-wrap" }}>
        {`curl -H "Authorization: Bearer $FORGE_TOKEN" ${window.location.origin}/api/v1/pipelines?repo=acme/app`}
      </pre>

      {isAdmin && policy && <PolicyPanel policy={policy} onSaved={setPolicy} />}

      {me === null && (
        <div className="empty card">
          Tokens belong to a signed-in user. SSO is not enabled on this server
          (open mode), so the API needs no token.
        </div>
      )}

      {revealed && (
        <div className="card runner-token-reveal">
          <div className="runner-token-reveal-head">
            <strong>Token created</strong>
            <span className="badge badge-blocked">shown once — copy it now</span>
          </div>
          <p className="muted">
            Expires: {revealed.expires_at ? fmtDate(revealed.expires_at) : "never"}.
            This is the only time the full token is shown.
          </p>
          <CopyToken value={revealed.token ?? ""} />
          <button type="button" className="btn" onClick={() => setRevealed(null)}>
            Done
          </button>
        </div>
      )}

      {signedIn && (
        <form className="inline-form" onSubmit={(e) => void onCreate(e)}>
          <label className="field">
            <span>Description</span>
            <input
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="e.g. deploy script on jenkins"
            />
          </label>
          <label className="field">
            <span>Expires</span>
            <select value={lifetime} onChange={(e) => setLifetime(e.target.value)}>
              {options.map((o) => (
                <option key={o.days} value={String(o.days)}>
                  {o.label}
                </option>
              ))}
              {allowNever && <option value="never">Never</option>}
            </select>
          </label>
          <button type="submit" className="btn btn-primary" disabled={creating}>
            {creating ? "Generating…" : "Generate token"}
          </button>
        </form>
      )}

      {isAdmin && signedIn && (
        <label className="check-row" style={{ margin: "12px 0" }}>
          <input
            type="checkbox"
            checked={showAll}
            onChange={(e) => setShowAll(e.target.checked)}
          />
          Show everyone&apos;s tokens (admin)
        </label>
      )}

      {actionError && <div className="error-banner">{actionError}</div>}
      {error && <div className="error-banner">Failed to load tokens: {error}</div>}
      {loading && !tokens && <div className="muted">Loading…</div>}
      {tokens && tokens.length === 0 && signedIn && (
        <div className="empty card">No tokens yet.</div>
      )}
      {tokens && tokens.length > 0 && (
        <TokenTable tokens={tokens} showOwner={showAll} onRevoke={(t) => void onRevoke(t)} />
      )}
    </div>
  );
}
