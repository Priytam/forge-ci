import { useEffect, useRef, useState } from "react";
import { dequeueChange, getMe, mergeChange, queueChange, reviewChange, type ChangePage } from "../../api";
import Icon from "../Icon";
import Menu, { type MenuItem } from "../ui/Menu";

const METHOD_LABELS: Record<string, string> = {
  merge: "Merge commit",
  squash: "Squash",
  rebase: "Rebase",
  fast_forward: "Fast-forward",
};

/**
 * The change's actions, top right: Review (a small form, not always-open
 * inputs), the one main button — Merge, or Add to merge queue — and the ⋯
 * menu for the rest. What shows depends on the host's capabilities and the
 * caller's permissions, never on which host it is.
 */
export default function ChangeActions({
  page,
  onChanged,
  onMessage,
  onEdit,
  onState,
}: {
  page: ChangePage;
  onChanged: () => void;
  onMessage: (m: { ok: boolean; text: string }) => void;
  onEdit: () => void;
  onState: (s: "open" | "closed") => void;
}) {
  const { change, capabilities: caps, gate, actions, permissions, queue } = page;
  const [method, setMethod] = useState(caps.merge_methods[0] ?? "");
  const [busy, setBusy] = useState(false);
  const open = change.state === "open";
  const merge = actions.find((a) => a.kind === "merge");
  const merging = merge?.state === "pending";
  const entry = queue?.entry;
  const inQueue = !!entry && ["queued", "testing", "merging"].includes(entry.state);
  const gateOK = !gate || gate.state === "pass";
  const queueReady =
    !!gate && gate.rules.every((r) => r.id === "merge_queue" || r.id === "freeze" || r.state === "pass");

  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    try {
      await fn();
      onMessage({ ok: true, text: ok });
      onChanged();
    } catch (e) {
      onMessage({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };

  const more: MenuItem[] = [];
  if (permissions.can_edit && change.state !== "merged")
    more.push({ label: "Edit title and description", icon: "edit", onSelect: onEdit });
  if (permissions.can_edit && open)
    more.push({ label: "Close change", icon: "closed", danger: true, onSelect: () => onState("closed") });
  if (permissions.can_edit && change.state === "closed")
    more.push({ label: "Reopen change", icon: "changes", onSelect: () => onState("open") });
  if (change.url)
    more.push({
      label: "Open on the host",
      icon: "external",
      onSelect: () => window.open(change.url, "_blank", "noopener"),
    });
  more.push({
    label: "Copy branch name",
    icon: "copy",
    onSelect: () =>
      void navigator.clipboard
        ?.writeText(change.source_branch)
        .then(() => onMessage({ ok: true, text: "Branch name copied." })),
  });

  const canMerge = open && caps.merge_methods.length > 0 && permissions.can_merge;
  const methodPicker = caps.merge_methods.length > 1 && (
    <select value={method} onChange={(e) => setMethod(e.target.value)} disabled={busy} aria-label="Merge method">
      {caps.merge_methods.map((m) => (
        <option key={m} value={m}>
          {METHOD_LABELS[m] ?? m}
        </option>
      ))}
    </select>
  );

  return (
    <div className="cr-actions">
      {open && permissions.can_review && <ReviewButton page={page} onChanged={onChanged} onMessage={onMessage} />}
      {canMerge && queue?.enabled && inQueue && (
        <button
          className="btn cr-big-btn"
          disabled={busy}
          onClick={() => void run(() => dequeueChange(change.id), "Removed from the merge queue.")}
        >
          Remove from queue
        </button>
      )}
      {canMerge && queue?.enabled && !inQueue && (
        <span className="cr-split">
          {methodPicker}
          <button
            className="btn btn-primary cr-big-btn"
            disabled={busy || !queueReady || change.draft}
            title={
              !queueReady
                ? gate?.summary
                : change.draft
                  ? "The change is a draft"
                  : "Test it on top of the latest target, then merge"
            }
            onClick={() => void run(() => queueChange(change.id, change.head_sha, method), "Added to the merge queue.")}
          >
            <Icon name="layers" size={14} /> Add to merge queue
          </button>
        </span>
      )}
      {canMerge && !queue?.enabled && (
        <span className="cr-split">
          {!merging && methodPicker}
          <button
            className="btn btn-primary cr-big-btn"
            disabled={busy || merging || !gateOK || change.draft}
            title={!gateOK ? gate?.summary : change.draft ? "The change is a draft" : undefined}
            onClick={() => void run(() => mergeChange(change.id, change.head_sha, method), "Merge queued.")}
          >
            <Icon name="merge" size={14} /> {merging ? "Merging…" : "Merge"}
          </button>
        </span>
      )}
      <Menu items={more} label="More actions" />
    </div>
  );
}

/** Review: approve or request changes, with an optional note. */
function ReviewButton({
  page,
  onChanged,
  onMessage,
}: {
  page: ChangePage;
  onChanged: () => void;
  onMessage: (m: { ok: boolean; text: string }) => void;
}) {
  const { change } = page;
  const [open, setOpen] = useState(false);
  const [me, setMe] = useState<string | null | undefined>(undefined);
  const [reviewer, setReviewer] = useState("");
  const [verdict, setVerdict] = useState<"approve" | "request_changes">("approve");
  const [body, setBody] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    void getMe().then((u) => setMe(u?.email ?? null));
  }, []);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => root.current && !root.current.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  const who = (me ?? reviewer).toLowerCase();
  const mine = change.reviews.find((r) => r.source === "forge" && r.reviewer === who && r.verdict !== "dismissed");

  const send = async (v: "approve" | "request_changes" | "dismiss") => {
    if (me === null && !reviewer.trim()) {
      setErr("Enter your email first (sign-in is off).");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await reviewChange(change.id, v, v === "dismiss" ? "" : body, me ? undefined : reviewer);
      onMessage({
        ok: true,
        text: v === "approve" ? "Approved." : v === "dismiss" ? "Your review was withdrawn." : "Changes requested.",
      });
      setBody("");
      setOpen(false);
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="cr-review" ref={root}>
      <button className="btn cr-big-btn" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen(!open)}>
        Review <Icon name="chevron" size={12} />
      </button>
      {open && (
        <div className="popover cr-review-pop" role="dialog" aria-label="Review this change">
          {me === null && (
            <input
              value={reviewer}
              onChange={(e) => setReviewer(e.target.value)}
              placeholder="you@company.com (sign-in is off)"
            />
          )}
          <textarea
            rows={3}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            placeholder="Leave a note (optional)"
          />
          {(["approve", "request_changes"] as const).map((v) => (
            <label key={v} className={verdict === v ? "cr-choice cr-choice-on" : "cr-choice"}>
              <input type="radio" name="verdict" checked={verdict === v} onChange={() => setVerdict(v)} />
              <span>
                {v === "approve" ? "Approve" : "Request changes"}
                <small>
                  {v === "approve"
                    ? "Counts towards the approvals the merge needs."
                    : "Blocks the merge until you approve or withdraw."}
                </small>
              </span>
            </label>
          ))}
          {err && <div className="error-text">{err}</div>}
          <div className="form-actions">
            <button className="btn btn-primary" disabled={busy} onClick={() => void send(verdict)}>
              Submit review
            </button>
            {mine && (
              <button className="btn" disabled={busy} onClick={() => void send("dismiss")}>
                Withdraw mine
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
