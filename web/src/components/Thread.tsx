import { useState, type ReactNode } from "react";
import { relativeTime, replyThread, resolveThread, type ChangeThread } from "../api";
import Avatar from "./Avatar";

const SUGGESTION = /```suggestion\n([\s\S]*?)\n?```/;

/** A comment body: text as written, a ```suggestion block shown as the
 *  proposed replacement (GitHub's syntax, so it round-trips). */
export function CommentBody({ body }: { body: string }) {
  const m = SUGGESTION.exec(body);
  if (!m) return <div className="comment-body">{body}</div>;
  const before = body.slice(0, m.index).trim();
  const after = body.slice(m.index + m[0].length).trim();
  return (
    <div className="comment-body">
      {before && <div>{before}</div>}
      <div className="suggestion">
        <div className="suggestion-head">Suggested change</div>
        <pre>
          {m[1].split("\n").map((l, i) => (
            <div key={i} className="diff-add">
              +{l}
            </div>
          ))}
        </pre>
      </div>
      {after && <div>{after}</div>}
    </div>
  );
}

const STATE: Record<string, ReactNode> = {
  pending: <span className="muted"> · sending to the host…</span>,
  local: <span className="muted"> · in Forge</span>,
};

/** Write box shared by new comments and replies. */
export function Composer({
  placeholder,
  suggest,
  needAuthor,
  onSubmit,
  onCancel,
}: {
  placeholder: string;
  /** the line's text, to start a suggestion from; undefined: no suggestion */
  suggest?: string;
  needAuthor: boolean;
  onSubmit: (body: string, author?: string) => Promise<unknown>;
  onCancel?: () => void;
}) {
  const [body, setBody] = useState("");
  const [author, setAuthor] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async () => {
    setBusy(true);
    setErr(null);
    try {
      await onSubmit(body, needAuthor ? author : undefined);
      setBody("");
      onCancel?.();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="composer">
      {needAuthor && (
        <input value={author} onChange={(e) => setAuthor(e.target.value)} placeholder="you@company.com (sign-in is off)" />
      )}
      <textarea rows={3} value={body} onChange={(e) => setBody(e.target.value)} placeholder={placeholder} />
      <div className="form-actions">
        <button className="btn btn-primary" disabled={busy || !body.trim()} onClick={() => void submit()}>
          Comment
        </button>
        {suggest !== undefined && (
          <button className="btn" type="button" onClick={() => setBody((b) => `${b}${b ? "\n" : ""}\`\`\`suggestion\n${suggest}\n\`\`\``)}>
            Suggest a change
          </button>
        )}
        {onCancel && (
          <button className="btn" type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>
      {err && <div className="error-banner">{err}</div>}
    </div>
  );
}

/** One thread: its comments, reply box and resolve toggle. */
export default function Thread({
  changeId,
  thread,
  canComment,
  needAuthor,
  onChanged,
  context,
}: {
  changeId: number;
  thread: ChangeThread;
  canComment: boolean;
  needAuthor: boolean;
  onChanged: () => void;
  /** a heading, for threads shown away from their line */
  context?: ReactNode;
}) {
  const [replying, setReplying] = useState(false);
  const [open, setOpen] = useState(!thread.resolved);
  const toggle = async () => {
    await resolveThread(changeId, thread.id, !thread.resolved);
    onChanged();
  };
  return (
    <div className={`thread${thread.resolved ? " thread-resolved" : ""}`}>
      {(context || thread.resolved || thread.outdated) && (
        <div className="thread-head" onClick={() => setOpen(!open)}>
          {context}
          {thread.outdated && <span className="badge badge-canceled">outdated</span>}
          {thread.resolved && <span className="badge badge-success">resolved</span>}
          {!open && <span className="muted"> · {thread.comments.length} comment(s)</span>}
        </div>
      )}
      {open && (
        <>
          {thread.comments.map((c) => (
            <div key={c.id} className="comment">
              <div className="comment-meta">
                <Avatar identity={c.author_email || c.author} size={16} /> <b>{c.author}</b>{" "}
                <span className="muted">{relativeTime(c.created_at)}</span>
                {STATE[c.state]}
                {c.state === "failed" && <span className="error-text"> · not posted: {c.error}</span>}
              </div>
              <CommentBody body={c.body} />
            </div>
          ))}
          {canComment && (
            <div className="thread-actions">
              {replying ? (
                <Composer
                  placeholder="Reply…"
                  needAuthor={needAuthor}
                  onSubmit={async (body, author) => {
                    await replyThread(changeId, thread.id, body, author);
                    onChanged();
                  }}
                  onCancel={() => setReplying(false)}
                />
              ) : (
                <>
                  <button className="btn" onClick={() => setReplying(true)}>
                    Reply
                  </button>{" "}
                  {thread.path && (
                    <button className="btn" onClick={() => void toggle()}>
                      {thread.resolved ? "Reopen" : "Resolve"}
                    </button>
                  )}
                </>
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
}
