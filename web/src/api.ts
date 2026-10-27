export type PipelineStatus =
  | "created"
  | "running"
  | "blocked"
  | "success"
  | "failed"
  | "canceled";

export type JobStatus =
  | "created"
  | "blocked"
  | "pending"
  | "running"
  | "success"
  | "failed"
  | "canceled";

export interface StageStatus {
  name: string;
  status: string;
}

export interface Pipeline {
  id: number;
  /** mean coverage of the jobs that report one */
  coverage?: number;
  /** deployment a promotion pipeline redeploys */
  promoted_from?: number;
  repo: string;
  ref: string;
  sha: string;
  status: string;
  created_at: string;
  /** Derived per-stage statuses, ordered by stage_idx. */
  stages: StageStatus[];
  /** Registered config version this run used; null = one-off custom config. */
  config_version: number | null;
  /** Where the config that ran came from: 'repo' (in-repo file) | 'registered'. */
  config_source?: string;
  /**
   * Deep link to the config file this run used, in the provider's web UI at the
   * run's commit. Absent when no correct link exists (unsupported provider,
   * registered config, missing CodeCommit region) — render nothing then.
   */
  config_url?: string;
  /** Trigger: api | push | webhook | merge_request | schedule. */
  source?: string;
  /** Identity that started the run; absent for a schedule (nobody did). */
  triggered_by?: string;
  /** Who WROTE the code — differs from triggered_by on merges, bot pushes and
   *  re-runs. Absent when the provider event does not say. */
  commit_author?: string;
  /** What the run is about: the commit message, or the PR title for a PR run. */
  commit_message?: string;
}

/** First line of a commit message — the subject, which is what a header shows. */
export function commitSubject(message?: string): string {
  if (!message) return "";
  const line = message.split("\n", 1)[0].trim();
  return line;
}

/**
 * What the run's actor DID, for the line beside their avatar. Kept separate
 * from sourceLabel (which labels the run) because this reads as a sentence:
 * "alice pushed", "alice opened the PR".
 */
export function triggerVerb(source?: string): string {
  switch (source) {
    case "push":
    case "webhook":
      return "pushed";
    case "merge_request":
      return "opened the PR";
    case "schedule":
      return "scheduled run";
    case "promotion":
      return "promoted a build";
    default:
      return "started this run";
  }
}

/** Human label for a pipeline trigger source; "" for the ordinary api/push case. */
export function sourceLabel(source?: string): string {
  switch (source) {
    case "merge_request":
      return "merge request";
    case "schedule":
      return "scheduled";
    case "promotion":
      return "promotion";
    default:
      return "";
  }
}

export interface Job {
  id: number;
  /** coverage percentage, when the job reports one */
  coverage?: number;
  pipeline_id: number;
  name: string;
  stage: string;
  stage_idx: number;
  image: string | null;
  environment: string | null;
  status: string;
  started_at: string | null;
  finished_at: string | null;
  exit_code: number | null;
  needs: number[];
  /** blocked jobs: true = manual gate (Play), false/absent = approval gate */
  manual?: boolean;
  /** failure of this job does not fail the pipeline */
  allow_failure?: boolean;
  /** append-only vote record, oldest first; only for environment-gated jobs */
  approvals?: JobApproval[];
  /** votes the environment's rule requires; absent when the job has no gate */
  required_approvals?: number;
  /** the gate also needs required_group_approvals votes from this group */
  required_group?: string;
  required_group_approvals?: number;
  /** approved votes so far from the required group, and its members */
  group_approvals?: number;
  group_members?: string[];
  /** why the job ended without running, e.g. a branch rule refused it */
  status_reason?: string;
  /** the job this one re-runs, when it came from "Retry job" */
  retry_of?: number;
  /** at most one job per (repo, group) runs at a time */
  resource_group?: string;
  /** the runner that ran (or is running) the job */
  runner_id?: string | null;
}

/** One recorded vote on an approval-gated job. */
export interface JobApproval {
  approver: string;
  verdict: "approved" | "rejected";
  comment?: string;
  created_at: string;
}

export interface PipelineDetail {
  pipeline: Pipeline;
  jobs: Job[];
  /** earlier attempts replaced by "Retry job", newest first */
  previous_jobs?: Job[];
}

export interface Variable {
  id: number;
  repo: string;
  key: string;
  /** empty string when masked and not revealed */
  value: string;
  protected: boolean;
  masked: boolean;
  environment_scope: string;
  created_at: string;
}

export interface Runner {
  id: number | string;
  executor: string;
  tags: string[];
  description: string;
  paused: boolean;
  online: boolean;
  created_at: string;
  last_contact_at: string;
  /** reported build version; outdated when it differs from the server's bundled runner */
  version?: string;
  outdated?: boolean;
  /** taking no new jobs; running_jobs still in flight */
  draining?: boolean;
  update_requested?: boolean;
  running_jobs?: number;
  /** job slots (the runner's --concurrency; 1 for older runners) */
  concurrency?: number;
  /** not seen for RUNNER_STALE_AFTER (default a day): left out of capacity and tag counts */
  stale?: boolean;
  /** the autoscaled pool it reported, or the one pool whose tags equal its tags (pool_inferred) */
  pool?: string;
  pool_inferred?: boolean;
}

export interface Artifact {
  id: number;
  job_id: number;
  job_name: string;
  pipeline_id: number;
  repo: string;
  name: string;
  size_bytes: number;
  created_at: string;
  /** Per-artifact expiry (from artifacts.expire_in); null/absent = no expiry. */
  expires_at?: string | null;
}

/** One failed or errored test case in a JUnit report. */
export interface JUnitFailure {
  name: string;
  classname?: string;
  /** "failure" | "error" */
  type: string;
  message?: string;
}

/** Per-job JUnit test summary. Absent (404) when the job produced no report. */
export interface JUnitReport {
  job_id: number;
  total: number;
  passed: number;
  failed: number;
  skipped: number;
  duration_seconds: number;
  failures: JUnitFailure[];
  created_at: string;
}

/** One test case's outcome from a single job's JUnit report. */
export interface TestCaseResult {
  id: number;
  job_id: number;
  name: string;
  classname?: string;
  status: "passed" | "failed" | "skipped";
  duration_seconds: number;
  /** Only set for a failed/errored case. */
  message?: string;
  created_at: string;
}

/** One test case's identity plus its recent runs, newest first. */
export interface TestCaseHistory {
  name: string;
  classname?: string;
  runs: TestCaseResult[];
}

export interface Member {
  id: number;
  repo: string;
  username: string;
  role: string;
}

export interface ProtectedEnvironment {
  id: number;
  /** empty string means a global default (read-only) */
  repo: string;
  name: string;
  required_approvals: number;
  approval_timeout_hours: number;
  approver_roles: string[];
  allow_self_approval: boolean;
  /** of the required approvals, at least required_group_approvals from this group */
  required_group?: string;
  required_group_approvals?: number;
  /** cannot match any registered environment (safe to delete) */
  inactive?: boolean;
}

export interface RepoConfig {
  repo: string;
  config: string;
  version: number;
}

export interface ConfigVersion {
  version: number;
  author: string;
  message: string;
  created_at: string;
}

export interface RepoSettings {
  repo: string;
  default_runner_tags: string[];
}

export interface Schedule {
  id: number;
  repo: string;
  ref: string;
  cron: string;
  enabled: boolean;
  created_by: string;
  created_at: string;
  last_run_at: string | null;
  next_run_at: string | null;
}

/** A git host Forge integrates with; "git" is any git server (plain git). */
export type RepoProvider = "github" | "gitlab" | "bitbucket" | "gitea" | "codecommit" | "git" | "other";

export interface RegisteredRepo {
  repo: string;
  provider: string;
  clone_url: string;
  has_token: boolean;
  default_branch: string;
  created_at: string;
  has_github_app?: boolean;
  github_app_id?: string;
  github_installation_id?: string;
  /** CodeCommit only. Non-secret: CodeCommit access is IAM-signed, not tokenised. */
  aws_region?: string;
  aws_profile?: string;
  aws_role_arn?: string;
}

export interface RepoRegistryInput {
  repo: string;
  provider: RepoProvider;
  clone_url?: string;
  token?: string;
  default_branch?: string;
  github_app_id?: string;
  github_installation_id?: string;
  github_app_private_key?: string;
  /** CodeCommit only; aws_region is required for that provider. */
  aws_region?: string;
  aws_profile?: string;
  aws_role_arn?: string;
}

export interface RepoSummary {
  repo: string;
  pipeline_count: number;
  success_count: number;
  failed_count: number;
  /** distinct recent refs, most recent first (may be cut short; see ref_count) */
  refs: string[];
  /** distinct refs among the repo's recent runs */
  ref_count?: number;
  /** last <=5 pipeline statuses, newest first */
  recent_statuses: string[];
  last_pipeline: Pipeline | null;
  last_activity_at: string;
}

export interface ApprovalRequest {
  approver: string;
  verdict: "approved" | "rejected";
  comment?: string;
}

export interface CreatePipelineRequest {
  repo: string;
  ref: string;
  /** Optional: connected repos resolve the ref tip server-side. */
  sha?: string;
  config: string;
}

const BASE = "/api/v1";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function parseError(res: Response): Promise<ApiError> {
  let message = `Request failed with status ${res.status}`;
  try {
    const body = await res.json();
    if (body && typeof body.error === "string") {
      message = body.error;
    }
  } catch {
    // response body was not JSON; keep default message
  }
  // When SSO enforcement is on, every non-auth endpoint 401s — send the
  // user to the login page (auth endpoints bypass this helper).
  if (
    res.status === 401 &&
    typeof window !== "undefined" &&
    !window.location.pathname.startsWith("/login")
  ) {
    // Come back to this page after signing in.
    const next = window.location.pathname + window.location.search;
    window.location.assign(next === "/" ? "/login" : `/login?next=${encodeURIComponent(next)}`);
  }
  return new ApiError(res.status, message);
}

// Stale-while-revalidate for GETs: a page you have seen in the last minute
// renders instantly from memory while a fresh copy loads; usePoll swaps it in
// as soon as it lands (see revalidation()). Any write clears the cache, so an
// action is never followed by stale data.
const SWR_MAX_AGE_MS = 60_000;
const SWR_FRESH_MS = 1_000;
const swr = new Map<string, { at: number; data: unknown; inflight?: Promise<unknown> }>();
const revalidating = new WeakMap<object, Promise<unknown>>();

/** The pending refresh of a result that was served from the cache, if any. */
export function revalidation(result: unknown): Promise<unknown> | undefined {
  return typeof result === "object" && result !== null ? revalidating.get(result) : undefined;
}

function mutated(): void {
  swr.clear();
}

async function fetchJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

function getJSON<T>(path: string): Promise<T> {
  const load = () =>
    fetchJSON<T>(path).then(
      (data) => {
        swr.set(path, { at: Date.now(), data });
        return data;
      },
      (err) => {
        swr.delete(path);
        throw err;
      }
    );
  const hit = swr.get(path);
  const age = hit ? Date.now() - hit.at : Infinity;
  if (!hit || age >= SWR_MAX_AGE_MS) return load();
  if (age >= SWR_FRESH_MS) {
    hit.inflight ??= load().catch(() => undefined);
    if (typeof hit.data === "object" && hit.data !== null) revalidating.set(hit.data, hit.inflight);
  }
  return Promise.resolve(hit.data as T);
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  mutated();
  const res = await fetch(`${BASE}${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

/** For endpoints that return no body (204 etc.). */
async function requestVoid(
  method: "PUT" | "POST" | "PATCH" | "DELETE",
  path: string,
  body?: unknown
): Promise<void> {
  mutated();
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers:
      body !== undefined
        ? { "Content-Type": "application/json", Accept: "application/json" }
        : { Accept: "application/json" },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    throw await parseError(res);
  }
}

async function putJSON<T>(path: string, body: unknown): Promise<T> {
  mutated();
  const res = await fetch(`${BASE}${path}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

export function listPipelines(repo?: string): Promise<Pipeline[]> {
  const qs = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  return getJSON<Pipeline[]>(`/pipelines${qs}`);
}

export function listRepos(): Promise<RepoSummary[]> {
  return getJSON<RepoSummary[]>("/repos");
}

export type RepoSort = "activity" | "name";

export interface RepoPage {
  items: RepoSummary[];
  total: number;
  hasMore: boolean;
}

/** One page of repo cards; search, sort and paging happen on the server. */
export async function listReposPage(
  q: string,
  sort: RepoSort,
  offset: number,
  limit: number
): Promise<RepoPage> {
  const qs = new URLSearchParams({ limit: String(limit), offset: String(offset) });
  if (q) qs.set("q", q);
  if (sort !== "activity") qs.set("sort", sort);
  const res = await fetch(`${BASE}/repos?${qs.toString()}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return {
    items: (await res.json()) as RepoSummary[],
    total: Number(res.headers.get("X-Total-Count") ?? 0),
    hasMore: res.headers.get("X-Has-More") === "true",
  };
}

// --- CI/CD settings ---

export function listVariables(repo: string, reveal: boolean): Promise<Variable[]> {
  const qs = `?repo=${encodeURIComponent(repo)}${reveal ? "&reveal=1" : ""}`;
  return getJSON<Variable[]>(`/variables${qs}`);
}

export interface VariableInput {
  key: string;
  value: string;
  protected: boolean;
  masked: boolean;
  environment_scope: string;
}

export function createVariable(
  repo: string,
  input: VariableInput
): Promise<Variable> {
  return postJSON<Variable>("/variables", { repo, ...input });
}

export function updateVariable(
  id: number,
  input: Omit<VariableInput, "key">
): Promise<Variable> {
  return putJSON<Variable>(`/variables/${id}`, input);
}

export function deleteVariable(id: number): Promise<void> {
  return requestVoid("DELETE", `/variables/${id}`);
}

export function listRunners(): Promise<Runner[]> {
  return getJSON<Runner[]>("/runners");
}

/** Admin: remove an offline, idle runner (409 when it is online or busy). */
export function deleteRunner(id: string): Promise<void> {
  return requestVoid("DELETE", `/runners/${encodeURIComponent(id)}`);
}

export function setRunnerPaused(
  id: number | string,
  paused: boolean
): Promise<Runner> {
  return postJSON<Runner>(`/runners/${encodeURIComponent(id)}/pause`, { paused });
}

export function listArtifacts(repo: string, jobId?: number | string): Promise<Artifact[]> {
  const qs = `?repo=${encodeURIComponent(repo)}${jobId !== undefined ? `&job=${jobId}` : ""}`;
  return getJSON<Artifact[]>(`/artifacts${qs}`);
}

export function artifactDownloadUrl(id: number): string {
  return `${BASE}/artifacts/${id}/download`;
}

/** A job's parsed JUnit summary, or null when the job produced no report (404). */
export async function getJobReport(
  id: number | string
): Promise<JUnitReport | null> {
  try {
    return await getJSON<JUnitReport>(`/jobs/${id}/report`);
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null;
    throw err;
  }
}

export function listMembers(repo: string): Promise<Member[]> {
  return getJSON<Member[]>(`/members?repo=${encodeURIComponent(repo)}`);
}

export function addMember(
  repo: string,
  username: string,
  role: string
): Promise<Member> {
  return postJSON<Member>("/members", { repo, username, role });
}

export function deleteMember(id: number): Promise<void> {
  return requestVoid("DELETE", `/members/${id}`);
}

export function listProtectedEnvironments(
  repo: string
): Promise<ProtectedEnvironment[]> {
  return getJSON<ProtectedEnvironment[]>(
    `/protected-environments?repo=${encodeURIComponent(repo)}`
  );
}

export interface ProtectedEnvironmentInput {
  name: string;
  required_approvals: number;
  approval_timeout_hours: number;
  approver_roles: string[];
  allow_self_approval: boolean;
  required_group?: string;
  required_group_approvals?: number;
}

export function upsertProtectedEnvironment(
  repo: string,
  input: ProtectedEnvironmentInput
): Promise<ProtectedEnvironment> {
  return postJSON<ProtectedEnvironment>("/protected-environments", {
    repo,
    ...input,
  });
}

export async function getRepoConfig(
  repo: string,
  version?: number
): Promise<RepoConfig | null> {
  const qs = `?repo=${encodeURIComponent(repo)}${
    version !== undefined ? `&version=${version}` : ""
  }`;
  try {
    return await getJSON<RepoConfig>(`/repo-configs${qs}`);
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null;
    throw err;
  }
}

export function putRepoConfig(
  repo: string,
  config: string,
  author: string,
  message: string
): Promise<{ repo: string; version: number }> {
  return putJSON<{ repo: string; version: number }>("/repo-configs", {
    repo,
    config,
    author,
    message,
  });
}

export function listConfigVersions(repo: string): Promise<ConfigVersion[]> {
  return getJSON<ConfigVersion[]>(
    `/repo-configs/versions?repo=${encodeURIComponent(repo)}`
  );
}

export function revertConfig(
  repo: string,
  version: number,
  author: string
): Promise<{ repo: string; version: number; reverted_to: number }> {
  return postJSON<{ repo: string; version: number; reverted_to: number }>(
    "/repo-configs/revert",
    { repo, version, author }
  );
}

export function listRegistry(): Promise<RegisteredRepo[]> {
  return getJSON<RegisteredRepo[]>("/repo-registry");
}

export function registerRepo(input: RepoRegistryInput): Promise<void> {
  return requestVoid("POST", "/repo-registry", input);
}

export function getRepoSettings(repo: string): Promise<RepoSettings> {
  return getJSON<RepoSettings>(`/repo-settings?repo=${encodeURIComponent(repo)}`);
}

export function putRepoSettings(
  repo: string,
  default_runner_tags: string[]
): Promise<void> {
  return requestVoid("PUT", "/repo-settings", { repo, default_runner_tags });
}

// --- Scheduled pipelines ---

export function listSchedules(repo: string): Promise<Schedule[]> {
  return getJSON<Schedule[]>(`/schedules?repo=${encodeURIComponent(repo)}`);
}

export interface ScheduleInput {
  ref: string;
  cron: string;
  enabled: boolean;
}

export function createSchedule(
  repo: string,
  input: ScheduleInput
): Promise<{ schedule: Schedule }> {
  return postJSON<{ schedule: Schedule }>("/schedules", { repo, ...input });
}

export function updateSchedule(
  id: number,
  input: Partial<ScheduleInput>
): Promise<{ schedule: Schedule }> {
  return putJSON<{ schedule: Schedule }>(`/schedules/${id}`, input);
}

export function deleteSchedule(id: number): Promise<void> {
  return requestVoid("DELETE", `/schedules/${id}`);
}

// --- Environments ---

export interface Deployment {
  id: number;
  sha: string;
  ref: string;
  pipeline_id: number;
  deployed_by: string;
  deployed_at: string;
  status: string;
  /** where it's reachable (environment: url:, or the job's DEPLOY_URL) */
  url?: string;
  /** DEPLOY_VERSION from the job, else the short sha */
  version?: string;
  /** immutable build identity (image digest, checksum) when reported */
  digest?: string;
  /** deployment this one was promoted from */
  promoted_from?: number;
}

export interface Environment {
  /** the environment name (server field is `environment`) */
  environment: string;
  repo?: string;
  current: Deployment | null;
  deployment_count: number;
  drift: "in_sync" | "drifted" | "unknown";
  ref_tip_sha?: string;
  frozen?: boolean;
  /** what Forge Live sees running there */
  live?: LiveSummary;
}

export interface Freeze {
  id: number;
  starts_at: string;
  ends_at: string;
  reason: string;
}

/**
 * Environment sub-paths embed the repo unencoded (the server parses the
 * subtree), so repos containing a slash must NOT be collapsed into one
 * encoded segment. Only the env/action segments are encoded.
 */
function envPath(repo: string, env: string, action?: string): string {
  const base = `/environments/${repo}/${encodeURIComponent(env)}`;
  return action ? `${base}/${action}` : base;
}

export function listEnvironments(repo: string): Promise<Environment[]> {
  return getJSON<Environment[]>(
    `/environments?repo=${encodeURIComponent(repo)}`
  );
}

/** ?limit caps runs PER CASE (not the total row count); omit for the server default. */
export function listTestHistory(
  repo: string,
  limit?: number
): Promise<TestCaseHistory[]> {
  const qs = `?repo=${encodeURIComponent(repo)}${limit ? `&limit=${limit}` : ""}`;
  return getJSON<TestCaseHistory[]>(`/test-history${qs}`);
}

export async function listDeployments(
  repo: string,
  env: string,
  limit: number,
  offset: number
): Promise<{ items: Deployment[]; total: number }> {
  const res = await fetch(
    `${BASE}${envPath(repo, env, "deployments")}?limit=${limit}&offset=${offset}`,
    { headers: { Accept: "application/json" } }
  );
  if (!res.ok) throw await parseError(res);
  const total = Number(res.headers.get("X-Total-Count") ?? "0");
  const items = (await res.json()) as Deployment[];
  return { items, total };
}

/** Redeploys an earlier deployment's build (same commit, same artifacts) to env. */
export function promoteToEnvironment(
  repo: string,
  env: string,
  from: { from_environment?: string; deployment_id?: number }
): Promise<{ pipeline: Pipeline; promoted_from: Deployment }> {
  return postJSON<{ pipeline: Pipeline; promoted_from: Deployment }>(envPath(repo, env, "promote"), from);
}

export interface CoveragePoint {
  pipeline_id: number;
  ref: string;
  sha: string;
  coverage: number;
  created_at: string;
}

export function getCoverageTrend(repo: string, ref?: string): Promise<CoveragePoint[]> {
  const qs = `?repo=${encodeURIComponent(repo)}${ref ? `&ref=${encodeURIComponent(ref)}` : ""}`;
  return getJSON<{ points: CoveragePoint[] }>(`/coverage${qs}`).then((r) => r.points ?? []);
}

export function rollbackEnvironment(
  repo: string,
  env: string,
  toPipelineId: number
): Promise<{ pipeline: Pipeline }> {
  return postJSON<{ pipeline: Pipeline }>(envPath(repo, env, "rollback"), {
    to_pipeline_id: toPipelineId,
  });
}

export async function listFreezes(repo: string, env: string): Promise<Freeze[]> {
  try {
    return await getJSON<Freeze[]>(envPath(repo, env, "freezes"));
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return [];
    throw err;
  }
}

export function createFreeze(
  repo: string,
  env: string,
  body: { starts_at: string; ends_at: string; reason: string }
): Promise<Freeze> {
  return postJSON<Freeze>(envPath(repo, env, "freezes"), body);
}

export function deleteFreeze(
  repo: string,
  env: string,
  id: number
): Promise<void> {
  return requestVoid("DELETE", envPath(repo, env, `freezes/${id}`));
}

// --- Audit log ---

export interface AuditEntry {
  id: number;
  ts: string;
  /** same instant as ts */
  created_at: string;
  actor: string;
  action: string;
  target: string;
  repo: string;
  detail: unknown;
  source_ip: string;
  result: string;
}

export async function listAuditLog(params: {
  limit: number;
  offset: number;
  repo?: string;
  actor?: string;
}): Promise<{ items: AuditEntry[]; total: number }> {
  const qs = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
  });
  if (params.repo) qs.set("repo", params.repo);
  if (params.actor) qs.set("actor", params.actor);
  const res = await fetch(`${BASE}/audit-log?${qs.toString()}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) throw await parseError(res);
  const items = (await res.json()) as AuditEntry[];
  const header = res.headers.get("X-Total-Count");
  // Some deployments omit the count header — estimate enough to keep
  // pagination working (assume more if the page came back full).
  const total =
    header !== null
      ? Number(header)
      : params.offset + items.length + (items.length === params.limit ? 1 : 0);
  return { items, total };
}

// --- Runner tokens (RUNNER_AUTH) ---

/** A runner auth token. Token is only ever populated by createRunnerToken's
 *  response — list responses carry TokenSuffix instead, never the raw value. */
export interface RunnerToken {
  id: number;
  token?: string;
  token_suffix: string;
  description: string;
  created_at: string;
  last_used_at: string | null;
  revoked: boolean;
}

export function listRunnerTokens(): Promise<RunnerToken[]> {
  return getJSON<RunnerToken[]>("/runner-tokens");
}

/** The response's `token` field is the raw secret, shown exactly once — the
 *  server never returns it again on any later list call. */
export function createRunnerToken(description: string): Promise<RunnerToken> {
  return postJSON<RunnerToken>("/runner-tokens", { description });
}

export function revokeRunnerToken(id: number): Promise<void> {
  return requestVoid("POST", `/runner-tokens/${id}/revoke`);
}

// --- Config templates (for include:) ---

export function listRepoTemplates(repo: string): Promise<string[]> {
  return getJSON<string[]>(`/repo-templates?repo=${encodeURIComponent(repo)}`);
}

export function getRepoTemplate(
  repo: string,
  name: string
): Promise<{ repo: string; name: string; yaml: string }> {
  return getJSON<{ repo: string; name: string; yaml: string }>(
    `/repo-templates?repo=${encodeURIComponent(repo)}&name=${encodeURIComponent(name)}`
  );
}

export function putRepoTemplate(
  repo: string,
  name: string,
  yaml: string
): Promise<void> {
  return requestVoid("PUT", "/repo-templates", { repo, name, yaml });
}

// --- Stats / dashboard ---

export interface StatsNow {
  running_jobs: number;
  pending_jobs: number;
  blocked_jobs: number;
  online_runners: number;
  active_executors: number;
  pipelines_today: number;
  /** -1 = no data in the window */
  success_rate_24h: number;
  /** runs the rate is computed over (success + failed); 0 = nothing finished */
  finished_24h?: number;
}

export interface PipelineBucket {
  hour: string;
  success: number;
  failed: number;
  other: number;
}

export interface JobBucket {
  hour: string;
  count: number;
}

export interface PeriodTotals {
  pipelines: number;
  finished: number;
  succeeded: number;
  /** percent; -1 when nothing finished */
  success_rate: number;
  jobs_started: number;
}

export interface Stats {
  now: StatsNow;
  /** dense buckets over the range, oldest first */
  pipelines: PipelineBucket[];
  jobs: JobBucket[];
  range: { from: string; to: string; bucket_seconds: number; label: string };
  totals: PeriodTotals;
  /** the same-length window before this one */
  previous?: PeriodTotals;
}

export function getStats(range = "24h"): Promise<Stats> {
  return getJSON<Stats>(`/stats?range=${encodeURIComponent(range)}`);
}

// ---- runner history and webhook health ----

/** A time range as the history endpoints take it: a preset, or custom from/to (RFC 3339). */
export interface RangeQuery {
  range: string;
  from?: string;
  to?: string;
}

function rangeSearch(r: RangeQuery, extra: Record<string, string | undefined> = {}): string {
  const q = new URLSearchParams({ range: r.range });
  if (r.range === "custom") {
    if (r.from) q.set("from", r.from);
    if (r.to) q.set("to", r.to);
  }
  for (const [k, v] of Object.entries(extra)) if (v) q.set(k, v);
  return q.toString();
}

export interface HistoryBucket {
  at: string;
  /** the history doesn't cover this bucket (not rolled up): a gap, not a zero */
  no_data?: boolean;
  jobs_started: number;
  busy_seconds: number;
  busy_slots: number;
  /** percentiles are null when nothing was measured in the bucket */
  wait_p50: number | null;
  wait_p95: number | null;
  duration_p50: number | null;
  duration_p95: number | null;
  finished: number;
  failed: number;
  /** fleet capacity and queue depth from per-minute snapshots; null where none fell in the bucket */
  capacity_total: number | null;
  capacity_available: number | null;
  queue_pending_avg: number | null;
  queue_pending_max: number | null;
  queue_oldest_wait_max: number | null;
}

export interface HistorySummary {
  jobs_started: number;
  busy_seconds: number;
  wait_p50: number | null;
  wait_p95: number | null;
  duration_p50: number | null;
  duration_p95: number | null;
  finished: number;
  failed: number;
  capacity_available: number | null;
  capacity_total: number | null;
  queue_pending_avg: number | null;
  queue_pending_max: number | null;
  queue_oldest_wait_max: number | null;
  /** slot-seconds available over the snapshots, and how much of the range they cover */
  available_slot_seconds: number;
  snapshot_seconds: number;
}

export interface RunnerHistoryResponse {
  range: { from: string; to: string; bucket_seconds: number; label: string };
  buckets: HistoryBucket[];
  summary: HistorySummary;
}

export interface HistoryFilter {
  runner?: string;
  repo?: string;
  job?: string;
}

export function getRunnerHistory(range: RangeQuery, filter: HistoryFilter = {}): Promise<RunnerHistoryResponse> {
  return getJSON<RunnerHistoryResponse>(`/runner-history?${rangeSearch(range, { ...filter })}`);
}

export function getHistoryJobs(from: string, to: string, filter: HistoryFilter = {}, limit?: number): Promise<JobRow[]> {
  const q = new URLSearchParams({ from, to });
  if (filter.runner) q.set("runner", filter.runner);
  if (filter.repo) q.set("repo", filter.repo);
  if (filter.job) q.set("job", filter.job);
  if (limit) q.set("limit", String(limit));
  return getJSON<{ jobs: JobRow[] }>(`/runner-history/jobs?${q.toString()}`).then((r) => r.jobs ?? []);
}

/** A runner operation, maintenance window or Forge deploy on the time axis. */
export interface RunnerEvent {
  at: string;
  action: string;
  target: string;
  /** admin-only */
  actor?: string;
  detail?: Record<string, unknown>;
}

export function getRunnerEvents(range: RangeQuery, runner?: string): Promise<RunnerEvent[]> {
  return getJSON<{ events: RunnerEvent[] }>(`/runner-events?${rangeSearch(range, { runner })}`).then((r) => r.events ?? []);
}

export interface WebhookHealth {
  repo: string;
  accepted: number;
  rejected: number;
  errors: number;
  duplicates: number;
  sources: number;
  failing_sources?: string[];
  last_at?: string;
}

export interface WebhookEntry {
  id: number;
  received_at: string;
  provider: string;
  repo?: string;
  event?: string;
  status: number;
  reason?: string;
  delivery_id?: string;
  source_ip?: string;
  pipeline_id?: number;
}

export function getWebhookLog(repo: string): Promise<{ health: WebhookHealth; entries: WebhookEntry[] }> {
  return getJSON<{ health: WebhookHealth; entries: WebhookEntry[] }>(`/webhook-log?repo=${encodeURIComponent(repo)}`);
}

export function getFailingWebhooks(): Promise<WebhookHealth[]> {
  return getJSON<{ repos: WebhookHealth[] }>("/webhook-log/failing").then((r) => r.repos ?? []);
}

// --- Auth / SSO ---

export interface AuthUser {
  email: string;
  name: string;
  provider: string;
  expires_at: string | null;
  /** true in open mode (everyone) and for admin sessions when SSO is enforced */
  is_admin?: boolean;
}

export interface AuthProviderInfo {
  name: string;
  label: string;
  /** redirect: a button to the IdP; password: a username/password form (LDAP) */
  kind: "redirect" | "password";
}

export interface AuthProviders {
  providers: string[];
  provider_info?: AuthProviderInfo[];
  enforced: boolean;
}

/** Signs in with a directory (LDAP / Active Directory) account. */
export async function ldapLogin(username: string, password: string): Promise<void> {
  mutated();
  const res = await fetch(`${BASE}/auth/ldap/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({ username, password }),
  });
  if (!res.ok) throw await parseError(res);
}

/** Public endpoint; bypasses the central 401 redirect. */
export async function getAuthProviders(): Promise<AuthProviders> {
  const res = await fetch(`${BASE}/auth/providers`);
  if (!res.ok) {
    throw new ApiError(res.status, `Request failed with status ${res.status}`);
  }
  return (await res.json()) as AuthProviders;
}

let meCache: Promise<AuthUser | null> | null = null;

/**
 * Current session identity, or null when signed out. 401 here is normal in
 * open mode, so this bypasses the central redirect. Cached per page load.
 */
export function getMe(force = false): Promise<AuthUser | null> {
  if (!meCache || force) {
    meCache = (async () => {
      try {
        const res = await fetch(`${BASE}/auth/me`);
        if (!res.ok) return null;
        return (await res.json()) as AuthUser;
      } catch {
        return null;
      }
    })();
  }
  return meCache;
}

let isAdminCache: Promise<boolean> | null = null;

/**
 * Whether the current viewer has admin rights. In open mode (SSO not
 * enforced) everyone is admin — /auth/me 401s with no session, so we derive
 * it from the providers state; when enforced, use the session's is_admin.
 */
export function getIsAdmin(force = false): Promise<boolean> {
  if (!isAdminCache || force) {
    isAdminCache = (async () => {
      try {
        const providers = await getAuthProviders();
        if (!providers.enforced) return true;
        const me = await getMe(force);
        return me?.is_admin ?? false;
      } catch {
        return false;
      }
    })();
  }
  return isAdminCache;
}

/** Full-page navigation target for starting an IdP login (302s to the IdP). */
export function loginUrl(provider: string, next?: string): string {
  const base = `${BASE}/auth/login/${encodeURIComponent(provider)}`;
  return next && next.startsWith("/") && !next.startsWith("//") ? `${base}?next=${encodeURIComponent(next)}` : base;
}

/** Extends the current session; returns the new expiry. */
export function refreshSession(): Promise<{ expires_at: string }> {
  return postJSON<{ expires_at: string }>("/auth/refresh", {});
}

export async function logout(): Promise<void> {
  mutated();
  await fetch(`${BASE}/auth/logout`, { method: "POST" });
  meCache = null;
}

export interface SsoProviderConfig {
  provider: string;
  enabled: boolean;
  client_id: string;
  has_secret: boolean;
  tenant: string;
  allowed_domain: string;
  settings?: Record<string, string>;
}

export interface SsoConfig {
  providers: SsoProviderConfig[];
  redirect_uris: Record<string, string>;
}

export function getSsoConfig(): Promise<SsoConfig> {
  return getJSON<SsoConfig>("/sso");
}

export interface SsoProviderInput {
  provider: string;
  enabled: boolean;
  client_id: string;
  /** omit / empty to keep the stored secret */
  client_secret?: string;
  tenant?: string;
  allowed_domain?: string;
  settings?: Record<string, string>;
}

export interface GroupMapping {
  id: number;
  provider: string;
  group: string;
  platform_admin: boolean;
  repo_pattern: string;
  role: string;
  created_at: string;
}

export function listGroupMappings(): Promise<GroupMapping[]> {
  return getJSON<{ mappings: GroupMapping[] }>("/identity/group-mappings").then((r) => r.mappings ?? []);
}

export function createGroupMapping(m: Omit<GroupMapping, "id" | "created_at">): Promise<GroupMapping> {
  return postJSON<GroupMapping>("/identity/group-mappings", m);
}

export function deleteGroupMapping(id: number): Promise<void> {
  return requestVoid("DELETE", `/identity/group-mappings/${id}`);
}

export interface SessionSettings {
  lifetime_hours: number;
  min_hours: number;
  max_hours: number;
}

export function getSessionSettings(): Promise<SessionSettings> {
  return getJSON<SessionSettings>("/identity/session-settings");
}

export function putSessionSettings(lifetime_hours: number): Promise<SessionSettings> {
  return putJSON<SessionSettings>("/identity/session-settings", { lifetime_hours });
}

export function deleteProtectedEnvironment(id: number): Promise<void> {
  return requestVoid("DELETE", `/protected-environments/${id}`);
}

export function putSsoProvider(input: SsoProviderInput): Promise<void> {
  return requestVoid("PUT", "/sso", input);
}

/** Human-readable byte size, e.g. "1.4 MB". */
export function humanSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

/** Decode a repo route param; react-router usually pre-decodes. */
export function decodeRepoParam(raw: string): string {
  try {
    return raw.includes("%") ? decodeURIComponent(raw) : raw;
  } catch {
    return raw;
  }
}

export function getPipeline(id: number | string): Promise<PipelineDetail> {
  return getJSON<PipelineDetail>(`/pipelines/${id}`);
}

export async function getJobLogs(id: number | string): Promise<string> {
  const res = await fetch(`${BASE}/jobs/${id}/logs`);
  if (!res.ok) {
    throw await parseError(res);
  }
  return res.text();
}

export interface LogChunk {
  bytes: string;
  next_offset: number;
  eof: boolean;
}

/** Incremental log fetch from a byte offset (poll-fallback path). */
export async function getJobLogsIncremental(
  id: number | string,
  offset: number
): Promise<LogChunk> {
  const res = await fetch(`${BASE}/jobs/${id}/logs?offset=${offset}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as LogChunk;
}

/** URL for the SSE log stream (EventSource can't set headers; cookie auth flows automatically). */
export function jobLogStreamUrl(id: number | string, offset = 0): string {
  return `${BASE}/jobs/${id}/logs/stream?offset=${offset}`;
}

export function submitApproval(
  jobId: number | string,
  body: ApprovalRequest
): Promise<{ job: Job }> {
  return postJSON<{ job: Job }>(`/jobs/${jobId}/approvals`, body);
}

/** Release a manual-gated blocked job (blocked → created). */
export function playJob(jobId: number | string): Promise<{ job: Job }> {
  return postJSON<{ job: Job }>(`/jobs/${jobId}/play`, {});
}

/** Cancel every non-terminal job in a pipeline. Idempotent — a finished
 * pipeline is a no-op returning its current state. */
export function cancelPipeline(
  pipelineId: number | string
): Promise<{ pipeline: Pipeline; jobs: Job[] }> {
  return postJSON<{ pipeline: Pipeline; jobs: Job[] }>(
    `/pipelines/${pipelineId}/cancel`,
    {}
  );
}

/** Cancel a single job by its current state. Idempotent for terminal jobs; a
 * running job is signaled to stop via its next heartbeat. */
export function cancelJob(jobId: number | string): Promise<{ job: Job }> {
  return postJSON<{ job: Job }>(`/jobs/${jobId}/cancel`, {});
}

/** Create a NEW pipeline that replays a prior one's exact config_yaml — same
 * repo/ref/sha and commit identity, not a re-resolve against whatever the
 * registry has drifted to since. Jobs are immutable history: this never
 * resurrects the original's job rows, it starts a fresh run. */
export function retryPipeline(
  pipelineId: number | string
): Promise<{ pipeline: Pipeline }> {
  return postJSON<{ pipeline: Pipeline }>(`/pipelines/${pipelineId}/retry`, {});
}

export function createPipeline(
  body: CreatePipelineRequest
): Promise<{ pipeline: Pipeline }> {
  return postJSON<{ pipeline: Pipeline }>("/pipelines", body);
}

/** Terminal states: no more polling needed once a job/pipeline reaches one. */
export function isTerminalStatus(status: string): boolean {
  return status === "success" || status === "failed" || status === "canceled";
}

/** Relative time like "3m ago". */
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const diffSec = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (diffSec < 60) return `${diffSec}s ago`;
  const min = Math.floor(diffSec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}

/** Human duration from two ISO timestamps, e.g. "1m 12s". */
export function duration(start: string | null, end: string | null): string | null {
  if (!start || !end) return null;
  const s = new Date(start).getTime();
  const e = new Date(end).getTime();
  if (Number.isNaN(s) || Number.isNaN(e) || e < s) return null;
  const totalSec = Math.round((e - s) / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const min = Math.floor(totalSec / 60);
  const sec = totalSec % 60;
  if (min < 60) return `${min}m ${sec}s`;
  const hr = Math.floor(min / 60);
  return `${hr}h ${min % 60}m`;
}

export function shortSha(sha: string): string {
  return sha.slice(0, 8);
}

/**
 * Artifact expiry label: null/absent = "no expiry", a future time =
 * "expires in 3d", a past time = "expired 2h ago".
 */
export function expiryLabel(iso: string | null | undefined): string {
  if (!iso) return "no expiry";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const diffSec = Math.round((t - Date.now()) / 1000);
  const abs = Math.abs(diffSec);
  let mag: string;
  if (abs < 60) mag = `${abs}s`;
  else if (abs < 3600) mag = `${Math.floor(abs / 60)}m`;
  else if (abs < 86400) mag = `${Math.floor(abs / 3600)}h`;
  else mag = `${Math.floor(abs / 86400)}d`;
  return diffSec < 0 ? `expired ${mag} ago` : `expires in ${mag}`;
}

/** Human test duration from JUnit seconds, e.g. "1m 12s" or "0.4s". */
export function testDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "";
  if (seconds < 60) {
    return seconds < 10 ? `${seconds.toFixed(1)}s` : `${Math.round(seconds)}s`;
  }
  const min = Math.floor(seconds / 60);
  const sec = Math.round(seconds % 60);
  return `${min}m ${sec}s`;
}

// --- Permissions (UI hint; the server re-checks every action) ---

export interface Permissions {
  is_admin: boolean;
  /** retry pipelines and play manual jobs on this repo */
  can_run: boolean;
  /** cancel pipelines and jobs on this repo (same rule as can_run) */
  can_cancel: boolean;
}

const permCache = new Map<string, Promise<Permissions>>();

/** What the viewer may do on repo. Cached per page load; on error it falls back
 * to showing nothing rather than buttons that will 403. */
export function getPermissions(repo: string): Promise<Permissions> {
  let p = permCache.get(repo);
  if (!p) {
    p = getJSON<Permissions>(`/permissions?repo=${encodeURIComponent(repo)}`).catch(
      () => ({ is_admin: false, can_run: false, can_cancel: false })
    );
    permCache.set(repo, p);
  }
  return p;
}

// --- Personal API tokens ---

/** A personal API token. token is only set in the create response. */
export interface ApiToken {
  id: number;
  token?: string;
  token_suffix: string;
  owner: string;
  owner_name: string;
  description: string;
  created_at: string;
  /** null = never expires */
  expires_at: string | null;
  last_used_at: string | null;
  revoked: boolean;
  status: "active" | "expired" | "revoked" | "disabled";
}

export interface TokenPolicy {
  allow_no_expiry: boolean;
  /** 0 = no cap */
  max_lifetime_days: number;
}

export function listApiTokens(all = false): Promise<ApiToken[]> {
  return getJSON<ApiToken[]>(`/api-tokens${all ? "?all=1" : ""}`);
}

export function createApiToken(body: {
  description: string;
  expires_in_days?: number;
  no_expiry?: boolean;
}): Promise<ApiToken> {
  return postJSON<ApiToken>("/api-tokens", body);
}

export function revokeApiToken(id: number): Promise<void> {
  return requestVoid("POST", `/api-tokens/${id}/revoke`);
}

export function getTokenPolicy(): Promise<TokenPolicy> {
  return getJSON<TokenPolicy>("/api-token-policy");
}

export function putTokenPolicy(p: TokenPolicy): Promise<TokenPolicy> {
  return putJSON<TokenPolicy>("/api-token-policy", p);
}

// --- Pipeline list filters and saved views ---

export const PIPELINE_STATUSES = [
  "running",
  "blocked",
  "success",
  "failed",
  "canceled",
  "created",
] as const;

export interface PipelineFilter {
  repos?: string[];
  ref?: string;
  statuses?: string[];
  runner?: string;
  /** started_at window (RFC 3339), from the history drill-down */
  started_from?: string;
  started_to?: string;
  environment?: string;
}

export interface PipelinePage {
  items: Pipeline[];
  total: number;
  hasMore: boolean;
}

/** Query string for a filter, in the same shape the list page keeps in its URL. */
export function filterToParams(f: PipelineFilter): URLSearchParams {
  const qs = new URLSearchParams();
  for (const r of f.repos ?? []) qs.append("repo", r);
  if (f.ref) qs.set("ref", f.ref);
  for (const s of f.statuses ?? []) qs.append("status", s);
  if (f.runner) qs.set("runner", f.runner);
  if (f.environment) qs.set("environment", f.environment);
  if (f.started_from) qs.set("started_from", f.started_from);
  if (f.started_to) qs.set("started_to", f.started_to);
  return qs;
}

export function paramsToFilter(qs: URLSearchParams): PipelineFilter {
  return {
    repos: qs.getAll("repo").filter(Boolean),
    ref: qs.get("ref") ?? "",
    statuses: qs.getAll("status").filter(Boolean),
    runner: qs.get("runner") ?? "",
    environment: qs.get("environment") ?? "",
    started_from: qs.get("started_from") ?? "",
    started_to: qs.get("started_to") ?? "",
  };
}

/** One page of pipelines matching f; filtering happens on the server. */
export async function listPipelinesPage(
  f: PipelineFilter,
  offset: number,
  limit = 50
): Promise<PipelinePage> {
  const qs = filterToParams(f);
  qs.set("limit", String(limit));
  qs.set("offset", String(offset));
  const res = await fetch(`${BASE}/pipelines?${qs.toString()}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return {
    items: (await res.json()) as Pipeline[],
    total: Number(res.headers.get("X-Total-Count") ?? 0),
    hasMore: res.headers.get("X-Has-More") === "true",
  };
}

export interface PipelineFacets {
  repos: string[];
  refs: string[];
  runners: string[];
  environments: string[];
}

export function getPipelineFacets(repos: string[]): Promise<PipelineFacets> {
  const qs = new URLSearchParams();
  for (const r of repos) qs.append("repo", r);
  return getJSON<PipelineFacets>(`/pipeline-facets?${qs.toString()}`);
}

export interface PipelineView {
  id: number;
  owner: string;
  name: string;
  filters: PipelineFilter;
  shared: boolean;
  created_at: string;
  updated_at: string;
}

export function listPipelineViews(): Promise<PipelineView[]> {
  return getJSON<PipelineView[]>("/pipeline-views");
}

export function createPipelineView(body: {
  name: string;
  filters: PipelineFilter;
  shared: boolean;
}): Promise<PipelineView> {
  return postJSON<PipelineView>("/pipeline-views", body);
}

export function updatePipelineView(
  id: number,
  body: { name: string; filters: PipelineFilter; shared: boolean }
): Promise<PipelineView> {
  return putJSON<PipelineView>(`/pipeline-views/${id}`, body);
}

export function deletePipelineView(id: number): Promise<void> {
  return requestVoid("DELETE", `/pipeline-views/${id}`);
}

// --- Org-wide environment board and job list ---

export interface EnvAttempt {
  job_id: number;
  job_name: string;
  /** set when the environment policy refused the job */
  status_reason?: string;
  pipeline_id: number;
  status: string;
  manual: boolean;
  ref: string;
  sha: string;
  created_at: string;
}

export interface EnvCell {
  current: {
    id: number;
    sha: string;
    ref: string;
    pipeline_id: number;
    job_id: number;
    deployed_by: string;
    deployed_at: string;
  } | null;
  latest: EnvAttempt | null;
  frozen: boolean;
  /** what Forge Live sees running there */
  live?: LiveSummary;
}

export interface EnvBoardRow {
  repo: string;
  cells: Record<string, EnvCell>;
}

export interface EnvBoardPage {
  environments: string[];
  rows: EnvBoardRow[];
  total: number;
  hasMore: boolean;
}

async function getPage<T>(path: string): Promise<{ body: T; total: number; hasMore: boolean }> {
  const res = await fetch(`${BASE}${path}`, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    throw await parseError(res);
  }
  return {
    body: (await res.json()) as T,
    total: Number(res.headers.get("X-Total-Count") ?? 0),
    hasMore: res.headers.get("X-Has-More") === "true",
  };
}

/** One page of the repo × environment board (repos sorted by name). */
export async function getEnvironmentBoard(
  opts: { q?: string; env?: string; attention?: boolean },
  offset: number,
  limit = 50
): Promise<EnvBoardPage> {
  const qs = new URLSearchParams();
  if (opts.q) qs.set("q", opts.q);
  if (opts.env) qs.set("env", opts.env);
  if (opts.attention) qs.set("attention", "1");
  qs.set("limit", String(limit));
  qs.set("offset", String(offset));
  const { body, total, hasMore } = await getPage<{
    environments: string[];
    rows: EnvBoardRow[];
  }>(`/environment-board?${qs.toString()}`);
  return { ...body, total, hasMore };
}

export const JOB_STATUSES = [
  "running",
  "pending",
  "blocked",
  "failed",
  "success",
  "canceled",
  "created",
] as const;

export interface JobRow {
  id: number;
  pipeline_id: number;
  repo: string;
  ref: string;
  sha: string;
  name: string;
  stage: string;
  environment: string | null;
  status: string;
  manual: boolean;
  allow_failure: boolean;
  runner_id: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  status_reason?: string;
}

export interface JobFilter extends PipelineFilter {
  name?: string;
}

/** One page of jobs across all pipelines, filtered on the server. */
export async function listJobsPage(
  f: JobFilter,
  offset: number,
  limit = 50
): Promise<{ items: JobRow[]; total: number; hasMore: boolean }> {
  const qs = filterToParams(f);
  if (f.name) qs.set("name", f.name);
  qs.set("limit", String(limit));
  qs.set("offset", String(offset));
  const { body, total, hasMore } = await getPage<JobRow[]>(`/jobs?${qs.toString()}`);
  return { items: body, total, hasMore };
}

// --- Environment registry (admin-managed, per server) ---

export type EnvTier = "dev" | "staging" | "prod" | "other";

export interface RegisteredEnv {
  name: string;
  tier: EnvTier;
  aliases: string[];
  /** branch globs allowed to deploy here; empty = any branch */
  allowed_refs: string[];
  description: string;
  /** cancel a waiting deploy once a newer pipeline has deployed here */
  cancel_superseded?: boolean;
}

export interface EnvPolicy {
  environments: RegisteredEnv[];
  /** what happens to a job whose environment isn't registered */
  unknown: "allow" | "deny";
}

let envPolicyCache: Promise<EnvPolicy> | null = null;

/** This server's environment list. Cached per page load; empty on error. */
export function getEnvRegistry(force = false): Promise<EnvPolicy> {
  if (!envPolicyCache || force) {
    envPolicyCache = getJSON<EnvPolicy>("/environment-registry").catch(() => ({
      environments: [],
      unknown: "allow" as const,
    }));
  }
  return envPolicyCache;
}

export async function putEnvRegistry(p: EnvPolicy): Promise<EnvPolicy> {
  const saved = await putJSON<EnvPolicy>("/environment-registry", p);
  envPolicyCache = Promise.resolve(saved);
  return saved;
}

// --- Config validation ---

export interface LintJob {
  name: string;
  stage: string;
  environment?: string;
  needs: string[];
  image?: string;
  tags?: string[];
  manual?: boolean;
  allow_failure?: boolean;
  /** set when this server's environment rules would refuse the job on ref */
  refused?: string;
}

export interface LintResult {
  valid: boolean;
  error?: string;
  ref: string;
  stages: string[];
  jobs: LintJob[];
}

/** Compile config as a run on ref would, without running anything. */
export function lintConfig(body: { config: string; ref?: string; repo?: string }): Promise<LintResult> {
  return postJSON<LintResult>("/lint", body);
}

// --- Notifications (admin) ---

export interface NotifySettings {
  slack_webhook_url?: string;
  has_slack: boolean;
  email_enabled: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_password?: string;
  has_smtp_password: boolean;
  smtp_from: string;
  clear_slack?: boolean;
  /** last delivery per channel, when any has been attempted */
  delivery?: Record<string, DeliveryResult>;
  /** connection check run on save (email enabled) */
  smtp_check?: { ok: boolean; error?: string };
}

export interface DeliveryResult {
  ok: boolean;
  error?: string;
  at: string;
  consecutive_failures: number;
}

export function getNotifySettings(): Promise<NotifySettings> {
  return getJSON<NotifySettings>("/notification-settings");
}

export function putNotifySettings(s: Partial<NotifySettings>): Promise<NotifySettings> {
  return putJSON<NotifySettings>("/notification-settings", s);
}

export function testNotify(): Promise<{ sent: boolean; email_to: string }> {
  return postJSON<{ sent: boolean; email_to: string }>("/notification-settings/test", {});
}

/** Re-run one failed/canceled job (and its downstream jobs that didn't
 * succeed) inside the same pipeline. Returns the new job's id. */
/** Retries a job; debug reruns it with CI_DEBUG_TRACE=true (every command echoed). */
export function retryJob(jobId: number | string, debug = false): Promise<{ pipeline_id: number; job_id: number; retried: number }> {
  return postJSON<{ pipeline_id: number; job_id: number; retried: number }>(`/jobs/${jobId}/retry`, debug ? { debug: true } : {});
}

// --- Approver groups (admin) ---

export interface ApproverGroup {
  name: string;
  description: string;
  members: string[];
  updated_at: string;
}

export function listApproverGroups(): Promise<ApproverGroup[]> {
  return getJSON<ApproverGroup[]>("/approver-groups");
}

export function putApproverGroup(name: string, g: { description: string; members: string[] }): Promise<ApproverGroup> {
  return putJSON<ApproverGroup>(`/approver-groups/${encodeURIComponent(name)}`, g);
}

export function deleteApproverGroup(name: string): Promise<void> {
  return requestVoid("DELETE", `/approver-groups/${encodeURIComponent(name)}`);
}

// --- Service accounts (admin) ---

export interface ServiceAccount {
  id: number;
  name: string;
  identity: string;
  description: string;
  is_admin: boolean;
  disabled: boolean;
  created_by: string;
  created_at: string;
  active_tokens: number;
}

export function listServiceAccounts(): Promise<ServiceAccount[]> {
  return getJSON<ServiceAccount[]>("/service-accounts");
}

export function createServiceAccount(b: { name: string; description: string; is_admin: boolean }): Promise<ServiceAccount> {
  return postJSON<ServiceAccount>("/service-accounts", b);
}

export async function updateServiceAccount(
  id: number,
  b: { description?: string; is_admin?: boolean; disabled?: boolean }
): Promise<ServiceAccount> {
  mutated();
  const res = await fetch(`${BASE}/service-accounts/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(b),
  });
  if (!res.ok) throw await parseError(res);
  return (await res.json()) as ServiceAccount;
}

export function deleteServiceAccount(id: number): Promise<void> {
  return requestVoid("DELETE", `/service-accounts/${id}`);
}

export function listServiceTokens(id: number): Promise<ApiToken[]> {
  return getJSON<ApiToken[]>(`/service-accounts/${id}/tokens`);
}

export function createServiceToken(
  id: number,
  b: { description: string; expires_in_days?: number; no_expiry?: boolean }
): Promise<ApiToken> {
  return postJSON<ApiToken>(`/service-accounts/${id}/tokens`, b);
}

// --- Job log tools ---

export interface LogSummary {
  first_error?: string;
  first_error_line: number;
  context?: string[];
  tail: string[];
  lines: number;
}

export function getLogSummary(jobId: number | string): Promise<LogSummary> {
  return getJSON<LogSummary>(`/jobs/${jobId}/logs/summary`);
}

// --- Runner load ---

export interface QueueStat {
  tags: string[];
  pending: number;
  oldest_wait_seconds: number;
  eligible_online: number;
  eligible_busy: number;
}

export interface RunnerLoadRow {
  id: string;
  executor: string;
  tags: string[];
  paused: boolean;
  online: boolean;
  last_contact_at: string;
  concurrency: number;
  running: number;
  running_jobs: { job_id: number; pipeline_id: number; repo: string; name: string; started_at: string }[];
  /** jobs started, and busy seconds, within the load's from/to */
  jobs: number;
  busy_seconds: number;
}

export interface RunnerLoad {
  from: string;
  to: string;
  queues: QueueStat[];
  runners: RunnerLoadRow[];
  pending: number;
  running: number;
}

export function getRunnerLoad(range: RangeQuery = { range: "24h" }): Promise<RunnerLoad> {
  return getJSON<RunnerLoad>(`/runner-load?${rangeSearch(range)}`);
}

/** The runners page's live feed: {runners, load} on connect and on change. */
export function runnerStreamURL(range: RangeQuery): string {
  return `${BASE}/runners/stream?${rangeSearch(range)}`;
}

// ---- template catalog ----

export interface TemplateInput {
  name: string;
  default?: string;
  required: boolean;
  description?: string;
  options?: string[];
}

export interface CatalogTemplate {
  name: string;
  version: string;
  yaml: string;
  description: string;
  published_by: string;
  created_at: string;
  inputs: TemplateInput[];
}

/** Every published version, newest version first within each name. */
export function listCatalog(): Promise<CatalogTemplate[]> {
  return getJSON<{ templates: CatalogTemplate[] }>("/template-catalog").then((r) => r.templates ?? []);
}

export function publishCatalogTemplate(t: { name: string; version: string; yaml: string; description: string }): Promise<CatalogTemplate> {
  return postJSON<CatalogTemplate>("/template-catalog", t);
}

export function deleteCatalogTemplate(name: string, version: string): Promise<void> {
  return requestVoid("DELETE", `/template-catalog?name=${encodeURIComponent(name)}&version=${encodeURIComponent(version)}`);
}

// ---- notification routes ----

export interface NotificationRoute {
  id: number;
  name: string;
  kind: string;
  has_target: boolean;
  target_hint?: string;
  has_secret: boolean;
  events: string[];
  repo_pattern: string;
  enabled: boolean;
  created_at: string;
}

export interface NotificationRoutesResponse {
  routes: NotificationRoute[];
  kinds: string[];
  events: string[];
  links_enabled: boolean;
}

export function listNotificationRoutes(): Promise<NotificationRoutesResponse> {
  return getJSON<NotificationRoutesResponse>("/notification-routes");
}

export interface NotificationRouteInput {
  name: string;
  kind: string;
  target?: string;
  secret?: string;
  events: string[];
  repo_pattern: string;
  enabled: boolean;
}

export function createNotificationRoute(r: NotificationRouteInput): Promise<NotificationRoute> {
  return postJSON<NotificationRoute>("/notification-routes", r);
}

export function updateNotificationRoute(id: number, r: NotificationRouteInput): Promise<NotificationRoute> {
  return putJSON<NotificationRoute>(`/notification-routes/${id}`, r);
}

export function deleteNotificationRoute(id: number): Promise<void> {
  return requestVoid("DELETE", `/notification-routes/${id}`);
}

export function testNotificationRoute(id: number): Promise<{ sent: boolean }> {
  return postJSON<{ sent: boolean }>(`/notification-routes/${id}/test`, {});
}

// ---- runner pools (autoscaling) ----

export interface RunnerPool {
  id: number;
  name: string;
  provider: string;
  tags: string[];
  min_runners: number;
  max_runners: number;
  jobs_per_runner: number;
  scale_down_after_s: number;
  /** provider settings JSON; secret keys come back masked */
  settings?: string;
  enabled: boolean;
  created_at: string;
  current_runners: number;
  desired_runners: number;
  pending_jobs: number;
  last_action: string;
  last_error: string;
  last_scaled_at: string | null;
  last_busy_at: string | null;
  checked_at: string | null;
}

export interface RunnerPoolsResponse {
  pools: RunnerPool[];
  providers: string[];
}

export function listRunnerPools(): Promise<RunnerPoolsResponse> {
  return getJSON<RunnerPoolsResponse>("/runner-pools");
}

export type RunnerPoolInput = Pick<RunnerPool, "name" | "provider" | "tags" | "min_runners" | "max_runners" | "jobs_per_runner" | "scale_down_after_s" | "settings" | "enabled">;

export function createRunnerPool(p: RunnerPoolInput): Promise<RunnerPool> {
  return postJSON<RunnerPool>("/runner-pools", p);
}

export function updateRunnerPool(id: number, p: RunnerPoolInput): Promise<RunnerPool> {
  return putJSON<RunnerPool>(`/runner-pools/${id}`, p);
}

export function deleteRunnerPool(id: number): Promise<void> {
  return requestVoid("DELETE", `/runner-pools/${id}`);
}

export function evaluateRunnerPools(): Promise<RunnerPoolsResponse> {
  return postJSON<RunnerPoolsResponse>("/runner-pools/evaluate", {});
}


// ---- server status, about, maintenance, runner operations ----

export interface ServerStatus {
  version: string;
  maintenance?: { on: boolean; message?: string; by?: string; since?: string };
}

/** Public: no session needed, never redirects. */
export async function getServerStatus(): Promise<ServerStatus> {
  const res = await fetch(`${BASE}/status`, { headers: { Accept: "application/json" } });
  if (!res.ok) throw new ApiError(res.status, `Request failed with status ${res.status}`);
  return (await res.json()) as ServerStatus;
}

export interface AboutInfo {
  version: string;
  platform: string;
  runner_version: string;
  started_at: string;
  uptime_seconds: number;
  database_ok: boolean;
  schema?: { applied_at?: string; version?: string };
  maintenance?: { on: boolean; message?: string; by?: string; since?: string };
  downloads_served: string[];
}

export function getAbout(): Promise<AboutInfo> {
  return getJSON<AboutInfo>("/about");
}

export function setMaintenance(on: boolean, message: string): Promise<unknown> {
  return putJSON<unknown>("/maintenance", { on, message });
}

export function runnerOp(id: string, op: "drain" | "resume" | "upgrade"): Promise<{ running_jobs: number }> {
  return postJSON<{ running_jobs: number }>(`/runners/${encodeURIComponent(id)}/${op}`, {});
}

export interface Rollout {
  active: boolean;
  target: string;
  current?: string;
  done: string[];
  skipped: string[];
  started_at?: string;
  by?: string;
  note?: string;
}

export function getRollout(): Promise<Rollout> {
  return getJSON<Rollout>("/runners/rollout");
}

export function startRollout(): Promise<Rollout> {
  return postJSON<Rollout>("/runners/rollout", {});
}

export function stopRollout(): Promise<void> {
  return requestVoid("DELETE", "/runners/rollout");
}

// ---- code review (batch 7) ----

/** What a change's host supports natively; Forge provides the rest. The UI
 *  branches on these, never on the provider's name. */
export interface Capabilities {
  changes: boolean;
  native_changes: boolean;
  threads: boolean;
  suggestions: boolean;
  resolve: boolean;
  drafts: boolean;
  merge_methods: string[];
  /** the host can show and require forge/merge-gate on a commit */
  status_checks: boolean;
  /** Forge can mirror the host with git and show diffs */
  diffs: boolean;
}

/** One pull/merge request, from any host (an inbox row). */
export interface Change {
  id: number;
  repo: string;
  provider: string;
  /** the host's per-repo number (Forge's own for native changes) */
  number: string;
  title: string;
  author: string;
  source_branch: string;
  target_branch: string;
  state: "open" | "merged" | "closed";
  draft: boolean;
  head_sha: string;
  base_sha: string;
  labels: string[];
  /** the change on the host's web UI; absent when the host has none */
  url?: string;
  created_at?: string;
  updated_at?: string;
  synced_at?: string;
  revisions: number;
  /** reviewers whose latest decisive verdict is approve */
  approvals: number;
  changes_requested: number;
  /** newest Forge pipeline on the head commit */
  pipeline_id?: number;
  pipeline_status?: string;
  /** the merge gate on the head; absent when no merge rule applies */
  gate_state?: "pass" | "pending" | "fail";
  gate_summary?: string;
}

export interface ChangeRevision {
  number: number;
  head_sha: string;
  base_sha: string;
  pushed_at: string;
}

export interface ChangeReview {
  id: number;
  source: "host" | "forge";
  reviewer: string;
  verdict: "approve" | "request_changes" | "comment" | "dismissed";
  body: string;
  commit_sha?: string;
  submitted_at: string;
}

export interface ChangeDetail extends Change {
  body: string;
  refresh_error?: string;
  revision_list: ChangeRevision[];
  reviews: ChangeReview[];
}

export interface ChangeFilter {
  state?: string;
  repo?: string;
  q?: string;
  /** rows to return (default 200); the total is counted either way */
  limit?: number;
}

/** The inbox: open changes across every repo and host (Postgres only — no
 *  provider is called to render it). */
export async function listChanges(f: ChangeFilter = {}): Promise<{ changes: Change[]; total: number }> {
  const qs = new URLSearchParams();
  if (f.state) qs.set("state", f.state);
  if (f.repo) qs.set("repo", f.repo);
  if (f.q) qs.set("q", f.q);
  qs.set("limit", String(f.limit ?? 200));
  const res = await fetch(`${BASE}/changes?${qs}`, { headers: { Accept: "application/json" } });
  if (!res.ok) throw await parseError(res);
  return { changes: (await res.json()) as Change[], total: Number(res.headers.get("X-Total-Count") ?? 0) };
}

export interface GateRule {
  id: string;
  label: string;
  state: "pass" | "pending" | "fail";
  detail: string;
}

export interface GateResult {
  change_id: number;
  head_sha: string;
  policy_id?: number;
  state: "pass" | "pending" | "fail";
  summary: string;
  rules: GateRule[];
  evaluated_at: string;
  /** what the host's forge/merge-gate shows ("" = not published yet) */
  published_state: string;
  published_at?: string;
}

/** A queued write to the host (gate publish, merge). */
export interface ChangeAction {
  id: number;
  kind: "publish_gate" | "merge";
  state: "pending" | "done" | "failed" | "superseded";
  attempts: number;
  next_attempt_at: string;
  last_error?: string;
  result?: Record<string, string>;
  requested_by?: string;
  created_at: string;
  done_at?: string;
}

export interface ThreadComment {
  id: number;
  author: string;
  author_email?: string;
  body: string;
  source: "host" | "forge";
  /** sent: on the host; pending: queued for it; failed: it refused; local: the host has no comments (Forge keeps it) */
  state: "sent" | "pending" | "failed" | "local";
  error?: string;
  created_at: string;
}

/** A comment thread on a line of the diff, or the change's conversation (path ""). */
export interface ChangeThread {
  id: number;
  path: string;
  side: "new" | "old";
  /** current line; 0 when outdated or not on a line */
  line: number;
  start_line?: number;
  original_line?: number;
  commit_sha?: string;
  resolved: boolean;
  outdated: boolean;
  resolve_pending?: boolean;
  source: "host" | "forge";
  comments: ThreadComment[];
}

/** A change's place in its target's merge queue. */
export interface QueueEntry {
  id: number;
  change_id: number;
  repo: string;
  target_branch: string;
  head_sha: string;
  method: string;
  state: "queued" | "testing" | "merging" | "merged" | "failed" | "removed";
  base_sha?: string;
  test_sha?: string;
  pipeline_id?: number;
  error?: string;
  requested_by?: string;
  enqueued_at: string;
  started_at?: string;
  finished_at?: string;
  /** 1 = next (live entries) */
  position?: number;
  number?: string;
  title?: string;
}

export interface ChangePage {
  change: ChangeDetail;
  capabilities: Capabilities;
  gate: GateResult | null;
  actions: ChangeAction[];
  threads: ChangeThread[];
  /** enabled: the target merges through the queue; entry: the change's live
   *  (or latest, on this head) entry; length: how many are in line */
  queue: { enabled: boolean; entry?: QueueEntry; length: number };
  /** can_edit: a Forge-native change the caller may edit, close or reopen */
  permissions: { can_review: boolean; can_merge: boolean; can_comment: boolean; can_edit: boolean };
}

/** Comment on a line (path, side, line) or, without them, on the conversation. */
export function createThread(
  changeId: number,
  input: { path?: string; side?: "new" | "old"; line?: number; body: string; author?: string }
): Promise<{ thread_id: number; comment_id: number }> {
  return postJSON(`/changes/${changeId}/threads`, input);
}

export function replyThread(changeId: number, threadId: number, body: string, author?: string): Promise<{ comment_id: number }> {
  return postJSON(`/changes/${changeId}/threads/${threadId}/comments`, { body, author });
}

export function resolveThread(changeId: number, threadId: number, resolved: boolean): Promise<unknown> {
  return postJSON(`/changes/${changeId}/threads/${threadId}/resolve`, { resolved });
}

export interface HostLink {
  provider: string;
  login: string;
  scopes: string;
  created_at: string;
}

export function listHostLinks(): Promise<{ links: HostLink[]; github_linking: boolean }> {
  return getJSON("/account/links");
}

export function unlinkHost(provider: string): Promise<void> {
  return requestVoid("DELETE", `/account/links/${encodeURIComponent(provider)}`);
}

/** Where the "Link GitHub" button goes (a full-page OAuth redirect). */
export const linkGitHubURL = `${BASE}/auth/link/github`;

export function getChange(id: number | string): Promise<ChangePage> {
  return getJSON(`/changes/${id}`);
}

/** Approve, request changes, or withdraw ("dismiss") your verdict. */
export function reviewChange(
  id: number,
  verdict: "approve" | "request_changes" | "dismiss",
  body = "",
  reviewer?: string
): Promise<{ reviewer: string; verdict: string }> {
  return postJSON(`/changes/${id}/reviews`, { verdict, body, reviewer });
}

/** What each host supports natively, by provider name. */
export function getProviderCapabilities(): Promise<Record<string, Capabilities>> {
  return getJSON("/code-review/capabilities");
}

/** Open a Forge-native change (hosts without pull requests, e.g. plain git). */
export function createChange(input: {
  repo: string;
  source_branch: string;
  target_branch: string;
  title: string;
  body?: string;
  draft?: boolean;
}): Promise<{ id: number; number: string }> {
  return postJSON("/changes", input);
}

/** Edit, close or reopen a Forge-native change. */
export function editChange(
  id: number,
  patch: { title?: string; body?: string; draft?: boolean; state?: "open" | "closed" }
): Promise<void> {
  return requestVoid("PATCH", `/changes/${id}`, patch);
}

/** Add the change (exactly this head) to its target's merge queue. */
export function queueChange(id: number, expectedHead: string, method: string): Promise<{ entry: QueueEntry }> {
  return postJSON(`/changes/${id}/queue`, { expected_head: expectedHead, method });
}

export function dequeueChange(id: number): Promise<void> {
  return requestVoid("DELETE", `/changes/${id}/queue`);
}

/** Live merge queue entries in order (one repo's, or every repo's). */
export function listMergeQueue(repo?: string, target?: string): Promise<QueueEntry[]> {
  const qs = new URLSearchParams();
  if (repo) qs.set("repo", repo);
  if (target) qs.set("target", target);
  return getJSON(`/merge-queue${qs.size ? `?${qs}` : ""}`);
}

/** Queue a merge of exactly the head you reviewed. */
export function mergeChange(id: number, expectedHead: string, method: string): Promise<{ action: ChangeAction }> {
  return postJSON(`/changes/${id}/merge`, { expected_head: expectedHead, method });
}

export interface FreezeWindow {
  start: string;
  end: string;
}

export interface MergePolicy {
  id?: number;
  repo: string;
  branch_pattern: string;
  required_approvals: number;
  approver_roles: string[];
  required_group: string;
  required_group_approvals: number;
  allow_self_approval: boolean;
  dismiss_on_push: boolean;
  block_on_changes_requested: boolean;
  require_ci: boolean;
  required_jobs: string[];
  required_labels: string[];
  blocked_labels: string[];
  ticket_pattern: string;
  freeze_windows: FreezeWindow[];
  timezone: string;
  /** owners (CODEOWNERS on the target) of every touched file must approve */
  require_code_owners: boolean;
  /** merges go through the merge queue */
  merge_queue: boolean;
  updated_by?: string;
  updated_at?: string;
}

export function listMergePolicies(): Promise<MergePolicy[]> {
  return getJSON("/merge-policies");
}

export function putMergePolicy(p: MergePolicy): Promise<MergePolicy> {
  return putJSON("/merge-policies", p);
}

export function deleteMergePolicy(id: number): Promise<void> {
  return requestVoid("DELETE", `/merge-policies/${id}`);
}

export interface HostIdentity {
  provider: string;
  login: string;
  email: string;
  created_at?: string;
}

export function listHostIdentities(): Promise<HostIdentity[]> {
  return getJSON("/host-identities");
}

export function putHostIdentity(h: HostIdentity): Promise<void> {
  return requestVoid("PUT", "/host-identities", h);
}

export function deleteHostIdentity(provider: string, login: string): Promise<void> {
  return requestVoid("DELETE", `/host-identities/${encodeURIComponent(provider)}/${encodeURIComponent(login)}`);
}

export interface ProviderCallCount {
  provider: string;
  class: string;
  result: string;
  read_path: boolean;
  calls: number;
}

export interface ProviderBudget {
  credential: string;
  provider: string;
  capacity: number;
  remaining: number;
  reset_at?: string;
}

export interface CodeReviewStats {
  window_hours: number;
  calls: ProviderCallCount[];
  total_calls: number;
  not_modified: number;
  not_modified_ratio: number;
  /** provider calls attempted while serving a page — must be 0 */
  read_path_calls: number;
  open_changes: number;
  calls_per_open_change: number;
  budgets: ProviderBudget[];
  refresh_backlog: number;
  oldest_refresh_seconds: number;
  outbox_backlog: number;
  oldest_outbox_seconds: number;
  diffs: {
    reads: Record<string, number>;
    hit_ratio: number;
    computed: number;
    compute_p95_ms: number;
    mirror_fetches: number;
    stored_bytes: number;
    task_backlog: number;
    oldest_task_seconds: number;
  };
  diff_open_p95_ms: number;
  diff_open_samples: number;
  gates: { evaluations: number; blocked_by: Record<string, number> };
}

export function getCodeReviewStats(): Promise<CodeReviewStats> {
  return getJSON("/code-review/metrics");
}

/**
 * Live change updates over SSE: onChange(id) fires once per changed change,
 * so a page refetches only what moved (the server answers 304 when it did
 * not). Returns the unsubscribe function.
 */
export function subscribeChanges(onChange: (id: number) => void): () => void {
  if (typeof EventSource === "undefined") return () => undefined;
  const es = new EventSource(`${BASE}/changes/stream`);
  es.addEventListener("change", (e) => {
    try {
      onChange(Number(JSON.parse((e as MessageEvent).data).id));
    } catch {
      // a malformed frame is skipped; the next one refreshes the page
    }
  });
  return () => es.close();
}

/** A diff's status: ready (with its immutable manifest URL), pending while a
 *  runner computes it, failed, or unsupported on this host. */
export interface DiffStatus {
  status: "ready" | "pending" | "failed" | "unsupported";
  mode: "change" | "range";
  from: string;
  to: string;
  merge_base?: string;
  files: number;
  additions: number;
  deletions: number;
  error?: string;
  manifest_url?: string;
}

export interface DiffFileEntry {
  path: string;
  old_path?: string;
  status: "added" | "modified" | "deleted" | "renamed" | "copied" | "type_changed";
  additions: number;
  deletions: number;
  binary?: boolean;
  generated?: boolean;
  too_large?: boolean;
  /** the unified patch, inline when small */
  patch?: string;
  /** else: the chunk to fetch on expand, relative to the manifest */
  chunk?: string;
}

export interface DiffManifest {
  version: number;
  mode: string;
  from: string;
  to: string;
  merge_base: string;
  additions: number;
  deletions: number;
  truncated?: boolean;
  files: DiffFileEntry[];
}

/** The change's diff against its target, or between two of its revisions. */
export function getChangeDiff(id: number, range?: { from: string; to: string }, retry = false): Promise<DiffStatus> {
  const qs = new URLSearchParams();
  if (range) {
    qs.set("from", range.from);
    qs.set("to", range.to);
  }
  if (retry) qs.set("retry", "1");
  const q = qs.toString();
  return getJSON(`/changes/${id}/diff${q ? `?${q}` : ""}`);
}

/** Manifests and chunks are immutable: the browser caches them forever. */
export async function getDiffManifest(url: string): Promise<DiffManifest> {
  const res = await fetch(url, { headers: { Accept: "application/json" } });
  if (!res.ok) throw await parseError(res);
  return (await res.json()) as DiffManifest;
}

export async function getDiffChunk(manifestUrl: string, chunk: string): Promise<string> {
  const url = manifestUrl.replace(/manifest\.json$/, chunk);
  const res = await fetch(url);
  if (!res.ok) throw await parseError(res);
  return res.text();
}

// ---- Forge Live (batch 9) ----
// Pages read Forge's copy of what runs (kept current by forge-agent); they
// never reach a cluster or a cloud API.

export type Health = "Healthy" | "Progressing" | "Degraded" | "Suspended" | "Missing" | "Unknown";

export interface LiveSummary {
  apps: number;
  /** the worst of the apps' health */
  health: Health;
  counts: Partial<Record<Health, number>>;
}

export interface LiveDeploy {
  sha: string;
  by: string;
  at: string;
  job_id: number;
  pipeline_id: number;
  message?: string;
}

export interface LiveApp {
  id: number;
  target_id: number;
  key: string;
  name: string;
  namespace: string;
  group: string;
  health: Health;
  health_message: string;
  health_since: string;
  sync: "Synced" | "OutOfSync" | "Unknown";
  resources: number;
  pods: number;
  pods_ready: number;
  images: string[];
  version: number;
  updated_at: string;
  target: string;
  environment: string;
  repo: string;
  provider: string;
  deployed: LiveDeploy | null;
  /** the desired state's revision (Git), when the target has a source */
  revision: string;
  revision_message: string;
  revision_author: string;
  revision_at: string | null;
  source_path: string;
  source_tool: string;
  source_error: string;
}

export interface LiveResource {
  group?: string;
  version?: string;
  kind: string;
  namespace?: string;
  name: string;
  uid: string;
  parents?: string[];
  health?: { status: Health; message?: string };
  /** compared with the desired state: absent when the resource is not in Git */
  sync?: "Synced" | "OutOfSync";
  summary: {
    ready?: string;
    status?: string;
    restarts?: number;
    images?: string[];
    info?: { name: string; value: string }[];
  };
  created_at: string;
}

export interface LiveCapabilities {
  events: boolean;
  manifests: boolean;
  watch: boolean;
  reconcile_seconds?: number;
  diff: boolean;
  sync: boolean;
  rollback: boolean;
  logs: boolean;
  pods: boolean;
  network: boolean;
}

/** A resource's key, as desired manifests are filed: group/kind/namespace/name. */
export function resourceKey(r: Pick<LiveResource, "group" | "kind" | "namespace" | "name">): string {
  return `${r.group ?? ""}/${r.kind}/${r.namespace ?? ""}/${r.name}`;
}

export interface LiveAppView {
  app: LiveApp;
  tree: { version: number; resources: LiveResource[] };
  target: {
    name: string;
    provider: string;
    capabilities: LiveCapabilities;
    agent_version: string;
    last_seen_at: string | null;
    synced: boolean;
  };
}

export interface LiveEvent {
  uid: string;
  resource_uid: string;
  kind: string;
  name: string;
  type: string;
  reason: string;
  message: string;
  count: number;
  first_seen: string;
  last_seen: string;
}

export type LiveFacet = "health" | "sync" | "environment" | "repo" | "target" | "namespace" | "group";

export interface LiveAppsPage {
  apps: LiveApp[];
  total: number;
  facets: Record<LiveFacet, Record<string, number>>;
}

/** The Applications page: one page of apps, the total and the facet counts. */
export function listLiveApps(filter: Partial<Record<LiveFacet, string[]>> & { q?: string }): Promise<LiveAppsPage> {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(filter)) {
    if (Array.isArray(v)) v.forEach((x) => qs.append(k, x));
    else if (v) qs.set(k, v);
  }
  const q = qs.toString();
  return fetchJSON(`/live/apps${q ? `?${q}` : ""}`);
}

export function getLiveApp(id: number): Promise<LiveAppView> {
  return fetchJSON(`/live/apps/${id}`);
}

export function getLiveEvents(id: number, uid?: string): Promise<LiveEvent[]> {
  return fetchJSON(`/live/apps/${id}/events${uid ? `?uid=${encodeURIComponent(uid)}` : ""}`);
}

export function getLiveManifest(id: number, uid: string): Promise<unknown> {
  return fetchJSON(`/live/apps/${id}/manifests/${encodeURIComponent(uid)}`);
}

/** environment → what runs there, for one repo (the pipeline graph's Live nodes). */
export function getLiveSummary(repo: string): Promise<Record<string, LiveSummary>> {
  return getJSON(`/live/summary?repo=${encodeURIComponent(repo)}`);
}

export interface LivePatch {
  app: number;
  base: number;
  version: number;
  row: LiveApp;
  upserts: LiveResource[];
  deletes: string[];
}

/**
 * Follows Forge Live: app summaries as they change (and removals), plus one
 * app's tree patches when app is given. onReady runs on every (re)connect —
 * the page refetches then, so nothing missed while away stays missed.
 */
export function subscribeLive(
  app: number | undefined,
  on: {
    ready?: () => void;
    app?: (row: LiveApp) => void;
    gone?: (id: number) => void;
    patch?: (p: LivePatch) => void;
    op?: (o: LiveOp) => void;
  }
): () => void {
  if (typeof EventSource === "undefined") return () => undefined;
  const es = new EventSource(`${BASE}/live/stream${app ? `?app=${app}` : ""}`);
  const handle = <T,>(name: string, fn?: (v: T) => void) =>
    es.addEventListener(name, (e) => {
      try {
        fn?.(JSON.parse((e as MessageEvent).data) as T);
      } catch {
        // a malformed frame is skipped; the next one, or a reconnect, catches up
      }
    });
  es.addEventListener("ready", () => on.ready?.());
  handle<LiveApp>("app", on.app);
  handle<{ id: number }>("gone", on.gone && ((g) => on.gone!(g.id)));
  handle<LivePatch>("patch", on.patch);
  handle<LiveOp>("op", on.op);
  return () => es.close();
}

export interface LiveTarget {
  id: number;
  name: string;
  environment: string;
  repo: string;
  provider: string;
  capabilities: LiveCapabilities;
  agent_version: string;
  synced: boolean;
  stats: {
    objects: number;
    apps: number;
    heap_bytes: number;
    cpu_seconds: number;
    bytes_sent: number;
    provider_calls: number;
    uptime_seconds: number;
  };
  bytes_in: number;
  last_seen_at: string | null;
  created_by: string;
  created_at: string;
  token_suffix: string;
  apps: number;
}

export type LiveOpKind = "sync" | "rollback" | "refresh" | "logs";

export interface LiveOpParams {
  kind: LiveOpKind;
  revision?: string;
  prune?: boolean;
  dry_run?: boolean;
  force?: boolean;
  resources?: string[];
  hard?: boolean;
  image?: string;
  pod?: string;
  container?: string;
  tail?: number;
  follow?: boolean;
}

export interface LiveOp {
  id: number;
  app_id: number;
  kind: LiveOpKind;
  state: "approval" | "pending" | "running" | "succeeded" | "failed" | "canceled";
  params: LiveOpParams;
  result: {
    ok: boolean;
    message: string;
    revision?: string;
    resources?: { key: string; kind: string; name: string; action: string; message?: string }[];
  } | null;
  message: string;
  revision: string;
  requested_by: string;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  approvals: { approver: string; verdict: string; comment: string; created_at: string }[];
  rule?: { required_approvals: number; approver_roles: string[] } | null;
}

/** Starts a sync, rollback, refresh or log stream on an application. */
export function createLiveOp(app: number, p: LiveOpParams): Promise<LiveOp> {
  return postJSON(`/live/apps/${app}/ops`, p);
}

export function getLiveOp(id: number): Promise<LiveOp> {
  return fetchJSON(`/live/ops/${id}`);
}

export function listLiveOps(app: number): Promise<LiveOp[]> {
  return fetchJSON(`/live/apps/${app}/ops`);
}

export function approveLiveOp(id: number, verdict: "approved" | "rejected", comment = ""): Promise<LiveOp> {
  return postJSON(`/live/ops/${id}/approve`, { verdict, comment });
}

export function cancelLiveOp(id: number): Promise<void> {
  return requestVoid("POST", `/live/ops/${id}/cancel`);
}

/** A resource's desired manifest (null when it has none). */
export async function getDesiredManifest(app: number, key: string): Promise<unknown | null> {
  try {
    return await fetchJSON(`/live/apps/${app}/desired?key=${encodeURIComponent(key)}`);
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

/** Follows an operation's output (pod logs); onEnd gets the finished operation. */
export function streamOpOutput(id: number, onChunk: (text: string) => void, onEnd: (op: LiveOp) => void): () => void {
  if (typeof EventSource === "undefined") return () => undefined;
  const es = new EventSource(`${BASE}/live/ops/${id}/output`);
  es.addEventListener("output", (e) => onChunk(JSON.parse((e as MessageEvent).data) as string));
  es.addEventListener("end", (e) => {
    es.close();
    onEnd(JSON.parse((e as MessageEvent).data) as LiveOp);
  });
  return () => es.close();
}

export function listLiveTargets(): Promise<LiveTarget[]> {
  return fetchJSON("/live/targets");
}

export function createLiveTarget(t: { name: string; environment: string; repo: string }): Promise<{ target: LiveTarget; token: string }> {
  return postJSON("/live/targets", t);
}

export function deleteLiveTarget(id: number): Promise<void> {
  return requestVoid("DELETE", `/live/targets/${id}`);
}
