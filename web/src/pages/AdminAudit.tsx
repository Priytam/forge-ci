import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, listAuditLog, relativeTime, type AuditEntry } from "../api";

const PAGE = 50;

function ResultBadge({ result }: { result: string }) {
  const cls =
    result === "ok"
      ? "badge-success"
      : result === "denied"
        ? "badge-blocked"
        : result === "error"
          ? "badge-failed"
          : "badge-created";
  return <span className={`badge ${cls}`}>{result}</span>;
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const [open, setOpen] = useState(false);
  const hasDetail =
    entry.detail !== null &&
    entry.detail !== undefined &&
    !(typeof entry.detail === "object" && Object.keys(entry.detail).length === 0);

  return (
    <>
      <tr className={hasDetail ? "row-link" : ""} onClick={() => hasDetail && setOpen((o) => !o)}>
        <td className="muted" title={new Date(entry.ts).toLocaleString()}>
          {relativeTime(entry.ts)}
        </td>
        <td>{entry.actor}</td>
        <td className="mono">{entry.action}</td>
        <td>
          {entry.target}
          {entry.repo && <span className="muted"> · {entry.repo}</span>}
        </td>
        <td>
          <ResultBadge result={entry.result} />
        </td>
        <td className="mono muted">{entry.source_ip}</td>
      </tr>
      {open && hasDetail && (
        <tr>
          <td colSpan={6}>
            <pre className="audit-detail">
              {JSON.stringify(entry.detail, null, 2)}
            </pre>
          </td>
        </tr>
      )}
    </>
  );
}

export default function AdminAudit() {
  const [repo, setRepo] = useState("");
  const [actor, setActor] = useState("");
  const [appliedRepo, setAppliedRepo] = useState("");
  const [appliedActor, setAppliedActor] = useState("");
  const [offset, setOffset] = useState(0);

  const [items, setItems] = useState<AuditEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [denied, setDenied] = useState(false);
  const [loading, setLoading] = useState(true);

  const load = useCallback(() => {
    setLoading(true);
    setError(null);
    listAuditLog({
      limit: PAGE,
      offset,
      repo: appliedRepo || undefined,
      actor: appliedActor || undefined,
    })
      .then((r) => {
        setItems(r.items);
        setTotal(r.total);
        setLoading(false);
      })
      .catch((err) => {
        if (err instanceof ApiError && err.status === 403) {
          setDenied(true);
        } else {
          setError(err instanceof Error ? err.message : String(err));
        }
        setLoading(false);
      });
  }, [offset, appliedRepo, appliedActor]);

  useEffect(() => {
    load();
  }, [load]);

  const applyFilters = () => {
    setAppliedRepo(repo.trim());
    setAppliedActor(actor.trim());
    setOffset(0);
  };

  if (denied) {
    return (
      <div>
        <div className="page-head">
          <h1>Audit log</h1>
        </div>
        <div className="error-banner">
          You do not have permission to view the audit log (admin only).
        </div>
      </div>
    );
  }

  const page = Math.floor(offset / PAGE) + 1;
  const pages = Math.max(1, Math.ceil(total / PAGE));

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>admin</span> <span className="crumb-sep">/</span> <span>audit</span>
      </div>

      <div className="page-head">
        <h1>Audit log</h1>
      </div>

      <div className="audit-filters">
        <input
          placeholder="Filter by repo"
          value={repo}
          onChange={(e) => setRepo(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && applyFilters()}
        />
        <input
          placeholder="Filter by actor"
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && applyFilters()}
        />
        <button type="button" className="btn" onClick={applyFilters}>
          Apply
        </button>
      </div>

      {error && <div className="error-banner">{error}</div>}
      {loading && <div className="muted">Loading…</div>}

      {!loading && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Actor</th>
                <th>Action</th>
                <th>Target</th>
                <th>Result</th>
                <th>Source IP</th>
              </tr>
            </thead>
            <tbody>
              {items.length === 0 ? (
                <tr>
                  <td colSpan={6} className="muted">
                    No audit entries.
                  </td>
                </tr>
              ) : (
                items.map((e) => <AuditRow key={e.id} entry={e} />)
              )}
            </tbody>
          </table>
        </div>
      )}

      <div className="pagination">
        <button
          type="button"
          className="btn"
          disabled={offset === 0}
          onClick={() => setOffset(Math.max(0, offset - PAGE))}
        >
          Previous
        </button>
        <span className="muted">
          Page {page} of {pages} · {total} total
        </span>
        <button
          type="button"
          className="btn"
          disabled={offset + PAGE >= total}
          onClick={() => setOffset(offset + PAGE)}
        >
          Next
        </button>
      </div>
    </div>
  );
}
