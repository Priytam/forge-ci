import { useEffect, useState } from "react";
import { getServerStatus, type ServerStatus } from "../api";

/** Banner shown everywhere while an admin has maintenance mode on. */
export default function MaintenanceBanner() {
  const [status, setStatus] = useState<ServerStatus | null>(null);
  useEffect(() => {
    const load = () => void getServerStatus().then(setStatus).catch(() => undefined);
    load();
    const t = window.setInterval(load, 30000);
    return () => window.clearInterval(t);
  }, []);
  if (!status?.maintenance?.on) return null;
  return (
    <div className="maintenance-banner" role="status">
      <strong>Forge is upgrading</strong> — new jobs queue, running jobs continue.
      {status.maintenance.message ? ` ${status.maintenance.message}` : ""}
    </div>
  );
}
