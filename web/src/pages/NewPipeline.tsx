import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { createPipeline, listRegistry, listRepos } from "../api";

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

  const [repoOptions, setRepoOptions] = useState<string[] | null>(null);
  const [repoChoice, setRepoChoice] = useState<string>(OTHER);
  const [otherRepo, setOtherRepo] = useState("demo/app");
  const repo = repoChoice === OTHER ? otherRepo : repoChoice;

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      const [reg, repos] = await Promise.allSettled([listRegistry(), listRepos()]);
      const names = new Set<string>();
      if (reg.status === "fulfilled") {
        for (const r of reg.value) names.add(r.repo);
      }
      if (repos.status === "fulfilled") {
        for (const r of repos.value) names.add(r.repo);
      }
      const options = [...names].sort();
      if (cancelled) return;
      setRepoOptions(options);
      if (preselect) {
        if (options.includes(preselect)) {
          setRepoChoice(preselect);
        } else {
          setRepoChoice(OTHER);
          setOtherRepo(preselect);
        }
      } else if (options.length > 0) {
        setRepoChoice(options[0]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [preselect]);

  const [ref, setRef] = useState("main");
  const [sha, setSha] = useState("deadbeefcafe1234");
  const [config, setConfig] = useState(DEFAULT_YAML);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const { pipeline } = await createPipeline({ repo, ref, sha, config });
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
        <span>new</span>
      </div>

      <div className="page-head">
        <h1>New Pipeline</h1>
      </div>

      <form className="card form" onSubmit={(e) => void onSubmit(e)}>
        <div className="form-row">
          <label className="field">
            <span>Repo</span>
            <select
              value={repoChoice}
              onChange={(e) => setRepoChoice(e.target.value)}
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
              Pipelines for connected repos clone the source at the given SHA
              before running.
            </span>
          </label>
          <label className="field">
            <span>Ref</span>
            <input value={ref} onChange={(e) => setRef(e.target.value)} required />
            <span className="field-hint">
              Jobs with only/except are included per ref — e.g. ref main gets
              deploy-prod (approval gate), any other ref gets deploy-dev.
            </span>
          </label>
          <label className="field">
            <span>SHA</span>
            <input
              className="mono"
              value={sha}
              onChange={(e) => setSha(e.target.value)}
              required
            />
          </label>
        </div>

        <label className="field">
          <span>
            Pipeline YAML{" "}
            <Link to="/docs/writing-yaml" className="guide-link">
              Guide →
            </Link>
          </span>
          <textarea
            rows={18}
            className="mono yaml-input"
            value={config}
            onChange={(e) => setConfig(e.target.value)}
            spellCheck={false}
          />
        </label>

        {error && <div className="error-banner">{error}</div>}

        <div className="form-actions">
          <button type="submit" className="btn btn-primary" disabled={submitting}>
            {submitting ? "Creating…" : "Create Pipeline"}
          </button>
        </div>
      </form>
    </div>
  );
}
