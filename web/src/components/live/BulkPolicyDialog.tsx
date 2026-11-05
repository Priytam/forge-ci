import { useState } from "react";
import { setLivePolicies, type BulkPolicyResult, type LiveFacet } from "../../api";

type Policy = { auto_sync: boolean; prune: boolean; self_heal: boolean };

/**
 * Sets one sync policy on every application the current filters match:
 * Preview (a dry run) lists what would change and what would be skipped and
 * why; Apply does it. Each app gets the same checks as its own Auto-sync
 * button.
 */
export default function BulkPolicyDialog({
  filter,
  count,
  onClose,
}: {
  filter: Partial<Record<LiveFacet, string[]>> & { q?: string };
  count: number;
  onClose: (changed: boolean) => void;
}) {
  const [policy, setPolicy] = useState<Policy>({ auto_sync: true, prune: false, self_heal: false });
  const [preview, setPreview] = useState<BulkPolicyResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<BulkPolicyResult | null>(null);

  const run = async (dryRun: boolean) => {
    setBusy(true);
    setError(null);
    try {
      const r = await setLivePolicies({ filter, policy, dry_run: dryRun });
      if (dryRun) setPreview(r);
      else setDone(r);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const change = (p: Policy) => {
    setPolicy(p.auto_sync ? p : { auto_sync: false, prune: false, self_heal: false });
    setPreview(null);
  };
  const shown = done ?? preview;

  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose(Boolean(done))}>
      <div className="dialog" role="dialog" aria-modal="true" aria-labelledby="bulk-policy-title">
        <h2 id="bulk-policy-title">Sync policy for {count} application{count === 1 ? "" : "s"}</h2>
        <div className="dialog-body">
          <p className="muted">Applies to every application the current filters match.</p>
          <label className="check-row">
            <input type="checkbox" checked={policy.auto_sync} disabled={Boolean(done)}
              onChange={(e) => change({ ...policy, auto_sync: e.target.checked })} />
            Auto-sync each new revision
          </label>
          <label className="check-row">
            <input type="checkbox" checked={policy.prune} disabled={!policy.auto_sync || Boolean(done)}
              onChange={(e) => change({ ...policy, prune: e.target.checked })} />
            Prune what Git dropped
          </label>
          <label className="check-row">
            <input type="checkbox" checked={policy.self_heal} disabled={!policy.auto_sync || Boolean(done)}
              onChange={(e) => change({ ...policy, self_heal: e.target.checked })} />
            Self-heal live changes
          </label>
          {error && <div className="error-banner">{error}</div>}
          {shown && (
            <div className="lv-bulk-result">
              <strong>
                {done ? "Changed" : "Would change"} {shown.changed.length}
                {shown.skipped.length > 0 && `, skip ${shown.skipped.length}`}
              </strong>
              {shown.skipped.length > 0 && (
                <ul>
                  {shown.skipped.slice(0, 20).map((s) => (
                    <li key={s.id}>
                      <span className="mono">{s.target}/{s.key}</span> — {s.reason}
                    </li>
                  ))}
                  {shown.skipped.length > 20 && <li>…and {shown.skipped.length - 20} more</li>}
                </ul>
              )}
            </div>
          )}
        </div>
        <div className="dialog-actions">
          <button type="button" className="btn" onClick={() => onClose(Boolean(done))}>
            {done ? "Close" : "Cancel"}
          </button>
          {!done && (
            <>
              <button type="button" className="btn" disabled={busy} onClick={() => void run(true)}>
                Preview
              </button>
              <button type="button" className="btn btn-primary" disabled={busy || !preview || preview.changed.length === 0}
                onClick={() => void run(false)}>
                {busy ? "Working…" : `Apply to ${preview?.changed.length ?? "…"}`}
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
