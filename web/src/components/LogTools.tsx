import { useEffect, useMemo, useState } from "react";
import { getLogSummary, type LogSummary } from "../api";

const MAX_MATCHES = 200;

function Highlight({ text, q }: { text: string; q: string }) {
  if (!q) return <>{text}</>;
  const lower = text.toLowerCase();
  const parts: JSX.Element[] = [];
  let i = 0;
  let k = 0;
  while (i < text.length) {
    const at = lower.indexOf(q, i);
    if (at < 0) {
      parts.push(<span key={k++}>{text.slice(i)}</span>);
      break;
    }
    if (at > i) parts.push(<span key={k++}>{text.slice(i, at)}</span>);
    parts.push(<mark key={k++}>{text.slice(at, at + q.length)}</mark>);
    i = at + q.length;
  }
  return <>{parts}</>;
}

/**
 * Search a job's log and, for failed jobs, show where the first error is.
 * Matches are listed with line numbers (click one for the lines around it),
 * which stays fast on logs far too long to scroll through.
 */
export default function LogTools({ jobId, text, failed }: { jobId: number; text: string; failed: boolean }) {
  const [input, setInput] = useState("");
  const [q, setQ] = useState("");
  const [open, setOpen] = useState<number | null>(null);
  const [summary, setSummary] = useState<LogSummary | null>(null);

  useEffect(() => {
    const t = window.setTimeout(() => setQ(input.trim().toLowerCase()), 150);
    return () => window.clearTimeout(t);
  }, [input]);

  useEffect(() => {
    setSummary(null);
    if (!failed) return;
    void getLogSummary(jobId)
      .then(setSummary)
      .catch(() => setSummary(null));
  }, [jobId, failed]);

  const lines = useMemo(() => text.split("\n"), [text]);
  const matches = useMemo(() => {
    if (!q) return [];
    const out: number[] = [];
    for (let i = 0; i < lines.length && out.length < MAX_MATCHES + 1; i++) {
      if (lines[i].toLowerCase().includes(q)) out.push(i);
    }
    return out;
  }, [q, lines]);

  const context = (i: number) => lines.slice(Math.max(0, i - 5), i + 6).map((l, k) => ({ n: Math.max(0, i - 5) + k, l }));

  return (
    <div className="log-tools">
      {summary?.first_error && (
        <div className="card log-first-error">
          <div className="log-first-error-head">
            <span className="badge badge-failed">first error</span>
            <span className="muted">line {summary.first_error_line} of {summary.lines}</span>
          </div>
          <pre className="log-snippet">
            {(summary.context ?? []).map((l, k) => {
              const n = summary.first_error_line - Math.min(2, summary.first_error_line - 1) + k;
              return (
                <div key={k} className={n === summary.first_error_line ? "log-line log-line-hit" : "log-line"}>
                  <span className="log-ln">{n}</span>
                  {l}
                </div>
              );
            })}
          </pre>
        </div>
      )}

      <div className="filter-bar log-search">
        <input
          className="filter-search"
          placeholder="Search this log…"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          aria-label="Search log"
        />
        {q && (
          <span className="muted">
            {matches.length > MAX_MATCHES ? `${MAX_MATCHES}+` : matches.length} matching line
            {matches.length === 1 ? "" : "s"}
          </span>
        )}
      </div>
      {q && matches.length > 0 && (
        <div className="card log-matches">
          {matches.slice(0, MAX_MATCHES).map((i) => (
            <div key={i}>
              <button type="button" className="log-match" onClick={() => setOpen(open === i ? null : i)}>
                <span className="log-ln">{i + 1}</span>
                <span className="log-match-text">
                  <Highlight text={lines[i]} q={q} />
                </span>
              </button>
              {open === i && (
                <pre className="log-snippet">
                  {context(i).map(({ n, l }) => (
                    <div key={n} className={n === i ? "log-line log-line-hit" : "log-line"}>
                      <span className="log-ln">{n + 1}</span>
                      <Highlight text={l} q={q} />
                    </div>
                  ))}
                </pre>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
