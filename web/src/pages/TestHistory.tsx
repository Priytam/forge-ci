import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  decodeRepoParam,
  listTestHistory,
  relativeTime,
  testDuration,
  type TestCaseHistory,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import { StatusIcon } from "../components/StageDots";

/** Test-case status -> the pipeline/job status vocabulary StatusIcon and the
 *  .badge-* classes already speak, so both are reused as-is rather than
 *  inventing a second color system for the same pass/fail/neutral meaning. */
function asJobStatus(status: string): string {
  switch (status) {
    case "passed":
      return "success";
    case "failed":
      return "failed";
    default:
      return "created"; // skipped
  }
}

function LastRunBadge({ status }: { status: string }) {
  return <span className={`badge badge-${asJobStatus(status)}`}>{status}</span>;
}

/** Consecutive failures counting back from the newest run — 0 means the latest
 *  run passed (or the case has never failed), so there is nothing to flag. */
function failStreak(runs: TestCaseHistory["runs"]): number {
  let n = 0;
  for (const r of runs) {
    if (r.status !== "failed") break;
    n++;
  }
  return n;
}

function FailStreakBadge({ runs }: { runs: TestCaseHistory["runs"] }) {
  const n = failStreak(runs);
  if (n === 0) return <span className="muted">—</span>;
  return (
    <span className="badge badge-failed">
      {n} run{n === 1 ? "" : "s"}
    </span>
  );
}

/** Oldest-to-newest, left-to-right — the same reading direction as StageDots. */
function RecentRuns({ runs }: { runs: TestCaseHistory["runs"] }) {
  const ordered = [...runs].reverse();
  return (
    <span
      className="stage-dots"
      title={`${runs.length} recorded run${runs.length === 1 ? "" : "s"}`}
    >
      {ordered.map((r, i) => (
        <StatusIcon key={i} status={asJobStatus(r.status)} />
      ))}
    </span>
  );
}

export default function TestHistory() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

  const fetcher = useCallback(() => listTestHistory(repo), [repo]);
  const { data: cases, error, loading } = usePoll(fetcher, 10000);

  const [query, setQuery] = useState("");
  const [failingOnly, setFailingOnly] = useState(false);

  const failingCount = useMemo(
    () => (cases ?? []).filter((c) => c.runs[0]?.status === "failed").length,
    [cases]
  );

  const rows = useMemo(() => {
    if (!cases) return [];
    const q = query.trim().toLowerCase();
    return cases
      .filter(
        (c) =>
          !q ||
          c.name.toLowerCase().includes(q) ||
          (c.classname ?? "").toLowerCase().includes(q)
      )
      .filter((c) => !failingOnly || c.runs[0]?.status === "failed")
      .slice()
      .sort((a, b) => {
        // Currently-failing cases first — that's what a QA skimming this page
        // after a nightly run actually came here to find.
        const af = a.runs[0]?.status === "failed" ? 0 : 1;
        const bf = b.runs[0]?.status === "failed" ? 0 : 1;
        if (af !== bf) return af - bf;
        return a.name.localeCompare(b.name);
      });
  }, [cases, query, failingOnly]);

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <Link to={`/repos/${encodeURIComponent(repo)}`}>{repo}</Link>{" "}
        <span className="crumb-sep">/</span> <span>tests</span>
      </div>

      <div className="page-head">
        <h1>Tests</h1>
      </div>

      {error && (
        <div className="error-banner">Failed to load test history: {error}</div>
      )}
      {loading && !cases && <div className="muted">Loading…</div>}

      {cases && cases.length === 0 && (
        <div className="empty card">
          No test case history yet. A job with <code>report_junit</code> set
          records one here after its next run.
        </div>
      )}

      {cases && cases.length > 0 && (
        <>
          <div className="test-history-toolbar">
            <input
              type="search"
              placeholder="Filter by test name…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className="test-history-search"
            />
            <label className="check-row">
              <input
                type="checkbox"
                checked={failingOnly}
                onChange={(e) => setFailingOnly(e.target.checked)}
              />
              Currently failing only{failingCount > 0 && ` (${failingCount})`}
            </label>
          </div>

          {rows.length === 0 ? (
            <div className="empty card">No tests match this filter.</div>
          ) : (
            <div className="card table-card">
              <table>
                <thead>
                  <tr>
                    <th>Test</th>
                    <th>Last run</th>
                    <th>Failing</th>
                    <th>Recent</th>
                    <th>Duration</th>
                    <th>When</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((c) => {
                    const last = c.runs[0];
                    return (
                      <tr key={`${c.classname ?? ""}.${c.name}`}>
                        <td>
                          <div className="row-subject">{c.name}</div>
                          {c.classname && (
                            <div className="muted mono">{c.classname}</div>
                          )}
                        </td>
                        <td>
                          <LastRunBadge status={last.status} />
                        </td>
                        <td>
                          <FailStreakBadge runs={c.runs} />
                        </td>
                        <td>
                          <RecentRuns runs={c.runs} />
                        </td>
                        <td className="muted">
                          {testDuration(last.duration_seconds)}
                        </td>
                        <td className="muted">{relativeTime(last.created_at)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}
