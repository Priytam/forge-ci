import { useCallback, useEffect, useState } from "react";
import { getTokenPolicy, listApiTokens, revokeApiToken, type ApiToken, type TokenPolicy } from "../api";
import { usePoll } from "../hooks/usePoll";
import Skeleton from "../components/ui/Skeleton";
import { PolicyPanel, TokenTable } from "./ApiTokens";

/**
 * Settings → API tokens: the token policy that applies to everyone, and every
 * user's personal tokens (owner, last use, expiry) with revoke. People create
 * their own tokens from the user menu → API tokens.
 */
export default function AdminApiTokens() {
  const [policy, setPolicy] = useState<TokenPolicy | null>(null);
  const [policyError, setPolicyError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    getTokenPolicy()
      .then(setPolicy)
      .catch((err) => setPolicyError(err instanceof Error ? err.message : String(err)));
  }, []);

  const fetcher = useCallback(() => listApiTokens(true), []);
  const { data: tokens, error, loading, refresh } = usePoll(fetcher, 0, false);

  const onRevoke = async (t: ApiToken) => {
    if (!window.confirm(`Revoke ${t.owner}'s token "${t.description || "(no description)"}" (fci_…${t.token_suffix})? Anything still using it will get 401s.`)) {
      return;
    }
    try {
      await revokeApiToken(t.id);
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      <div className="page-head">
        <h1>API tokens</h1>
      </div>
      <p className="page-lede muted">
        Personal tokens call the Forge API as the person who made them. The policy below applies to every
        token on every request.
      </p>

      {policyError && <div className="error-banner">Failed to load the token policy: {policyError}</div>}
      {policy ? <PolicyPanel policy={policy} onSaved={setPolicy} /> : !policyError && <Skeleton rows={2} />}

      <h2 className="section-title">Everyone&apos;s tokens</h2>
      {actionError && <div className="error-banner">{actionError}</div>}
      {error && <div className="error-banner">Failed to load tokens: {error}</div>}
      {loading && !tokens && <Skeleton />}
      {tokens && tokens.length === 0 && <div className="empty card">No one has created a token yet.</div>}
      {tokens && tokens.length > 0 && (
        <TokenTable tokens={tokens} showOwner onRevoke={(t) => void onRevoke(t)} />
      )}
    </div>
  );
}
