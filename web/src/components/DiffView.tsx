import Skeleton from "./ui/Skeleton";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  getChangeDiff,
  getDiffChunk,
  getDiffManifest,
  shortSha,
  createThread,
  getMe,
  type ChangePage,
  type ChangeThread,
  type DiffFileEntry,
  type DiffManifest,
  type DiffStatus,
} from "../api";
import { useChangeStream } from "../hooks/useChangeStream";
import Thread, { Composer } from "./Thread";
import Icon from "./Icon";

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

/** Comment threads on a file's lines, and the box to start one. */
export interface Annotations {
  /** threads at (side, line) of this file */
  at: (side: "new" | "old", line: number) => ChangeThread[];
  /** comments can be started here (the whole change, at its head) */
  canStart: boolean;
  open: { side: "new" | "old"; line: number } | null;
  start: (side: "new" | "old", line: number) => void;
  renderThreads: (threads: ChangeThread[]) => ReactNode;
  renderComposer: (side: "new" | "old", line: number, text: string) => ReactNode;
}

/** The thread and composer rows under one diff line. */
function annotationRows(a: Annotations | undefined, side: "new" | "old", line: number | undefined, text: string, cols: number, key: string) {
  if (!a || line === undefined) return null;
  const ts = a.at(side, line);
  const composing = a.open?.side === side && a.open.line === line;
  if (!ts.length && !composing) return null;
  return (
    <tr key={key} className="diff-thread-row">
      <td colSpan={cols}>
        <div className="thread-wrap">
          {ts.length > 0 && a.renderThreads(ts)}
          {composing && a.renderComposer(side, line, text)}
        </div>
      </td>
    </tr>
  );
}

/** A line number cell; with comments on, a commentable line carries the
 *  "+" to start one. */
function NoCell({ no, side, a, commentable }: { no?: number; side: "new" | "old"; a?: Annotations; commentable: boolean }) {
  return (
    <td className="diff-no">
      {a?.canStart && commentable && no !== undefined && (
        <button className="diff-comment-btn" title="Comment on this line" onClick={() => a.start(side, no)}>
          +
        </button>
      )}
      {no ?? ""}
    </td>
  );
}

function FileDiff({
  file,
  manifestUrl,
  split,
  annotations,
}: {
  file: DiffFileEntry;
  manifestUrl: string;
  split: boolean;
  annotations?: Annotations;
}) {
  const a = annotations;
  // A new or deleted file has one side only: side by side would be half empty.
  split = split && file.status !== "added" && file.status !== "deleted";
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
        <span className={open ? "diff-toggle diff-toggle-open" : "diff-toggle"}>
          <Icon name="expand" size={12} />
        </span>
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
                  ? splitRows(lines).flatMap(([l, r], i) =>
                      l?.kind === "hunk" || l?.kind === "note"
                        ? [
                            <tr key={i} className={`diff-${l.kind}`}>
                              <td colSpan={4}>{l.text}</td>
                            </tr>,
                          ]
                        : [
                            <tr key={i}>
                              <NoCell no={l?.oldNo} side="old" a={a} commentable={l?.kind === "del"} />
                              <td className={`diff-code ${l && l.kind === "del" ? "diff-del" : ""}`}>{l?.text ?? ""}</td>
                              <NoCell no={r?.newNo} side="new" a={a} commentable={!!r} />
                              <td className={`diff-code ${r && r.kind === "add" ? "diff-add" : ""}`}>{r?.text ?? ""}</td>
                            </tr>,
                            l && l.kind === "del" ? annotationRows(a, "old", l.oldNo, l.text, 4, `o${i}`) : null,
                            r ? annotationRows(a, "new", r.newNo, r.text, 4, `n${i}`) : null,
                          ]
                    )
                  : lines.flatMap((l, i) =>
                      l.kind === "hunk" || l.kind === "note"
                        ? [
                            <tr key={i} className={`diff-${l.kind}`}>
                              <td colSpan={3}>{l.text}</td>
                            </tr>,
                          ]
                        : [
                            <tr key={i} className={`diff-${l.kind}`}>
                              <NoCell no={l.oldNo} side="old" a={a} commentable={l.kind === "del"} />
                              <NoCell no={l.newNo} side="new" a={a} commentable={l.kind !== "del"} />
                              <td className="diff-code">
                                {l.kind === "add" ? "+" : l.kind === "del" ? "-" : " "}
                                {l.text}
                              </td>
                            </tr>,
                            l.kind === "del"
                              ? annotationRows(a, "old", l.oldNo, l.text, 3, `t${i}`)
                              : annotationRows(a, "new", l.newNo, l.text, 3, `t${i}`),
                          ]
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
export default function DiffView({ page, onChanged }: { page: ChangePage; onChanged: () => void }) {
  const { change } = page;
  const [composing, setComposing] = useState<{ path: string; side: "new" | "old"; line: number } | null>(null);
  const [me, setMe] = useState<string | null | undefined>(undefined);
  useEffect(() => {
    void getMe().then((u) => setMe(u?.email ?? null));
  }, []);
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

  // Threads sit on the lines of the whole change at its head; "since rN"
  // views show the diff alone.
  const showThreads = !since && status?.status === "ready" && status.to === change.head_sha;
  const lineThreads = page.threads.filter((t) => t.path && !t.outdated && t.line > 0);
  const annotationsFor = (path: string): Annotations => ({
    at: (side, line) => lineThreads.filter((t) => t.path === path && t.side === side && t.line === line),
    canStart: page.permissions.can_comment,
    open: composing && composing.path === path ? composing : null,
    start: (side, line) => setComposing({ path, side, line }),
    renderThreads: (ts: ChangeThread[]) =>
      ts.map((t) => (
        <Thread key={t.id} changeId={change.id} thread={t} canComment={page.permissions.can_comment} needAuthor={me === null} onChanged={onChanged} />
      )),
    renderComposer: (side, line, text) => (
      <Composer
        placeholder={`Comment on line ${line}…`}
        suggest={side === "new" ? text : undefined}
        needAuthor={me === null}
        onSubmit={async (body, author) => {
          await createThread(change.id, { path, side, line, body, author });
          onChanged();
        }}
        onCancel={() => setComposing(null)}
      />
    ),
  });

  const toggleSplit = (v: boolean) => {
    setSplit(v);
    try {
      localStorage.setItem("forge.diff.split", v ? "1" : "0");
    } catch {
      // ignore
    }
  };

  const jump = (path: string) =>
    document.getElementById(`f-${encodeURIComponent(path)}`)?.scrollIntoView({ behavior: "smooth", block: "start" });

  return (
    <div>
      <div className="cr-diff-bar">
        {status?.status === "ready" && (
          <span className="cr-diff-sum">
            {status.files} file{status.files === 1 ? "" : "s"} <span className="diff-plus">+{status.additions}</span>{" "}
            <span className="diff-minus">−{status.deletions}</span>
          </span>
        )}
        <select value={since} onChange={(e) => setSince(e.target.value)} aria-label="Which changes">
          <option value="">All changes (vs {change.target_branch})</option>
          {revisions.slice(0, -1).map((r) => (
            <option key={r.number} value={r.head_sha}>
              Since r{r.number} ({shortSha(r.head_sha)}) → r{latest.number}
            </option>
          ))}
        </select>
        {seenRev && since !== seenRev.head_sha && (
          <button className="btn" onClick={() => setSince(seenRev.head_sha)}>
            New since you last looked (r{seenRev.number})
          </button>
        )}
        <div className="segmented" role="radiogroup" aria-label="Diff layout">
          {[false, true].map((v) => (
            <button
              key={String(v)}
              type="button"
              role="radio"
              aria-checked={split === v}
              className={split === v ? "seg-btn seg-active" : "seg-btn"}
              onClick={() => toggleSplit(v)}
            >
              {v ? "Split" : "Unified"}
            </button>
          ))}
        </div>
      </div>
      {error && <div className="error-banner">{error}</div>}
      {!status ? (
        <Skeleton rows={4} />
      ) : status.status === "unsupported" ? (
        <div className="cr-empty">Diffs are not available for this host yet.</div>
      ) : status.status === "pending" ? (
        <div className="cr-empty">
          <Icon name="activity" size={24} />
          <div className="cr-empty-title">Computing the diff…</div>
          <div>A runner is working it out from its git mirror. It appears here as soon as it's ready.</div>
        </div>
      ) : status.status === "failed" ? (
        <div className="error-banner">
          The diff could not be computed: {status.error}{" "}
          <button className="btn" onClick={() => load(true)}>
            Try again
          </button>
        </div>
      ) : !manifest ? (
        <Skeleton rows={4} />
      ) : (
        <div className="cr-files">
          <nav className="cr-filelist" aria-label="Files">
            {manifest.files.map((f) => (
              <button key={f.path + (f.old_path ?? "")} className="cr-file-link" onClick={() => jump(f.path)} title={f.path}>
                <Icon name="file" size={12} />
                <span className="cr-file-name">{f.path}</span>
                <span className="cr-file-counts">
                  <span className="diff-plus">+{f.additions}</span> <span className="diff-minus">−{f.deletions}</span>
                </span>
              </button>
            ))}
          </nav>
          <div>
            {manifest.truncated && (
              <p className="muted">This diff is very large: the patches of the last files were left out (their counts are shown).</p>
            )}
            {manifest.files.length === 0 && <div className="cr-empty">No file changes.</div>}
            {manifest.files.map((f) => (
              <FileDiff
                key={f.path + (f.old_path ?? "")}
                file={f}
                manifestUrl={status.manifest_url!}
                split={split}
                annotations={showThreads ? annotationsFor(f.path) : undefined}
              />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
