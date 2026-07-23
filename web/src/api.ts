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

export interface Pipeline {
  id: number;
  repo: string;
  ref: string;
  sha: string;
  status: string;
  created_at: string;
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

export function listPipelines(): Promise<Pipeline[]> {
  return getJSON<Pipeline[]>("/pipelines");
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
