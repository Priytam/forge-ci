import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import { Link, useParams } from "react-router-dom";
import {
  addMember,
  artifactDownloadUrl,
  createSchedule,
  createVariable,
  decodeRepoParam,
  deleteMember,
  deleteSchedule,
  deleteVariable,
  getIsAdmin,
  getRepoConfig,
  getRepoSettings,
  humanSize,
  listArtifacts,
  listConfigVersions,
  getRepoTemplate,
  listMembers,
  listProtectedEnvironments,
  listRepoTemplates,
  listRunners,
  listSchedules,
  listVariables,
  putRepoConfig,
  putRepoSettings,
  putRepoTemplate,
  relativeTime,
  revertConfig,
  updateSchedule,
  updateVariable,
  upsertProtectedEnvironment,
  type Schedule,
  type Variable,
} from "../api";
import { usePoll } from "../hooks/usePoll";

const MASKED_PLACEHOLDER = "********************";
const ROLES = ["admin", "owner", "developer"];

function Section({
  title,
  description,
  children,
}: {
  title: string;
  description: ReactNode;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  return (
    <section className="card settings-section">
      <div className="settings-head">
        <div className="settings-head-text">
          <h2>{title}</h2>
          <div className="muted settings-desc">{description}</div>
        </div>
        <button type="button" className="btn" onClick={() => setOpen((o) => !o)}>
          {open ? "Collapse" : "Expand"}
        </button>
      </div>
      {open && <div className="settings-body">{children}</div>}
    </section>
  );
}

function BoolMark({ on }: { on: boolean }) {
  return <span className={on ? "bool-yes" : "bool-no"}>{on ? "✓" : "✕"}</span>;
}

/* ---------------- Variables ---------------- */

function VariablesSection({ repo }: { repo: string }) {
  const [reveal, setReveal] = useState(false);
  const fetcher = useCallback(() => listVariables(repo, reveal), [repo, reveal]);
  const { data: vars, error, refresh } = usePoll(fetcher, 0, false);

  const [editing, setEditing] = useState<Variable | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [key, setKey] = useState("");
  const [value, setValue] = useState("");
  const [scope, setScope] = useState("*");
  const [isProtected, setIsProtected] = useState(false);
  const [isMasked, setIsMasked] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const resetForm = () => {
    setEditing(null);
    setShowForm(false);
    setKey("");
    setValue("");
    setScope("*");
    setIsProtected(false);
    setIsMasked(false);
    setFormError(null);
  };

  const startEdit = (v: Variable) => {
    setEditing(v);
    setShowForm(true);
    setKey(v.key);
    setValue(v.value);
    setScope(v.environment_scope);
    setIsProtected(v.protected);
    setIsMasked(v.masked);
    setFormError(null);
  };

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFormError(null);
    try {
      if (editing) {
        await updateVariable(editing.id, {
          value,
          protected: isProtected,
          masked: isMasked,
          environment_scope: scope,
        });
      } else {
        await createVariable(repo, {
          key,
          value,
          protected: isProtected,
          masked: isMasked,
          environment_scope: scope,
        });
      }
      resetForm();
      refresh();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const onDelete = async (v: Variable) => {
    if (!window.confirm(`Delete variable "${v.key}"?`)) return;
    try {
      await deleteVariable(v.id);
      refresh();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}

      <div className="section-toolbar">
        <button
          type="button"
          className="btn"
          onClick={() => {
            setReveal((r) => !r);
            refresh();
          }}
        >
          {reveal ? "Hide values" : "Reveal values"}
        </button>
        {!showForm && (
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              resetForm();
              setShowForm(true);
            }}
          >
            Add Variable
          </button>
        )}
      </div>

      {vars && vars.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Key</th>
                <th>Value</th>
                <th>Protected</th>
                <th>Masked</th>
                <th>Environment scope</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {vars.map((v) => (
                <tr key={v.id}>
                  <td className="mono">{v.key}</td>
                  <td className="mono var-value">
                    {v.masked && !reveal ? MASKED_PLACEHOLDER : v.value}
                  </td>
                  <td>
                    <BoolMark on={v.protected} />
                  </td>
                  <td>
                    <BoolMark on={v.masked} />
                  </td>
                  <td className="mono">{v.environment_scope}</td>
                  <td className="actions-cell">
                    <button
                      type="button"
                      className="btn btn-icon"
                      title="Edit"
                      onClick={() => startEdit(v)}
                    >
                      ✎
                    </button>
                    <button
                      type="button"
                      className="btn btn-icon"
                      title="Delete"
                      onClick={() => void onDelete(v)}
                    >
                      🗑
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No variables yet.</div>
      )}

      {showForm && (
        <form className="settings-form" onSubmit={(e) => void onSubmit(e)}>
          <h3>{editing ? `Edit variable: ${editing.key}` : "Add variable"}</h3>
          <div className="form-row">
            <label className="field">
              <span>Key</span>
              <input
                className="mono"
                value={key}
                onChange={(e) => setKey(e.target.value)}
                required
                disabled={Boolean(editing)}
              />
            </label>
            <label className="field">
              <span>Environment scope</span>
              <input
                className="mono"
                value={scope}
                onChange={(e) => setScope(e.target.value)}
              />
            </label>
          </div>
          <label className="field">
            <span>Value</span>
            <textarea
              rows={1}
              className="mono"
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          </label>
          <label className="check-row">
            <input
              type="checkbox"
              checked={isProtected}
              onChange={(e) => setIsProtected(e.target.checked)}
            />
            <span>
              Protected{" "}
              <span className="muted">
                — only exposed to protected refs like main
              </span>
            </span>
          </label>
          <label className="check-row">
            <input
              type="checkbox"
              checked={isMasked}
              onChange={(e) => setIsMasked(e.target.checked)}
            />
            <span>
              Masked{" "}
              <span className="muted">
                — hidden in job logs; min 8 chars, no whitespace
              </span>
            </span>
          </label>
          {formError && <div className="error-banner">{formError}</div>}
          <div className="form-actions">
            <button type="button" className="btn" onClick={resetForm}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={busy}>
              {editing ? "Save changes" : "Add variable"}
            </button>
          </div>
        </form>
      )}
    </div>
  );
}

/* ---------------- Runner tags ---------------- */

function RunnerTagsSection({ repo }: { repo: string }) {
  const settingsFetcher = useCallback(() => getRepoSettings(repo), [repo]);
  const { data, error, loading } = usePoll(settingsFetcher, 0, false);

  const { data: runners } = usePoll(listRunners, 0, false);

  const [tags, setTags] = useState<string[]>([]);
  const [initialized, setInitialized] = useState(false);
  const [input, setInput] = useState("");
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!initialized && !loading && data) {
      setTags(data.default_runner_tags ?? []);
      setInitialized(true);
    }
  }, [data, loading, initialized]);

  const fleetTags = useMemo(() => {
    const set = new Set<string>();
    for (const r of runners ?? []) {
      for (const t of r.tags ?? []) set.add(t);
    }
    return [...set].sort();
  }, [runners]);

  const addTag = (raw: string) => {
    const parts = raw
      .split(",")
      .map((t) => t.trim())
      .filter(Boolean);
    if (parts.length === 0) return;
    setTags((prev) => {
      const next = [...prev];
      for (const p of parts) {
        if (!next.includes(p)) next.push(p);
      }
      return next;
    });
    setSaved(false);
  };

  const removeTag = (tag: string) => {
    setTags((prev) => prev.filter((t) => t !== tag));
    setSaved(false);
  };

  const onSave = async () => {
    setBusy(true);
    setSaveError(null);
    setSaved(false);
    try {
      await putRepoSettings(repo, tags);
      setSaved(true);
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}

      <div className="tag-editor">
        {tags.length > 0 ? (
          tags.map((t) => (
            <span key={t} className="tag-edit-chip">
              <span className="mono">{t}</span>
              <button
                type="button"
                className="tag-remove"
                title={`Remove ${t}`}
                onClick={() => removeTag(t)}
              >
                ✕
              </button>
            </span>
          ))
        ) : (
          <span className="muted">No tags — jobs run on any runner.</span>
        )}
      </div>

      <div className="tag-input-row">
        <input
          className="mono"
          placeholder="Add tags (comma or Enter)"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === ",") {
              e.preventDefault();
              addTag(input);
              setInput("");
            }
          }}
        />
        <button
          type="button"
          className="btn btn-primary"
          disabled={busy}
          onClick={() => {
            if (input.trim()) {
              addTag(input);
              setInput("");
            }
            void onSave();
          }}
        >
          Save
        </button>
      </div>

      {fleetTags.length > 0 && (
        <div className="tag-suggestions">
          <span className="muted">Tags in the fleet:</span>
          {fleetTags.map((t) => (
            <button
              key={t}
              type="button"
              className="tag-suggestion"
              disabled={tags.includes(t)}
              onClick={() => addTag(t)}
            >
              {t}
            </button>
          ))}
        </div>
      )}

      {saveError && <div className="error-banner">{saveError}</div>}
      {saved && <div className="saved-note">Runner tags saved.</div>}
    </div>
  );
}

/* ---------------- Artifacts ---------------- */

function ArtifactsSection({ repo }: { repo: string }) {
  const fetcher = useCallback(() => listArtifacts(repo), [repo]);
  const { data: artifacts, error } = usePoll(fetcher, 0, false);

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}
      {artifacts && artifacts.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Pipeline</th>
                <th>Job</th>
                <th>Artifact</th>
                <th>Size</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {artifacts.map((a) => (
                <tr key={a.id}>
                  <td>
                    <Link to={`/pipelines/${a.pipeline_id}`} className="mono">
                      #{a.pipeline_id}
                    </Link>
                  </td>
                  <td>{a.job_name}</td>
                  <td className="mono">{a.name}</td>
                  <td>{humanSize(a.size_bytes)}</td>
                  <td className="muted">{relativeTime(a.created_at)}</td>
                  <td>
                    <a className="btn" href={artifactDownloadUrl(a.id)}>
                      Download
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No artifacts for this repo yet.</div>
      )}
    </div>
  );
}

/* ---------------- Members & approval rules ---------------- */

function MembersSection({ repo }: { repo: string }) {
  const membersFetcher = useCallback(() => listMembers(repo), [repo]);
  const {
    data: members,
    error: membersError,
    refresh: refreshMembers,
  } = usePoll(membersFetcher, 0, false);

  const envsFetcher = useCallback(() => listProtectedEnvironments(repo), [repo]);
  const {
    data: envs,
    error: envsError,
    refresh: refreshEnvs,
  } = usePoll(envsFetcher, 0, false);

  // add-member form
  const [username, setUsername] = useState("");
  const [role, setRole] = useState("developer");
  const [memberError, setMemberError] = useState<string | null>(null);

  const onAddMember = async (e: FormEvent) => {
    e.preventDefault();
    setMemberError(null);
    try {
      await addMember(repo, username.trim(), role);
      setUsername("");
      refreshMembers();
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : String(err));
    }
  };

  const onDeleteMember = async (id: number, name: string) => {
    if (!window.confirm(`Remove member "${name}"?`)) return;
    setMemberError(null);
    try {
      await deleteMember(id);
      refreshMembers();
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : String(err));
    }
  };

  // approval-rule form
  const [envName, setEnvName] = useState("production");
  const [requiredApprovals, setRequiredApprovals] = useState(1);
  const [timeoutHours, setTimeoutHours] = useState(24);
  const [approverRoles, setApproverRoles] = useState<string[]>([
    "admin",
    "owner",
  ]);
  const [allowSelf, setAllowSelf] = useState(false);
  const [ruleError, setRuleError] = useState<string | null>(null);
  const [ruleSaved, setRuleSaved] = useState(false);

  const toggleRole = (r: string) => {
    setApproverRoles((prev) =>
      prev.includes(r) ? prev.filter((x) => x !== r) : [...prev, r]
    );
  };

  const onSaveRule = async (e: FormEvent) => {
    e.preventDefault();
    setRuleError(null);
    setRuleSaved(false);
    try {
      await upsertProtectedEnvironment(repo, {
        name: envName.trim(),
        required_approvals: requiredApprovals,
        approval_timeout_hours: timeoutHours,
        approver_roles: approverRoles,
        allow_self_approval: allowSelf,
      });
      setRuleSaved(true);
      refreshEnvs();
    } catch (err) {
      setRuleError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      <h3>Members</h3>
      {membersError && <div className="error-banner">{membersError}</div>}
      {members && members.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Username</th>
                <th>Role</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.id}>
                  <td>{m.username}</td>
                  <td>
                    <span className="ref-tag">{m.role}</span>
                  </td>
                  <td>
                    <button
                      type="button"
                      className="btn btn-icon"
                      title="Remove"
                      onClick={() => void onDeleteMember(m.id, m.username)}
                    >
                      🗑
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No members.</div>
      )}
      <div className="muted small-note">
        Repos with no members allow anyone to approve (bootstrap mode).
      </div>

      <form className="settings-form inline-form" onSubmit={(e) => void onAddMember(e)}>
        <label className="field">
          <span>Username</span>
          <input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
          />
        </label>
        <label className="field">
          <span>Role</span>
          <select value={role} onChange={(e) => setRole(e.target.value)}>
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </label>
        <button type="submit" className="btn btn-primary">
          Add member
        </button>
      </form>
      {memberError && <div className="error-banner">{memberError}</div>}

      <h3 className="subsection-title">Approval rules</h3>
      {envsError && <div className="error-banner">{envsError}</div>}
      {envs && envs.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Environment</th>
                <th>Required approvals</th>
                <th>Timeout (h)</th>
                <th>Approver roles</th>
                <th>Self-approval</th>
              </tr>
            </thead>
            <tbody>
              {envs.map((env) => (
                <tr key={env.id} className={env.repo === "" ? "row-global" : ""}>
                  <td>
                    <span className="mono">{env.name}</span>{" "}
                    {env.repo === "" && (
                      <span className="global-chip">global default</span>
                    )}
                  </td>
                  <td>{env.required_approvals}</td>
                  <td>{env.approval_timeout_hours}</td>
                  <td>
                    <span className="tag-chips">
                      {env.approver_roles.map((r) => (
                        <span key={r} className="ref-tag">
                          {r}
                        </span>
                      ))}
                    </span>
                  </td>
                  <td>
                    <BoolMark on={env.allow_self_approval} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No approval rules.</div>
      )}

      <form className="settings-form" onSubmit={(e) => void onSaveRule(e)}>
        <h3>Add / update repo rule</h3>
        <div className="form-row">
          <label className="field">
            <span>Environment name</span>
            <input
              className="mono"
              value={envName}
              onChange={(e) => setEnvName(e.target.value)}
              required
            />
          </label>
          <label className="field">
            <span>Required approvals</span>
            <input
              type="number"
              min={1}
              value={requiredApprovals}
              onChange={(e) => setRequiredApprovals(Number(e.target.value))}
            />
          </label>
          <label className="field">
            <span>Approval timeout (hours)</span>
            <input
              type="number"
              min={1}
              value={timeoutHours}
              onChange={(e) => setTimeoutHours(Number(e.target.value))}
            />
          </label>
        </div>
        <div className="field">
          <span>Approver roles</span>
          <div className="check-inline">
            {ROLES.map((r) => (
              <label key={r} className="check-row">
                <input
                  type="checkbox"
                  checked={approverRoles.includes(r)}
                  onChange={() => toggleRole(r)}
                />
                <span>{r}</span>
              </label>
            ))}
          </div>
        </div>
        <label className="check-row">
          <input
            type="checkbox"
            checked={allowSelf}
            onChange={(e) => setAllowSelf(e.target.checked)}
          />
          <span>Allow self-approval</span>
        </label>
        {ruleError && <div className="error-banner">{ruleError}</div>}
        {ruleSaved && <div className="saved-note">Rule saved.</div>}
        <div className="form-actions">
          <button type="submit" className="btn btn-primary">
            Save rule
          </button>
        </div>
      </form>
    </div>
  );
}

/* ---------------- Pipeline config (versioned) ---------------- */

function ConfigSection({ repo }: { repo: string }) {
  const fetcher = useCallback(() => getRepoConfig(repo), [repo]);
  const { data, error, loading, refresh } = usePoll(fetcher, 0, false);

  const versionsFetcher = useCallback(() => listConfigVersions(repo), [repo]);
  const { data: versions, refresh: refreshVersions } = usePoll(
    versionsFetcher,
    0,
    false
  );

  const [text, setText] = useState("");
  const [initialized, setInitialized] = useState(false);
  const [author, setAuthor] = useState(
    () => window.localStorage.getItem("forge-author") ?? ""
  );
  const [message, setMessage] = useState("");
  const [viewing, setViewing] = useState<number | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [pushedVersion, setPushedVersion] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);

  const currentVersion = data?.version ?? null;

  useEffect(() => {
    if (!initialized && !loading) {
      setText(data?.config ?? "");
      setInitialized(true);
    }
  }, [data, loading, initialized]);

  const setAuthorPersist = (value: string) => {
    setAuthor(value);
    try {
      window.localStorage.setItem("forge-author", value);
    } catch {
      // storage unavailable — non-fatal
    }
  };

  const onPush = async () => {
    setBusy(true);
    setActionError(null);
    setPushedVersion(null);
    try {
      const res = await putRepoConfig(repo, text, author.trim(), message.trim());
      setPushedVersion(res.version);
      setMessage("");
      refresh();
      refreshVersions();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const onView = async (version: number) => {
    setActionError(null);
    setPushedVersion(null);
    try {
      const rc = await getRepoConfig(repo, version);
      if (rc) {
        setText(rc.config);
        setViewing(version);
      }
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  const backToCurrent = () => {
    setText(data?.config ?? "");
    setViewing(null);
  };

  const onRevert = async (version: number) => {
    setActionError(null);
    if (!author.trim()) {
      setActionError("Enter your name in the author field before reverting.");
      return;
    }
    if (
      !window.confirm(
        `This creates a new version identical to v${version}. Nothing is deleted.`
      )
    ) {
      return;
    }
    setBusy(true);
    try {
      const res = await revertConfig(repo, version, author.trim());
      setPushedVersion(res.version);
      setViewing(null);
      const rc = await getRepoConfig(repo);
      setText(rc?.config ?? "");
      refresh();
      refreshVersions();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}
      {initialized && data === null && (
        <div className="muted small-note">
          No config registered — pipelines for this repo can only be triggered
          manually until one is registered.
        </div>
      )}

      {currentVersion !== null && (
        <div className="config-editor-head">
          <span className="cfg-chip">Current: v{currentVersion}</span>
        </div>
      )}

      {viewing !== null && (
        <div className="view-banner">
          Viewing v{viewing} — current is v{currentVersion ?? "?"}
          <button type="button" className="btn" onClick={backToCurrent}>
            Back to current
          </button>
        </div>
      )}

      <textarea
        rows={16}
        className="mono yaml-input config-textarea"
        value={text}
        readOnly={viewing !== null}
        onChange={(e) => {
          setText(e.target.value);
          setPushedVersion(null);
        }}
        spellCheck={false}
        placeholder="stages: [build, test, deploy]&#10;jobs:&#10;  ..."
      />

      {actionError && <div className="error-banner">{actionError}</div>}
      {pushedVersion !== null && (
        <div className="saved-note">Pushed v{pushedVersion}</div>
      )}

      <div className="push-row">
        <label className="field push-author">
          <span>Author</span>
          <input
            value={author}
            onChange={(e) => setAuthorPersist(e.target.value)}
            placeholder="your name"
            required
          />
        </label>
        <label className="field push-message">
          <span>Change message (optional)</span>
          <input
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            placeholder="what changed and why"
          />
        </label>
        <button
          type="button"
          className="btn btn-primary"
          disabled={busy || viewing !== null || !author.trim()}
          onClick={() => void onPush()}
        >
          Push new version
        </button>
      </div>

      <h3 className="subsection-title">Version history</h3>
      {versions && versions.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Version</th>
                <th>Author</th>
                <th>Message</th>
                <th>When</th>
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => (
                <tr key={v.version}>
                  <td className="mono">
                    v{v.version}
                    {v.version === currentVersion && (
                      <span className="global-chip">current</span>
                    )}
                  </td>
                  <td>{v.author}</td>
                  <td>{v.message || <span className="muted">—</span>}</td>
                  <td className="muted">{relativeTime(v.created_at)}</td>
                  <td className="actions-cell">
                    <button
                      type="button"
                      className="btn btn-icon"
                      onClick={() => void onView(v.version)}
                    >
                      View
                    </button>{" "}
                    <button
                      type="button"
                      className="btn btn-icon"
                      disabled={busy}
                      onClick={() => void onRevert(v.version)}
                    >
                      Revert to this
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No versions yet.</div>
      )}
    </div>
  );
}

/* ---------------- Page ---------------- */

/* ---------------- Config templates (for include:) ---------------- */

const NEW_TEMPLATE = "__new__";

function TemplatesSection({ repo }: { repo: string }) {
  const fetcher = useCallback(() => listRepoTemplates(repo), [repo]);
  const { data: names, refresh } = usePoll(fetcher, 0, false);

  const [selected, setSelected] = useState<string>(NEW_TEMPLATE);
  const [name, setName] = useState("");
  const [yaml, setYaml] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  const onSelect = async (value: string) => {
    setSelected(value);
    setSaved(false);
    setError(null);
    if (value === NEW_TEMPLATE) {
      setName("");
      setYaml("");
      return;
    }
    try {
      const t = await getRepoTemplate(repo, value);
      setName(t.name);
      setYaml(t.yaml);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const onSave = async () => {
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      await putRepoTemplate(repo, name.trim(), yaml);
      setSaved(true);
      setSelected(name.trim());
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <p className="muted small-note">
        Templates resolved by <code>include: [&#123;template: name&#125;]</code>{" "}
        in a pipeline config.
      </p>
      <div className="form-row">
        <label className="field">
          <span>Template</span>
          <select value={selected} onChange={(e) => void onSelect(e.target.value)}>
            <option value={NEW_TEMPLATE}>New template…</option>
            {(names ?? []).map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Name</span>
          <input
            className="mono"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={selected !== NEW_TEMPLATE}
            required
          />
        </label>
      </div>
      <textarea
        rows={12}
        className="mono yaml-input config-textarea"
        value={yaml}
        onChange={(e) => {
          setYaml(e.target.value);
          setSaved(false);
        }}
        spellCheck={false}
        placeholder="jobs:&#10;  ..."
      />
      {error && <div className="error-banner">{error}</div>}
      {saved && <div className="saved-note">Template saved.</div>}
      <div className="form-actions">
        <button
          type="button"
          className="btn btn-primary"
          disabled={busy || !name.trim()}
          onClick={() => void onSave()}
        >
          Save template
        </button>
      </div>
    </div>
  );
}

/* ---------------- Scheduled pipelines ---------------- */

/** Absolute UTC label for a title tooltip, e.g. "2026-07-25 03:00 UTC". */
function absoluteUtc(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ` +
    `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`
  );
}

function SchedulesSection({ repo }: { repo: string }) {
  const fetcher = useCallback(() => listSchedules(repo), [repo]);
  const { data: schedules, error, refresh } = usePoll(fetcher, 0, false);

  const [isAdmin, setIsAdmin] = useState(false);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  // add-schedule form
  const [showForm, setShowForm] = useState(false);
  const [ref, setRef] = useState("main");
  const [cron, setCron] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const resetForm = () => {
    setShowForm(false);
    setRef("main");
    setCron("");
    setEnabled(true);
    setFormError(null);
  };

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFormError(null);
    try {
      await createSchedule(repo, { ref: ref.trim(), cron: cron.trim(), enabled });
      resetForm();
      refresh();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const onToggle = async (s: Schedule) => {
    setActionError(null);
    try {
      await updateSchedule(s.id, { enabled: !s.enabled });
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  const onDelete = async (s: Schedule) => {
    if (!window.confirm(`Delete schedule "${s.cron}" for ${s.ref}?`)) return;
    setActionError(null);
    try {
      await deleteSchedule(s.id);
      refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}
      {actionError && <div className="error-banner">{actionError}</div>}

      {isAdmin && (
        <div className="section-toolbar">
          {!showForm && (
            <button
              type="button"
              className="btn btn-primary"
              onClick={() => {
                resetForm();
                setShowForm(true);
              }}
            >
              Add Schedule
            </button>
          )}
        </div>
      )}

      {schedules && schedules.length > 0 ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Cron</th>
                <th>Ref</th>
                <th>Enabled</th>
                <th>Next run</th>
                <th>Last run</th>
                <th>Created by</th>
                {isAdmin && <th>Actions</th>}
              </tr>
            </thead>
            <tbody>
              {schedules.map((s) => (
                <tr key={s.id}>
                  <td className="mono">{s.cron}</td>
                  <td className="mono">{s.ref}</td>
                  <td>
                    <label className="check-row">
                      <input
                        type="checkbox"
                        checked={s.enabled}
                        disabled={!isAdmin}
                        onChange={() => void onToggle(s)}
                      />
                    </label>
                  </td>
                  <td className="muted">
                    {s.next_run_at ? (
                      <span title={absoluteUtc(s.next_run_at)}>
                        {relativeTime(s.next_run_at)}
                      </span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="muted">
                    {s.last_run_at ? (
                      <span title={absoluteUtc(s.last_run_at)}>
                        {relativeTime(s.last_run_at)}
                      </span>
                    ) : (
                      "never"
                    )}
                  </td>
                  <td>{s.created_by}</td>
                  {isAdmin && (
                    <td className="actions-cell">
                      <button
                        type="button"
                        className="btn btn-icon"
                        title="Delete"
                        onClick={() => void onDelete(s)}
                      >
                        🗑
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="muted">No schedules yet.</div>
      )}

      {showForm && isAdmin && (
        <form className="settings-form" onSubmit={(e) => void onSubmit(e)}>
          <h3>Add schedule</h3>
          <div className="form-row">
            <label className="field">
              <span>Ref</span>
              <input
                className="mono"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                required
              />
            </label>
            <label className="field">
              <span>Cron</span>
              <input
                className="mono"
                value={cron}
                onChange={(e) => setCron(e.target.value)}
                placeholder="0 3 * * *"
                required
              />
              <span className="muted small-note">
                5-field cron, UTC — e.g. <code>0 3 * * *</code> = 03:00 UTC daily
              </span>
            </label>
          </div>
          <label className="check-row">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            <span>Enabled</span>
          </label>
          {formError && <div className="error-banner">{formError}</div>}
          <div className="form-actions">
            <button type="button" className="btn" onClick={resetForm}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={busy}>
              Add schedule
            </button>
          </div>
        </form>
      )}

      <div className="muted small-note">
        Scheduled runs use <code>CI_PIPELINE_SOURCE=schedule</code> and the
        repo's registered pipeline config.
      </div>
    </div>
  );
}

/* ---------------- Page ---------------- */

export default function RepoSettings() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <Link to={`/repos/${encodeURIComponent(repo)}`}>{repo}</Link>{" "}
        <span className="crumb-sep">/</span> <span>settings</span>
      </div>

      <div className="page-head">
        <h1>CI/CD Settings</h1>
      </div>

      <Section
        title="Variables"
        description={
          <>
            Key/value pairs injected into job environments. Protected
            variables are only exposed to protected refs; masked values are
            hidden in job logs.{" "}
            <Link to="/docs/variables-secrets" className="guide-link">
              Guide →
            </Link>
          </>
        }
      >
        <VariablesSection repo={repo} />
      </Section>

      <Section
        title="Runner tags"
        description={
          <>
            Jobs in this repo run only on runners advertising ALL of these
            tags, unless a job sets its own <code>tags:</code> in YAML. Leave
            empty to run on any runner.{" "}
            <Link to="/docs/runner-groups" className="guide-link">
              Guide →
            </Link>
          </>
        }
      >
        <RunnerTagsSection repo={repo} />
      </Section>

      <Section
        title="Artifacts"
        description={
          <>
            Files uploaded by finished jobs for this repo. Downloads stream a
            .tar.gz.{" "}
            <Link to="/docs/artifacts" className="guide-link">
              Guide →
            </Link>
          </>
        }
      >
        <ArtifactsSection repo={repo} />
      </Section>

      <Section
        title="Members & approval rules"
        description={
          <>
            Who belongs to this repo and which roles can approve blocked
            deployments to protected environments.{" "}
            <Link to="/docs/approvals" className="guide-link">
              Guide →
            </Link>
          </>
        }
      >
        <MembersSection repo={repo} />
      </Section>

      <Section
        title="Pipeline config (CI YAML)"
        description={
          <>
            This YAML runs when GitHub/Bitbucket push webhooks arrive (Forge
            does not host the repo). Webhook URLs:{" "}
            <code>/api/v1/webhooks/github</code> and{" "}
            <code>/api/v1/webhooks/bitbucket</code>.{" "}
            <Link to="/docs/add-a-repo" className="guide-link">
              Guide →
            </Link>
          </>
        }
      >
        <ConfigSection repo={repo} />
      </Section>

      <Section
        title="Config templates"
        description={
          <>
            Reusable YAML fragments that a pipeline config pulls in with{" "}
            <code>include:</code>.
          </>
        }
      >
        <TemplatesSection repo={repo} />
      </Section>

      <Section
        title="Schedules"
        description={
          <>
            Cron-triggered pipeline runs for this repo. Each run uses the repo's
            registered pipeline config and sets{" "}
            <code>CI_PIPELINE_SOURCE=schedule</code>. Cron is 5-field, evaluated
            in UTC.
          </>
        }
      >
        <SchedulesSection repo={repo} />
      </Section>
    </div>
  );
}
