import { useState, type ReactNode } from "react";

/** Monospace code block with a copy button. */
export function CodeBlock({ code }: { code: string }) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable (insecure context) — ignore
    }
  };

  return (
    <div className="code-block">
      <button type="button" className="code-copy" onClick={() => void copy()}>
        {copied ? "Copied ✓" : "Copy"}
      </button>
      <pre>{code}</pre>
    </div>
  );
}

/** Info / warning callout. */
export function Note({
  tone = "info",
  title,
  children,
}: {
  tone?: "info" | "warn";
  title?: string;
  children: ReactNode;
}) {
  return (
    <div className={`doc-note doc-note-${tone}`}>
      {title && <div className="doc-note-title">{title}</div>}
      <div>{children}</div>
    </div>
  );
}

/** Numbered step list; pass <li> children. */
export function Steps({ children }: { children: ReactNode }) {
  return <ol className="doc-steps">{children}</ol>;
}

/** Simple table with header row. */
export function DocTable({
  head,
  rows,
}: {
  head: ReactNode[];
  rows: ReactNode[][];
}) {
  return (
    <div className="table-wrap doc-table">
      <table>
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={i}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}>
              {row.map((cell, j) => (
                <td key={j}>{cell}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
