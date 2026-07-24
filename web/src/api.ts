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
  repo: string;
  ref: string;
  sha: string;
  status: string;
  created_at: string;
  /** Derived per-stage statuses, ordered by stage_idx. */
  stages: StageStatus[];
  /** Registered config version this run used; null = one-off custom config. */
  config_version: number | null;
  /** Trigger: api | push | webhook | merge_request | schedule. */
  source?: string;
}

/** Human label for a pipeline trigger source; "" for the ordinary api/push case. */
export function sourceLabel(source?: string): string {
  switch (source) {
    case "merge_request":
      return "merge request";
    case "schedule":
      return "scheduled";
    default:
      return "";
  }
}

export interface Job {
  id: number;
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
}

export interface PipelineDetail {
  pipeline: Pipeline;
  jobs: Job[];
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

export type RepoProvider = "github" | "bitbucket" | "other";

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
}

export interface RepoSummary {
  repo: string;
  pipeline_count: number;
  success_count: number;
  failed_count: number;
  /** distinct refs seen, most recent first */
  refs: string[];
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
    window.location.assign("/login");
  }
  return new ApiError(res.status, message);
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) {
    throw await parseError(res);
  }
  return (await res.json()) as T;
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
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
  method: "PUT" | "POST" | "DELETE",
  path: string,
  body?: unknown
): Promise<void> {
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
}

export interface Environment {
  /** the environment name (server field is `environment`) */
  environment: string;
  repo?: string;
  current: {
    sha: string;
    ref: string;
    pipeline_id: number;
    deployed_by: string;
    deployed_at: string;
  } | null;
  deployment_count: number;
  drift: "in_sync" | "drifted" | "unknown";
  ref_tip_sha?: string;
  frozen?: boolean;
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

export interface Stats {
  now: StatsNow;
  /** exactly 24 hourly buckets, oldest first */
  pipelines: PipelineBucket[];
  jobs: JobBucket[];
}

export function getStats(): Promise<Stats> {
  return getJSON<Stats>("/stats");
}

// --- Auth / SSO ---

export interface AuthUser {
  email: string;
  name: string;
  provider: string;
  expires_at: string;
  /** true in open mode (everyone) and for admin sessions when SSO is enforced */
  is_admin?: boolean;
}

export interface AuthProviders {
  providers: string[];
  enforced: boolean;
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
export function loginUrl(provider: string): string {
  return `${BASE}/auth/login/${encodeURIComponent(provider)}`;
}

export async function logout(): Promise<void> {
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
