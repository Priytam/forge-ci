import type { Runner, RunnerLoad } from "../../api";

/* Test fixtures: a small mixed fleet. */

export const NOW = Date.parse("2026-10-26T12:00:00Z");
const ago = (s: number) => new Date(NOW - s * 1000).toISOString();

export function runner(over: Partial<Runner> & { id: string }): Runner {
  return {
    executor: "docker",
    tags: ["docker"],
    description: "",
    paused: false,
    online: true,
    created_at: ago(30 * 86400),
    last_contact_at: ago(2),
    version: "4de70f1",
    running_jobs: 0,
    concurrency: 1,
    stale: false,
    ...over,
  };
}

export const FLEET: Runner[] = [
  runner({ id: "builder-1", running_jobs: 1 }),
  runner({ id: "builder-2", paused: true }),
  runner({ id: "builder-3", draining: true, running_jobs: 1 }),
  runner({ id: "k8s-1", executor: "kubernetes", tags: ["docker", "k8s"], concurrency: 4, running_jobs: 2 }),
  runner({ id: "gce-runner-dev", tags: ["gcp"], online: false, stale: true, last_contact_at: ago(12 * 86400) }),
  runner({ id: "gce-builder-1", tags: ["gcp"], version: "99e8c2c", outdated: true }),
  runner({ id: "gpu-1", tags: ["gpu"], online: false, last_contact_at: ago(600) }),
];

export function loadFor(runners: Runner[], from = NOW - 86400_000, to = NOW): RunnerLoad {
  return {
    from: new Date(from).toISOString(),
    to: new Date(to).toISOString(),
    queues: [],
    pending: 0,
    running: 0,
    runners: runners.map((r) => ({
      id: String(r.id),
      executor: r.executor,
      tags: r.tags,
      paused: r.paused,
      online: r.online,
      last_contact_at: r.last_contact_at,
      concurrency: r.concurrency ?? 1,
      running: r.running_jobs ?? 0,
      running_jobs: [],
      jobs: 12,
      busy_seconds: 43200,
    })),
  };
}
