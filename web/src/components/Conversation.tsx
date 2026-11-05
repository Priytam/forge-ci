import { useEffect, useState, type ReactNode } from "react";
import { createThread, getMe, relativeTime, shortSha, type ChangePage } from "../api";
import Avatar, { displayName } from "./Avatar";
import Icon from "./Icon";
import Thread, { CommentBody, Composer } from "./Thread";

interface Item {
  at: string;
  key: string;
  node: ReactNode;
}

const DELIVERY: Record<string, string> = { pending: "sending to the host…", local: "in Forge" };

/**
 * The change's story, top to bottom: its description, then every comment,
 * review and push in the order they happened, and the box to add to it.
 * Threads that left the diff (outdated) show here too. Synced both ways
 * with the host when it has comments.
 */
export default function Conversation({ page, onChanged }: { page: ChangePage; onChanged: () => void }) {
  const { change, threads, permissions, capabilities } = page;
  const [me, setMe] = useState<string | null | undefined>(undefined);
  useEffect(() => {
    void getMe().then((u) => setMe(u?.email ?? null));
  }, []);
  const conversation = threads.find((t) => !t.path);
  const away = threads.filter((t) => t.path && (t.outdated || t.line === 0));

  const items: Item[] = [];
  for (const c of conversation?.comments ?? []) {
    items.push({
      at: c.created_at,
      key: `c${c.id}`,
      node: (
        <div className="cr-item">
          <span className="cr-avatar-col">
            <Avatar identity={c.author_email || c.author} size={28} />
          </span>
          <div className="cr-card">
            <div className="cr-card-head">
              <b>{displayName(c.author)}</b> commented {relativeTime(c.created_at)}
              {DELIVERY[c.state] && <span>· {DELIVERY[c.state]}</span>}
              {c.state === "failed" && <span className="error-text">· not posted: {c.error}</span>}
            </div>
            <div className="cr-card-body">
              <CommentBody body={c.body} />
            </div>
          </div>
        </div>
      ),
    });
  }
  for (const r of change.reviews) {
    if (r.verdict === "comment") continue;
    const ok = r.verdict === "approve";
    const gone = r.verdict === "dismissed";
    items.push({
      at: r.submitted_at,
      key: `r${r.id}`,
      node: (
        <div className="cr-item">
          <span className={`cr-ev-ic ${gone ? "" : ok ? "cr-ev-ok" : "cr-ev-no"}`}>
            <span>
              <Icon name={gone ? "close" : ok ? "check" : "alert"} size={12} />
            </span>
          </span>
          <div className="cr-ev">
            <b>{displayName(r.reviewer)}</b>{" "}
            {gone ? "withdrew their review" : ok ? "approved these changes" : "requested changes"}{" "}
            {r.source === "host" && "on the host "}· {relativeTime(r.submitted_at)}
            {r.body && (
              <div className="cr-card cr-card-body" style={{ marginTop: 6, color: "var(--text)" }}>
                {r.body}
              </div>
            )}
          </div>
        </div>
      ),
    });
  }
  for (const rev of change.revision_list) {
    items.push({
      at: rev.pushed_at,
      key: `p${rev.number}`,
      node: (
        <div className="cr-item">
          <span className="cr-ev-ic">
            <span>
              <Icon name="upgrade" size={12} />
            </span>
          </span>
          <div className="cr-ev">
            {rev.number === 1 ? "Opened with" : "Pushed"} <span className="mono">{shortSha(rev.head_sha)}</span> ·
            revision {rev.number} · {relativeTime(rev.pushed_at)}
          </div>
        </div>
      ),
    });
  }
  for (const t of away) {
    items.push({
      at: t.comments[0]?.created_at ?? "",
      key: `t${t.id}`,
      node: (
        <div className="cr-item">
          <span className="cr-ev-ic">
            <span>
              <Icon name="comment" size={12} />
            </span>
          </span>
          <div style={{ flex: 1, minWidth: 0 }}>
            <Thread
              changeId={change.id}
              thread={t}
              canComment={permissions.can_comment}
              needAuthor={me === null}
              onChanged={onChanged}
              context={
                <span className="mono">
                  {t.path}:{t.original_line || t.line}{" "}
                </span>
              }
            />
          </div>
        </div>
      ),
    });
  }
  items.sort((a, b) => a.at.localeCompare(b.at));

  return (
    <div>
      <div className="cr-item">
        <span className="cr-avatar-col">
          <Avatar identity={change.author} size={28} />
        </span>
        <div className="cr-card">
          <div className="cr-card-head">
            <b>{displayName(change.author)}</b> opened this change{" "}
            {change.created_at && relativeTime(change.created_at)}
          </div>
          <div className="cr-card-body">
            {change.body?.trim() ? change.body : <span className="muted">No description.</span>}
          </div>
        </div>
      </div>
      <div className="cr-timeline">
        {items.map((i) => (
          <div key={i.key}>{i.node}</div>
        ))}
      </div>
      {permissions.can_comment ? (
        <div className="cr-compose">
          <Composer
            placeholder={capabilities.threads ? "Write a comment… (posted to the host too)" : "Write a comment…"}
            needAuthor={me === null}
            onSubmit={async (body, author) => {
              await createThread(change.id, { body, author });
              onChanged();
            }}
          />
        </div>
      ) : (
        <p className="muted cr-compose">You need a role on {change.repo} to comment.</p>
      )}
    </div>
  );
}
