import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  createPipeline,
  getRepoConfig,
  listRegistry,
  listRepos,
  type CreatePipelineRequest,
  type RegisteredRepo,
  type RepoConfig,
} from "../api";

const OTHER = "__other__";

const DEFAULT_YAML = `stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    script:
      - echo "compiling..."
      - sleep 2
      - echo "build done"
  unit-tests:
    stage: test
    script:
      - echo "running unit tests"
      - sleep 2
      - echo "all 42 tests passed"
  deploy-dev:
    stage: deploy
    except: [main]
    script:
      - echo "deploying to DEV"
      - echo "done"
  deploy-prod:
    stage: deploy
    only: [main]
    environment: production
    script:
      - echo "deploying to production"
      - sleep 1
      - echo "deployed"
`;

export default function NewPipeline() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const preselect = searchParams.get("repo") ?? "";

  const [registry, setRegistry] = useState<RegisteredRepo[]>([]);
  const [repoOptions, setRepoOptions] = useState<string[] | null>(null);
  const [repoChoice, setRepoChoice] = useState<string>(OTHER);
  const [otherRepo, setOtherRepo] = useState("demo/app");
  const repo = repoChoice === OTHER ? otherRepo : repoChoice;

  const [ref, setRef] = useState("main");
  const [sha, setSha] = useState("");
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [config, setConfig] = useState(DEFAULT_YAML);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const connectedEntry = registry.find((r) => r.repo === repo) ?? null;
  const connected = Boolean(connectedEntry);

  // Registered pipeline config for the selected repo (null = none / 404).
  const [registeredConfig, setRegisteredConfig] = useState<RepoConfig | null>(
    null
  );

  useEffect(() => {
    let cancelled = false;
    if (repoChoice === OTHER) {
      setRegisteredConfig(null);
      setConfig(DEFAULT_YAML);
      return;
    }
    void (async () => {
      let loaded: RepoConfig | null = null;
      try {
        loaded = await getRepoConfig(repoChoice);
      } catch {
        loaded = null;
      }
      if (cancelled) return;
      setRegisteredConfig(loaded);
      setConfig(loaded?.config ?? DEFAULT_YAML);
    })();
    return () => {
      cancelled = true;
    };
  }, [repoChoice]);

  const configEdited =
    registeredConfig !== null && config !== registeredConfig.config;

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const [reg, repos] = await Promise.allSettled([listRegistry(), listRepos()]);
      const registered = reg.status === "fulfilled" ? reg.value : [];
      const names = new Set<string>();
      for (const r of registered) names.add(r.repo);
      if (repos.status === "fulfilled") {
        for (const r of repos.value) names.add(r.repo);
      }
      const options = [...names].sort();
      if (cancelled) return;
      setRegistry(registered);
      setRepoOptions(options);

      let chosen: string | null = null;
      if (preselect) {
        if (options.includes(preselect)) {
          chosen = preselect;
          setRepoChoice(preselect);
        } else {
          setRepoChoice(OTHER);
          setOtherRepo(preselect);
        }
      } else if (options.length > 0) {
        chosen = options[0];
        setRepoChoice(options[0]);
      }
      if (chosen) {
        const entry = registered.find((r) => r.repo === chosen);
        if (entry?.default_branch) setRef(entry.default_branch);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [preselect]);

  const onChoiceChange = (value: string) => {
    setRepoChoice(value);
    const entry = registry.find((r) => r.repo === value);
    if (entry?.default_branch) setRef(entry.default_branch);
  };

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const body: CreatePipelineRequest = { repo, ref, config };
      if (sha.trim()) body.sha = sha.trim();
      const { pipeline } = await createPipeline(body);
      navigate(`/pipelines/${pipeline.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  };

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>run</span>
      </div>

      <div className="page-head">
        <h1>Run pipeline</h1>
      </div>

      <form
        className="card form glow glow-neutral"
        onSubmit={(e) => void onSubmit(e)}
      >
        <div className="form-row">
          <label className="field">
            <span>Repo</span>
            <select
              value={repoChoice}
              onChange={(e) => onChoiceChange(e.target.value)}
            >
              {(repoOptions ?? []).map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
              <option value={OTHER}>Other…</option>
            </select>
            {repoChoice === OTHER && (
              <input
                className="mono"
                placeholder="owner/name"
                value={otherRepo}
                onChange={(e) => setOtherRepo(e.target.value)}
                required
              />
            )}
            <span className="field-hint">
              The branch tip is resolved and cloned automatically for
              connected repos.
            </span>
          </label>
          <label className="field">
            <span>Branch / tag</span>
            <input value={ref} onChange={(e) => setRef(e.target.value)} required />
            <span className="field-hint">
              Jobs with only/except are included per ref — e.g. ref main gets
              deploy-prod (approval gate), any other ref gets deploy-dev.
            </span>
          </label>
          {!connected && (
            <label className="field">
              <span>SHA</span>
              <input
                className="mono"
                placeholder="40-char commit sha"
                value={sha}
                onChange={(e) => setSha(e.target.value)}
                required
              />
              <span className="field-hint">
                Unconnected repos can't resolve a branch — provide the commit
                SHA to record.
              </span>
            </label>
          )}
        </div>

        {connected && (
          <div className="advanced">
            <button
              type="button"
              className="advanced-toggle"
              onClick={() => setShowAdvanced((s) => !s)}
            >
              {showAdvanced ? "Advanced ▾" : "Advanced ▸"}
            </button>
            {showAdvanced && (
              <label className="field advanced-body">
                <span>Specific commit SHA (leave empty to use the branch tip)</span>
                <input
                  className="mono"
                  placeholder="40-char commit sha"
                  value={sha}
                  onChange={(e) => setSha(e.target.value)}
                />
              </label>
            )}
          </div>
        )}

        <label className="field">
          <span>
            {registeredConfig !== null
              ? `Pipeline configuration (from repo settings, v${registeredConfig.version})`
              : "Pipeline YAML"}{" "}
            <Link to="/docs/writing-yaml" className="guide-link">
              Guide →
            </Link>
            {configEdited && (
              <button
                type="button"
                className="link-btn"
                onClick={() => setConfig(registeredConfig?.config ?? DEFAULT_YAML)}
              >
                Reset to registered config
              </button>
            )}
          </span>
          <textarea
            rows={18}
            className="mono yaml-input"
            value={config}
            onChange={(e) => setConfig(e.target.value)}
            spellCheck={false}
          />
          <span className="field-hint">
            {registeredConfig !== null ? (
              <>
                Loaded from this repo's registered config. Edits below apply
                to this run only — change the permanent config in{" "}
                <Link to={`/repos/${encodeURIComponent(repo)}/settings`}>
                  Settings → Pipeline config
                </Link>
                .
              </>
            ) : (
              <>
                No registered config for this repo — using an example.
                Register one in{" "}
                {repoChoice !== OTHER ? (
                  <Link to={`/repos/${encodeURIComponent(repo)}/settings`}>
                    Settings → Pipeline config
                  </Link>
                ) : (
                  <>Settings → Pipeline config</>
                )}{" "}
                so webhooks can trigger runs.
              </>
            )}
          </span>
        </label>

        {error && <div className="error-banner">{error}</div>}

        <div className="form-actions">
          <button type="submit" className="btn btn-primary" disabled={submitting}>
            {submitting ? "Starting…" : "Run pipeline"}
          </button>
        </div>
      </form>
    </div>
  );
}
