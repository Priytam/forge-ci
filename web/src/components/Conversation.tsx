import { useEffect, useState } from "react";
import { createThread, getMe, type ChangePage } from "../api";
import Thread, { Composer } from "./Thread";

/**
 * The change's discussion: the conversation (comments not on a line) and
 * every line thread that no longer sits on a line of the current diff
 * (outdated). Synced both ways with the host when it has comments.
 */
export default function Conversation({ page, onChanged }: { page: ChangePage; onChanged: () => void }) {
  const { change, threads, permissions, capabilities } = page;
  const [me, setMe] = useState<string | null | undefined>(undefined);
  useEffect(() => {
    void getMe().then((u) => setMe(u?.email ?? null));
  }, []);
  const conversation = threads.find((t) => !t.path);
  const away = threads.filter((t) => t.path && (t.outdated || t.line === 0));
  const open = threads.filter((t) => t.path && !t.resolved).length;
  return (
    <div className="card settings-form" style={{ marginTop: 16 }}>
      <h2>
        Conversation{" "}
        <span className="muted">
          · {open} open thread{open === 1 ? "" : "s"}
          {!capabilities.threads && " · kept in Forge (this host has no comments)"}
        </span>
      </h2>
      {conversation && (
        <Thread changeId={change.id} thread={conversation} canComment={false} needAuthor={me === null} onChanged={onChanged} />
      )}
      {away.map((t) => (
        <Thread
          key={t.id}
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
      ))}
      {permissions.can_comment && (
        <Composer
          placeholder="Comment on this change…"
          needAuthor={me === null}
          onSubmit={async (body, author) => {
            await createThread(change.id, { body, author });
            onChanged();
          }}
        />
      )}
    </div>
  );
}
