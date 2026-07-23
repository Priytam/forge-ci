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
  sha: string;
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

export async function getRepoConfig(repo: string): Promise<RepoConfig | null> {
  try {
    return await getJSON<RepoConfig>(`/repo-configs?repo=${encodeURIComponent(repo)}`);
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null;
    throw err;
  }
}

export function putRepoConfig(repo: string, config: string): Promise<void> {
  return requestVoid("PUT", "/repo-configs", { repo, config });
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

export function submitApproval(
  jobId: number | string,
  body: ApprovalRequest
): Promise<{ job: Job }> {
  return postJSON<{ job: Job }>(`/jobs/${jobId}/approvals`, body);
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
