import Skeleton from "../components/ui/Skeleton";
import { useCallback, useEffect, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { editChange, getChange, relativeTime, shortSha, type ChangePage } from "../api";
import Avatar, { displayName } from "../components/Avatar";
import Icon, { type IconName } from "../components/Icon";
import DiffView from "../components/DiffView";
import Conversation from "../components/Conversation";
import RepoHeader from "../components/RepoHeader";
import ChangeActions from "../components/review/ChangeActions";
import ChangeSidebar from "../components/review/ChangeSidebar";
import { CIPill, ChangeStatePill, RuleMark, StatusBar, gateStatus } from "../components/review/ui";
import type { Tone } from "../components/ui/Pill";
import { useToast } from "../components/ui/Toast";
import { useChangeStream } from "../hooks/useChangeStream";

type Tab = "conversation" | "files" | "checks" | "revisions";
const TAB_IDS: Tab[] = ["conversation", "files", "checks", "revisions"];

/**
 * One change, under its repo's Changes tab: what it is and what it waits
 * for at the top, its conversation, files, checks and revisions in tabs,
 * and the merge checklist beside them. Rendered from Forge's copy; what the
 * page offers depends on the host's capabilities, never on which host it is.
 */
export default function ChangeDetail() {
  const { id } = useParams();
  const [params, setParams] = useSearchParams();
  const asked = params.get("tab") as Tab;
  const tab: Tab = TAB_IDS.includes(asked) ? asked : "conversation";
  const [page, setPage] = useState<ChangePage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<{ title: string; body: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  const load = useCallback(() => {
    if (!id) return;
    getChange(id)
      .then((r) => {
        setPage(r);
        setError(null);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [id]);
  useEffect(load, [load]);
  useChangeStream(load, id ? Number(id) : undefined);

  if (error) return <div className="error-banner">{error}</div>;
  if (!page) return <Skeleton />;
  const { change, capabilities: caps, gate, actions, queue } = page;

  const save = async (patch: Parameters<typeof editChange>[1], done: string) => {
    setBusy(true);
    try {
      await editChange(change.id, patch);
      setEditing(null);
      toast(done, "success");
      load();
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e), "danger");
    } finally {
      setBusy(false);
    }
  };
  const setTab = (t: Tab) => {
    const next = new URLSearchParams(params);
    if (t === "conversation") next.delete("tab");
    else next.set("tab", t);
    setParams(next, { replace: true });
  };

  // The status bar: what is true about this change right now.
  const merge = actions.find((a) => a.kind === "merge");
  const entry = queue?.entry;
  const inQueue = !!entry && ["queued", "testing", "merging"].includes(entry.state);
  let status: {
    tone: Tone;
    icon: IconName;
    title: string;
    detail?: string;
    progress?: { done: number; total: number };
  };
  if (change.state === "merged") {
    const sha = merge?.result?.merge_sha;
    status = {
      tone: "accent",
      icon: "merge",
      title: `Merged into ${change.target_branch}`,
      detail: sha ? `as ${shortSha(sha)}` : undefined,
    };
  } else if (change.state === "closed") {
    status = { tone: "neutral", icon: "closed", title: "Closed without merging" };
  } else if (merge?.state === "failed") {
    status = { tone: "danger", icon: "blocked", title: "The merge failed", detail: merge.last_error };
  } else if (merge?.state === "pending") {
    status = {
      tone: "accent",
      icon: "merge",
      title: "Merging…",
      detail: "Forge is merging exactly the head that was approved.",
    };
  } else if (entry?.state === "failed" && !inQueue) {
    status = { tone: "danger", icon: "layers", title: "Left the merge queue", detail: entry.error };
  } else if (change.draft) {
    status = {
      tone: "neutral",
      icon: "changes",
      title: "Draft",
      detail: "It can't merge until it's marked ready for review.",
    };
  } else {
    status = gateStatus(gate, change.target_branch, inQueue);
  }

  const comments = page.threads.reduce((n, t) => n + t.comments.length, 0);
  const tabs: { id: Tab; label: string; icon: IconName; n?: number; hidden?: boolean }[] = [
    { id: "conversation", label: "Conversation", icon: "comment", n: comments },
    { id: "files", label: "Files", icon: "file", hidden: !caps.diffs },
    { id: "checks", label: "Checks", icon: "check" },
    { id: "revisions", label: "Revisions", icon: "upgrade", n: change.revision_list.length },
  ];

  return (
    <div className="cr-page">
      <RepoHeader repo={change.repo} tab="changes" />

      <div className="cr-head">
        <div className="cr-head-main">
          {editing ? (
            <input
              className="cr-title-input"
              value={editing.title}
              autoFocus
              onChange={(e) => setEditing({ ...editing, title: e.target.value })}
              aria-label="Title"
            />
          ) : (
            <h1 className="cr-title">
              {change.title || "(untitled)"} <span className="cr-num">#{change.number}</span>
            </h1>
          )}
          <div className="cr-meta">
            <ChangeStatePill change={change} />
            <Avatar identity={change.author} size={18} />
            <b>{displayName(change.author)}</b>
            <span>
              {change.state === "merged" ? "merged" : change.state === "closed" ? "wanted to merge" : "wants to merge"}
            </span>
            <span className="cr-ref">{change.source_branch}</span>
            <Icon name="expand" size={12} />
            <span className="cr-ref">{change.target_branch}</span>
            {change.updated_at && <span>· updated {relativeTime(change.updated_at)}</span>}
          </div>
        </div>
        <ChangeActions
          page={page}
          onChanged={load}
          onMessage={(m) => toast(m.text, m.ok ? "success" : "danger")}
          onEdit={() => setEditing({ title: change.title, body: change.body })}
          onState={(s) => void save({ state: s }, s === "closed" ? "Change closed." : "Change reopened.")}
        />
      </div>

      {editing && (
        <div className="cr-card" style={{ marginTop: 12 }}>
          <div className="cr-card-head">
            <b>Edit</b> the title above and the description here
          </div>
          <div style={{ padding: 12 }}>
            <textarea
              rows={6}
              style={{ width: "100%" }}
              value={editing.body}
              onChange={(e) => setEditing({ ...editing, body: e.target.value })}
              placeholder="What the change does, and why"
            />
            <div className="form-actions">
              <button
                className="btn btn-primary"
                disabled={busy || !editing.title.trim()}
                onClick={() => void save({ title: editing.title, body: editing.body }, "Saved.")}
              >
                Save
              </button>
              <button className="btn" disabled={busy} onClick={() => setEditing(null)}>
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}

      {change.refresh_error && (
        <div className="notice-line" style={{ marginTop: 12 }}>
          <Icon name="alert" size={14} /> The last sync with the host failed: {change.refresh_error}. Showing the last
          good copy.
        </div>
      )}

      <StatusBar {...status} />

      <nav className="cr-tabs" role="tablist" aria-label="Change">
        {tabs
          .filter((t) => !t.hidden)
          .map((t) => (
            <button
              key={t.id}
              role="tab"
              aria-selected={tab === t.id}
              className={tab === t.id ? "cr-tab cr-tab-on" : "cr-tab"}
              onClick={() => setTab(t.id)}
            >
              <Icon name={t.icon} size={14} />
              {t.label}
              {t.n !== undefined && t.n > 0 && <span className="num">{t.n}</span>}
            </button>
          ))}
      </nav>

      {tab === "files" && caps.diffs ? (
        <DiffView page={page} onChanged={load} />
      ) : (
        <div className="cr-layout">
          <div>
            {tab === "conversation" && <Conversation page={page} onChanged={load} />}
            {tab === "checks" && <Checks page={page} />}
            {tab === "revisions" && <Revisions page={page} onOpen={() => setTab("files")} />}
          </div>
          <ChangeSidebar page={page} />
        </div>
      )}
    </div>
  );
}

/** The Forge pipeline on the head, and every merge rule in full. */
function Checks({ page }: { page: ChangePage }) {
  const { change, gate } = page;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div className="cr-card">
        <div className="cr-card-head">
          <b>Forge pipeline</b> on <span className="mono">{shortSha(change.head_sha)}</span>
        </div>
        <div style={{ padding: 12, display: "flex", alignItems: "center", gap: 10 }}>
          {change.pipeline_id ? (
            <>
              <CIPill status={change.pipeline_status} />
              <Link to={`/pipelines/${change.pipeline_id}`}>Open pipeline #{change.pipeline_id}</Link>
            </>
          ) : (
            <span className="muted">
              No pipeline has run on this commit yet. A push to {change.source_branch} starts one.
            </span>
          )}
        </div>
      </div>
      <div className="cr-card">
        <div className="cr-card-head">
          <b>Merge rules</b> for {change.target_branch}
        </div>
        <div style={{ padding: "6px 12px" }}>
          {!gate && <p className="muted">No merge rules apply: it merges as the host allows.</p>}
          {gate?.rules.map((r) => (
            <div key={r.id} className="cr-side-row" style={{ padding: "6px 0" }}>
              <RuleMark state={r.state} />
              <span className="cr-rule-label">
                <b>{r.label}</b>
                <span className="cr-rule-detail">{r.detail}</span>
              </span>
            </div>
          ))}
          {gate && page.capabilities.status_checks && (
            <p className="muted" style={{ fontSize: "var(--fs-sm)" }}>
              {gate.published_state === gate.state
                ? `The host shows this as the forge/merge-gate check (updated ${relativeTime(gate.published_at ?? gate.evaluated_at)}).`
                : "Publishing this result to the host…"}
            </p>
          )}
        </div>
      </div>
    </div>
  );
}

/** Every push, newest first. */
function Revisions({ page, onOpen }: { page: ChangePage; onOpen: () => void }) {
  const { change } = page;
  return (
    <div className="cr-card">
      <div className="cr-card-head">
        <b>
          {change.revision_list.length} revision{change.revision_list.length === 1 ? "" : "s"}
        </b>{" "}
        · every push is one, on every host
      </div>
      {change.revision_list.map((r) => (
        <div
          key={r.number}
          className="cr-side-row"
          style={{ padding: "8px 12px", borderTop: "1px solid var(--border)" }}
        >
          <span className="badge2">r{r.number}</span>
          <span className="mono">{shortSha(r.head_sha)}</span>
          <span className="cr-rule-label muted">
            on {change.target_branch} {shortSha(r.base_sha)}
          </span>
          <span className="muted">{relativeTime(r.pushed_at)}</span>
        </div>
      ))}
      {page.capabilities.diffs && change.revision_list.length > 1 && (
        <div style={{ padding: "8px 12px", borderTop: "1px solid var(--border)" }}>
          <button className="link-btn" onClick={onOpen}>
            Compare revisions in Files →
          </button>
        </div>
      )}
    </div>
  );
}
