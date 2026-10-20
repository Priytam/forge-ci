import { useEffect, useState } from "react";
import { getMe, refreshSession } from "../api";

const WARN_BEFORE_MS = 10 * 60 * 1000;

/**
 * Warns before the session expires and extends it in place, so a form in
 * progress (an approval-rule edit, a long comment) isn't lost to a silent
 * redirect to the login page.
 */
export default function SessionWatch() {
  const [expiresAt, setExpiresAt] = useState<number | null>(null);
  const [now, setNow] = useState(Date.now());
  const [busy, setBusy] = useState(false);
  const [dismissedFor, setDismissedFor] = useState<number | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      void getMe(true).then((me) => {
        if (cancelled) return;
        const t = me?.expires_at ? new Date(me.expires_at).getTime() : null;
        setExpiresAt(t && !Number.isNaN(t) ? t : null);
      });
    load();
    const reload = window.setInterval(load, 5 * 60 * 1000);
    const tick = window.setInterval(() => setNow(Date.now()), 30 * 1000);
    return () => {
      cancelled = true;
      window.clearInterval(reload);
      window.clearInterval(tick);
    };
  }, []);

  if (!expiresAt || dismissedFor === expiresAt) return null;
  const left = expiresAt - now;
  if (left > WARN_BEFORE_MS) return null;

  const stay = async () => {
    setBusy(true);
    try {
      const r = await refreshSession();
      setExpiresAt(new Date(r.expires_at).getTime());
    } catch {
      // The session is already gone: the next request redirects to login
      // with ?next= so this page comes back after signing in.
      window.location.assign(`/login?next=${encodeURIComponent(window.location.pathname + window.location.search)}`);
    } finally {
      setBusy(false);
    }
  };
  const mins = Math.max(0, Math.ceil(left / 60000));
  return (
    <div className="session-toast" role="status">
      <span>{left <= 0 ? "Your session has expired." : `Your session expires in ${mins} min.`}</span>
      <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void stay()}>
        {left <= 0 ? "Sign in again" : "Stay signed in"}
      </button>
      {left > 0 && (
        <button type="button" className="btn btn-icon" aria-label="Dismiss" onClick={() => setDismissedFor(expiresAt)}>
          ✕
        </button>
      )}
    </div>
  );
}
