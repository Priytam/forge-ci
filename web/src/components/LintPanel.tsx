import { useState } from "react";
import { lintConfig, type LintResult } from "../api";

/**
 * "Validate" for a pipeline YAML: compiles it on the server exactly as a run
 * on `ref` would (includes, rules, environment branch rules) and shows the
 * jobs that would run, or the error. Nothing is started.
 */
export default function LintPanel({
  config,
  repo,
  defaultRef = "main",
}: {
  config: string;
  repo?: string;
  defaultRef?: string;
}) {
  const [ref, setRef] = useState(defaultRef);
  const [result, setResult] = useState<LintResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      setResult(await lintConfig({ config, ref: ref.trim() || "main", repo }));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const refused = result?.jobs.filter((j) => j.refused) ?? [];
  return (
    <div className="lint-panel">
      <div className="filter-bar">
        <button type="button" className="btn" disabled={busy || !config.trim()} onClick={() => void run()}>
          {busy ? "Validating…" : "Validate"}
        </button>
        <label className="lint-ref">
          <span className="muted">as branch</span>
          <input value={ref} onChange={(e) => setRef(e.target.value)} aria-label="Branch to validate against" />
        </label>
        {result?.valid && (
          <span className="badge badge-success">
            valid · {result.jobs.length} job{result.jobs.length === 1 ? "" : "s"}
          </span>
        )}
        {result && !result.valid && <span className="badge badge-failed">invalid</span>}
      </div>
      {error && <div className="error-banner">{error}</div>}
      {result && !result.valid && <pre className="lint-error">{result.error}</pre>}
      {result?.valid && (
        <div className="card table-card">
          <table className="dense-table">
            <thead>
              <tr>
                <th>Stage</th>
                <th>Job</th>
                <th>Environment</th>
                <th>Needs</th>
                <th>Notes</th>
              </tr>
            </thead>
            <tbody>
              {result.jobs.map((j) => (
                <tr key={j.name}>
                  <td className="muted">{j.stage}</td>
                  <td>{j.name}</td>
                  <td>{j.environment && <span className="env-tag">{j.environment}</span>}</td>
                  <td className="mono muted">{j.needs.join(", ")}</td>
                  <td>
                    {j.manual && <span className="cell-meta">manual · </span>}
                    {j.allow_failure && <span className="cell-meta">allowed to fail · </span>}
                    {j.refused && <span className="status-reason">refused: {j.refused}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {refused.length > 0 && (
        <div className="muted">
          {refused.length} job{refused.length === 1 ? "" : "s"} would be refused on {result?.ref} by this
          server's environment rules.
        </div>
      )}
    </div>
  );
}
