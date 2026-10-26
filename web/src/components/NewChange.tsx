import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { createChange, getProviderCapabilities, listRegistry, type RegisteredRepo } from "../api";

/**
 * Open a change on a repo whose host has no pull requests (plain git): Forge
 * keeps the change, reads its two branches, and a runner merges it. Repos on
 * hosts with pull requests are not offered — their changes come from the
 * host by themselves.
 */
export default function NewChange({ repo, onClose }: { repo?: string; onClose: () => void }) {
  const navigate = useNavigate();
  const [repos, setRepos] = useState<RegisteredRepo[] | null>(null);
  const [form, setForm] = useState({ repo: "", source_branch: "", target_branch: "", title: "", body: "", draft: false });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    Promise.all([listRegistry(), getProviderCapabilities()])
      .then(([all, caps]) => {
        const native = all.filter((r) => caps[r.provider]?.native_changes && (!repo || r.repo === repo));
        setRepos(native);
        if (native.length > 0) {
          setForm((f) => ({ ...f, repo: native[0].repo, target_branch: native[0].default_branch || "main" }));
        }
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [repo]);

  const set = (k: keyof typeof form, v: string | boolean) => setForm((f) => ({ ...f, [k]: v }));
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const { id } = await createChange(form);
      navigate(`/changes/${id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <div className="card settings-form" style={{ marginBottom: 12 }}>
      <h2>New change</h2>
      {repos === null && !error && <p className="muted">Loading…</p>}
      {repos !== null && repos.length === 0 && (
        <p className="muted">
          {repo ? `${repo}'s host has its own pull requests: open the change there and it appears here by itself.` : ""}
          {!repo &&
            "Changes are opened here only for repos on plain git servers. Pull and merge requests on GitHub, GitLab, Bitbucket, Gitea and CodeCommit appear in this list by themselves."}
        </p>
      )}
      {repos !== null && repos.length > 0 && (
        <>
          <label className="field">
            <span>Repo</span>
            <select
              value={form.repo}
              onChange={(e) => {
                const r = repos.find((x) => x.repo === e.target.value);
                setForm((f) => ({ ...f, repo: e.target.value, target_branch: r?.default_branch || f.target_branch }));
              }}
            >
              {repos.map((r) => (
                <option key={r.repo} value={r.repo}>
                  {r.repo}
                </option>
              ))}
            </select>
          </label>
          <div className="filter-bar">
            <label className="field">
              <span>Merge from branch</span>
              <input value={form.source_branch} onChange={(e) => set("source_branch", e.target.value)} placeholder="feature/seat-lock" />
            </label>
            <label className="field">
              <span>Into</span>
              <input value={form.target_branch} onChange={(e) => set("target_branch", e.target.value)} placeholder="main" />
            </label>
          </div>
          <label className="field">
            <span>Title</span>
            <input value={form.title} onChange={(e) => set("title", e.target.value)} placeholder="What the change does" />
          </label>
          <label className="field">
            <span>Description</span>
            <textarea rows={5} value={form.body} onChange={(e) => set("body", e.target.value)} />
          </label>
          <label className="field-inline">
            <input type="checkbox" checked={form.draft} onChange={(e) => set("draft", e.target.checked)} /> Draft (not ready for
            review)
          </label>
        </>
      )}
      {error && <div className="error-banner">{error}</div>}
      <div className="form-actions">
        {repos !== null && repos.length > 0 && (
          <button
            className="btn btn-primary"
            disabled={busy || !form.title.trim() || !form.source_branch.trim() || !form.target_branch.trim()}
            onClick={() => void submit()}
          >
            Open change
          </button>
        )}
        <button className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </div>
  );
}
