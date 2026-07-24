import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  createFreeze,
  decodeRepoParam,
  deleteFreeze,
  getIsAdmin,
  listDeployments,
  listEnvironments,
  listFreezes,
  relativeTime,
  rollbackEnvironment,
  shortSha,
  type Deployment,
  type Environment,
  type Freeze,
} from "../api";
import { usePoll } from "../hooks/usePoll";

function DriftBadge({ env }: { env: Environment }) {
  if (env.drift === "in_sync") {
    return <span className="badge badge-success">in sync</span>;
  }
  if (env.drift === "drifted") {
    return (
      <span className="badge badge-blocked">
        drifted{env.ref_tip_sha ? ` · tip ${shortSha(env.ref_tip_sha)}` : ""}
      </span>
    );
  }
  return <span className="badge badge-created">unknown</span>;
}

function FreezePanel({
  repo,
  env,
  isAdmin,
}: {
  repo: string;
  env: string;
  isAdmin: boolean;
}) {
  const fetcher = useCallback(() => listFreezes(repo, env), [repo, env]);
  const { data: freezes, refresh } = usePoll(fetcher, 0, false);

  const [starts, setStarts] = useState("");
  const [ends, setEnds] = useState("");
  const [reason, setReason] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);

  const now = Date.now();
  const active = (freezes ?? []).filter(
    (f) => new Date(f.starts_at).getTime() <= now && new Date(f.ends_at).getTime() >= now
  );

  const onAdd = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      await createFreeze(repo, env, {
        starts_at: new Date(starts).toISOString(),
        ends_at: new Date(ends).toISOString(),
        reason,
      });
      setStarts("");
      setEnds("");
      setReason("");
      setOpen(false);
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const onDelete = async (f: Freeze) => {
    if (!window.confirm("Lift this deploy freeze?")) return;
    try {
      await deleteFreeze(repo, env, f.id);
      refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="freeze-panel">
      {active.map((f) => (
        <div key={f.id} className="freeze-banner">
          <span>
            ❄ Deploy freeze until {new Date(f.ends_at).toLocaleString()}
            {f.reason ? ` — ${f.reason}` : ""}
          </span>
          {isAdmin && (
            <button
              type="button"
              className="btn btn-icon"
              onClick={() => void onDelete(f)}
            >
              Lift
            </button>
          )}
        </div>
      ))}
      {isAdmin && (
        <div className="freeze-add">
          {!open ? (
            <button type="button" className="btn" onClick={() => setOpen(true)}>
              Add freeze
            </button>
          ) : (
            <form className="settings-form" onSubmit={(e) => void onAdd(e)}>
              <div className="form-row">
                <label className="field">
                  <span>Starts</span>
                  <input
                    type="datetime-local"
                    value={starts}
                    onChange={(e) => setStarts(e.target.value)}
                    required
                  />
                </label>
                <label className="field">
                  <span>Ends</span>
                  <input
                    type="datetime-local"
                    value={ends}
                    onChange={(e) => setEnds(e.target.value)}
                    required
                  />
                </label>
              </div>
              <label className="field">
                <span>Reason</span>
                <input
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  placeholder="e.g. release window"
                />
              </label>
              {error && <div className="error-banner">{error}</div>}
              <div className="form-actions">
                <button type="button" className="btn" onClick={() => setOpen(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary">
                  Add freeze
                </button>
              </div>
            </form>
          )}
        </div>
      )}
    </div>
  );
}

function EnvHistory({
  repo,
  env,
  onRolledBack,
}: {
  repo: string;
  env: string;
  onRolledBack: (pipelineId: number) => void;
}) {
  const [items, setItems] = useState<Deployment[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    listDeployments(repo, env, 20, 0)
      .then((r) => {
        if (!cancelled) {
          setItems(r.items);
          setLoading(false);
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [repo, env]);

  const onRollback = async (d: Deployment) => {
    if (
      !window.confirm(
        `Creates a new pipeline deploying ${shortSha(d.sha)}; protected environments still require approval.`
      )
    ) {
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const { pipeline } = await rollbackEnvironment(repo, env, d.pipeline_id);
      onRolledBack(pipeline.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  if (loading) return <div className="muted">Loading history…</div>;

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}
      {items.length === 0 ? (
        <div className="muted">No deployments recorded.</div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Pipeline</th>
                <th>SHA</th>
                <th>Ref</th>
                <th>By</th>
                <th>When</th>
                <th>Status</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {items.map((d) => (
                <tr key={d.id}>
                  <td>
                    <Link to={`/pipelines/${d.pipeline_id}`} className="mono">
                      #{d.pipeline_id}
                    </Link>
                  </td>
                  <td className="mono">{shortSha(d.sha)}</td>
                  <td>{d.ref}</td>
                  <td>{d.deployed_by}</td>
                  <td className="muted">{relativeTime(d.deployed_at)}</td>
                  <td>
                    <span className={`badge badge-${d.status}`}>{d.status}</span>
                  </td>
                  <td>
                    <button
                      type="button"
                      className="btn btn-icon"
                      disabled={busy}
                      onClick={() => void onRollback(d)}
                    >
                      Rollback to this
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function EnvCard({
  repo,
  env,
  isAdmin,
}: {
  repo: string;
  env: Environment;
  isAdmin: boolean;
}) {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);

  return (
    <div className="card env-card glow glow-neutral">
      <div className="env-card-top">
        <span className="env-name">{env.environment}</span>
        <DriftBadge env={env} />
      </div>
      {env.current ? (
        <div className="env-current">
          <span className="mono sha">{shortSha(env.current.sha)}</span>
          <span className="ref-tag">{env.current.ref}</span>
          <span className="muted">
            deployed by {env.current.deployed_by || "unknown"},{" "}
            {relativeTime(env.current.deployed_at)}
          </span>
        </div>
      ) : (
        <div className="muted">Nothing deployed yet.</div>
      )}
      <div className="env-meta muted">
        {env.deployment_count} deployment
        {env.deployment_count === 1 ? "" : "s"}
      </div>

      <FreezePanel repo={repo} env={env.environment} isAdmin={isAdmin} />

      <button
        type="button"
        className="btn env-history-toggle"
        onClick={() => setOpen((o) => !o)}
      >
        {open ? "Hide history" : "History & rollback"}
      </button>
      {open && (
        <EnvHistory
          repo={repo}
          env={env.environment}
          onRolledBack={(pid) => navigate(`/pipelines/${pid}`)}
        />
      )}
    </div>
  );
}

export default function Environments() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

  const fetcher = useCallback(() => listEnvironments(repo), [repo]);
  const { data: envs, error, loading } = usePoll(fetcher, 5000);

  const [isAdmin, setIsAdmin] = useState(false);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <Link to={`/repos/${encodeURIComponent(repo)}`}>{repo}</Link>{" "}
        <span className="crumb-sep">/</span> <span>environments</span>
      </div>

      <div className="page-head">
        <h1>Environments</h1>
      </div>

      {error && <div className="error-banner">Failed to load environments: {error}</div>}
      {loading && !envs && <div className="muted">Loading…</div>}
      {envs && envs.length === 0 && (
        <div className="empty card">
          No environments yet — a job with an <code>environment:</code> deploys
          to one.
        </div>
      )}

      {envs && envs.length > 0 && (
        <div className="env-grid">
          {envs.map((env) => (
            <EnvCard
              key={env.environment}
              repo={repo}
              env={env}
              isAdmin={isAdmin}
            />
          ))}
        </div>
      )}
    </div>
  );
}
