import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  getChangeDiff,
  getDiffChunk,
  getDiffManifest,
  shortSha,
  type ChangePage,
  type DiffFileEntry,
  type DiffManifest,
  type DiffStatus,
} from "../api";
import { useChangeStream } from "../hooks/useChangeStream";

/** One line of a parsed unified diff. */
interface Line {
  kind: "add" | "del" | "ctx" | "hunk" | "note";
  text: string;
  oldNo?: number;
  newNo?: number;
}

const HUNK = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/;

/** Parses a file's unified patch (headers skipped) into numbered lines. */
export function parsePatch(patch: string): Line[] {
  const out: Line[] = [];
  let oldNo = 0;
  let newNo = 0;
  let inHunk = false;
  for (const raw of patch.split("\n")) {
    const m = HUNK.exec(raw);
    if (m) {
      oldNo = Number(m[1]);
      newNo = Number(m[2]);
      inHunk = true;
      out.push({ kind: "hunk", text: raw });
      continue;
    }
    if (!inHunk) continue; // diff --git / index / --- / +++ headers
    if (raw.startsWith("+")) out.push({ kind: "add", text: raw.slice(1), newNo: newNo++ });
    else if (raw.startsWith("-")) out.push({ kind: "del", text: raw.slice(1), oldNo: oldNo++ });
    else if (raw.startsWith("\\")) out.push({ kind: "note", text: raw });
    else if (raw !== "" || out.length > 0) out.push({ kind: "ctx", text: raw.slice(1), oldNo: oldNo++, newNo: newNo++ });
  }
  while (out.length && out[out.length - 1].kind === "ctx" && out[out.length - 1].text === "") out.pop();
  return out;
}

/** Pairs deletions with the additions that replace them, for side by side. */
function splitRows(lines: Line[]): [Line | null, Line | null][] {
  const rows: [Line | null, Line | null][] = [];
  for (let i = 0; i < lines.length; ) {
    const l = lines[i];
    if (l.kind === "del") {
      const dels: Line[] = [];
      const adds: Line[] = [];
      while (i < lines.length && lines[i].kind === "del") dels.push(lines[i++]);
      while (i < lines.length && lines[i].kind === "add") adds.push(lines[i++]);
      for (let k = 0; k < Math.max(dels.length, adds.length); k++) rows.push([dels[k] ?? null, adds[k] ?? null]);
      continue;
    }
    rows.push(l.kind === "add" ? [null, l] : [l, l]);
    i++;
  }
  return rows;
}

const STATUS: Record<string, string> = {
  added: "added",
  deleted: "deleted",
  renamed: "renamed",
  copied: "copied",
  modified: "",
  type_changed: "type changed",
};

function FileDiff({ file, manifestUrl, split }: { file: DiffFileEntry; manifestUrl: string; split: boolean }) {
  const collapsedByDefault = !!(file.generated || file.binary || file.too_large || file.chunk);
  const [open, setOpen] = useState(!collapsedByDefault);
  const [patch, setPatch] = useState<string | null>(file.patch ?? null);
  const [err, setErr] = useState<string | null>(null);
  const lines = useMemo(() => (patch ? parsePatch(patch) : []), [patch]);

  const expand = () => {
    setOpen(!open);
    if (!open && patch === null && file.chunk) {
      getDiffChunk(manifestUrl, file.chunk)
        .then(setPatch)
        .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
    }
  };
  const why = file.binary
    ? "Binary file"
    : file.too_large
      ? "Too large to show"
      : file.generated
        ? "Generated file (collapsed)"
        : file.chunk
          ? "Large diff (collapsed)"
          : "";

  return (
    <div className="diff-file" id={`f-${encodeURIComponent(file.path)}`}>
      <div className="diff-file-head" onClick={expand}>
        <span className="diff-toggle">{open ? "▾" : "▸"}</span>
        <span className="mono">{file.old_path ? `${file.old_path} → ${file.path}` : file.path}</span>
        {STATUS[file.status] && <span className="diff-status">{STATUS[file.status]}</span>}
        <span className="diff-counts">
          <span className="diff-plus">+{file.additions}</span> <span className="diff-minus">−{file.deletions}</span>
        </span>
      </div>
      {open &&
        (file.binary || file.too_large ? (
          <div className="diff-empty muted">{why}</div>
        ) : err ? (
          <div className="error-banner">{err}</div>
        ) : patch === null ? (
          <div className="diff-empty muted">Loading…</div>
        ) : lines.length === 0 ? (
          <div className="diff-empty muted">No content changes (mode or rename only).</div>
        ) : (
          <div className="diff-scroll">
            <table className={`diff-table${split ? " diff-split" : ""}`}>
              <tbody>
                {split
                  ? splitRows(lines).map(([l, r], i) =>
                      l?.kind === "hunk" || l?.kind === "note" ? (
                        <tr key={i} className={`diff-${l.kind}`}>
                          <td colSpan={4}>{l.text}</td>
                        </tr>
                      ) : (
                        <tr key={i}>
                          <td className="diff-no">{l?.oldNo ?? ""}</td>
                          <td className={`diff-code ${l && l.kind === "del" ? "diff-del" : ""}`}>{l?.text ?? ""}</td>
                          <td className="diff-no">{r?.newNo ?? ""}</td>
                          <td className={`diff-code ${r && r.kind === "add" ? "diff-add" : ""}`}>{r?.text ?? ""}</td>
                        </tr>
                      )
                    )
                  : lines.map((l, i) =>
                      l.kind === "hunk" || l.kind === "note" ? (
                        <tr key={i} className={`diff-${l.kind}`}>
                          <td colSpan={3}>{l.text}</td>
                        </tr>
                      ) : (
                        <tr key={i} className={`diff-${l.kind}`}>
                          <td className="diff-no">{l.oldNo ?? ""}</td>
                          <td className="diff-no">{l.newNo ?? ""}</td>
                          <td className="diff-code">
                            {l.kind === "add" ? "+" : l.kind === "del" ? "-" : " "}
                            {l.text}
                          </td>
                        </tr>
                      )
                    )}
              </tbody>
            </table>
          </div>
        ))}
    </div>
  );
}

const seenKey = (id: number) => `forge.change.${id}.seen`;

/**
 * Files changed. Diffs come from a runner's git mirror (never the host's
 * API), are computed once per pair of commits and cached by the browser for
 * good; while a runner computes one the page waits for the live update.
 */
export default function DiffView({ page }: { page: ChangePage }) {
  const { change } = page;
  const revisions = [...change.revision_list].sort((a, b) => a.number - b.number);
  const latest = revisions[revisions.length - 1];
  const [since, setSince] = useState<string>(""); // "" = all changes; else the revision head to compare from
  const [split, setSplit] = useState(() => {
    try {
      return localStorage.getItem("forge.diff.split") === "1";
    } catch {
      return false;
    }
  });
  const [status, setStatus] = useState<DiffStatus | null>(null);
  const [manifest, setManifest] = useState<DiffManifest | null>(null);
  const [error, setError] = useState<string | null>(null);

  // What this viewer saw, for "changes since you last looked": the first head
  // shown in this visit, else the one a previous visit showed.
  const previousVisit = useMemo(() => {
    try {
      return localStorage.getItem(seenKey(change.id)) ?? "";
    } catch {
      return "";
    }
  }, [change.id]);
  const shownThisVisit = useRef("");
  const seen = shownThisVisit.current && shownThisVisit.current !== latest?.head_sha ? shownThisVisit.current : previousVisit;
  const seenRev = revisions.find((r) => r.head_sha === seen && r.head_sha !== latest?.head_sha);

  const range = since && latest ? { from: since, to: latest.head_sha } : undefined;
  const load = useCallback(
    (retry = false) => {
      getChangeDiff(change.id, range, retry)
        .then((s) => {
          setStatus(s);
          setError(null);
          if (s.status === "ready" && s.manifest_url) {
            void getDiffManifest(s.manifest_url).then(setManifest).catch((e) => setError(String(e)));
          } else {
            setManifest(null);
          }
        })
        .catch((e) => setError(e instanceof Error ? e.message : String(e)));
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [change.id, change.head_sha, since]
  );
  useEffect(() => load(), [load]);
  useChangeStream(() => {
    if (status?.status !== "ready") load();
  }, change.id);
  useEffect(() => {
    if (manifest && !since && latest && manifest.to === latest.head_sha) {
      if (!shownThisVisit.current) shownThisVisit.current = latest.head_sha; // the first head shown stays the baseline
      try {
        localStorage.setItem(seenKey(change.id), latest.head_sha);
      } catch {
        // private mode: no memory of what was seen, nothing else lost
      }
    }
  }, [manifest, since, latest, change.id]);

  const toggleSplit = (v: boolean) => {
    setSplit(v);
    try {
      localStorage.setItem("forge.diff.split", v ? "1" : "0");
    } catch {
      // ignore
    }
  };

  return (
    <div className="card settings-form" style={{ marginTop: 16 }}>
      <h2>
        Files changed{" "}
        {status?.status === "ready" && (
          <span className="muted">
            · {status.files} file{status.files === 1 ? "" : "s"}, <span className="diff-plus">+{status.additions}</span>{" "}
            <span className="diff-minus">−{status.deletions}</span>
          </span>
        )}
      </h2>
      <div className="form-actions diff-controls">
        <select value={since} onChange={(e) => setSince(e.target.value)}>
          <option value="">All changes (vs {change.target_branch})</option>
          {revisions.slice(0, -1).map((r) => (
            <option key={r.number} value={r.head_sha}>
              Since r{r.number} ({shortSha(r.head_sha)}) → r{latest.number}
            </option>
          ))}
        </select>
        {seenRev && since !== seenRev.head_sha && (
          <button className="btn" onClick={() => setSince(seenRev.head_sha)}>
            Changes since you last looked (r{seenRev.number})
          </button>
        )}
        <label className="field-inline">
          <input type="checkbox" checked={split} onChange={(e) => toggleSplit(e.target.checked)} /> Side by side
        </label>
      </div>
      {error && <div className="error-banner">{error}</div>}
      {!status ? (
        <p className="muted">Loading…</p>
      ) : status.status === "unsupported" ? (
        <p className="muted">Diffs are not available for this host yet.</p>
      ) : status.status === "pending" ? (
        <p className="muted">A runner is computing this diff from its git mirror… it appears here as soon as it is ready.</p>
      ) : status.status === "failed" ? (
        <div className="error-banner">
          The diff could not be computed: {status.error}{" "}
          <button className="btn" onClick={() => load(true)}>
            Try again
          </button>
        </div>
      ) : !manifest ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          {manifest.truncated && (
            <p className="muted">This diff is very large: the patches of the last files were left out (their counts are shown).</p>
          )}
          {manifest.files.length === 0 && <p className="muted">No file changes.</p>}
          {manifest.files.map((f) => (
            <FileDiff key={f.path + (f.old_path ?? "")} file={f} manifestUrl={status.manifest_url!} split={split} />
          ))}
        </>
      )}
    </div>
  );
}
