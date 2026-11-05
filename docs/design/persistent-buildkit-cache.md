# Design: persistent per-runner BuildKit cache

Status: **proposed, not implemented.** Cache affinity (`runner_affinity`) and
the registry layer cache are shipped; this is the remaining piece of
feature-request 1c.

## Goal

Container builds on docker/kubernetes runners reuse a warm local BuildKit
cache across jobs, so unchanged layers (and `RUN --mount=type=cache` caches
such as Go/npm/Maven) are not rebuilt or re-downloaded, without depending on
registry round-trips.

## Why it isn't shipped as-is

A BuildKit daemon (`buildkitd`) must run **privileged** (or rootless with
unconfined seccomp/AppArmor) and its API gives full access to everything it
has built and cached: layers, cache mounts, and any build secrets passed to
it. The obvious design — one `buildkitd` per runner host, reachable from job
containers at `tcp://…:1234` — lets **any job of any repo** on that runner:

- read another repo's cached layers and cache mounts (source code, built
  artifacts, credentials baked into intermediate layers);
- poison the cache another repo will reuse (a supply-chain risk);
- talk to the daemon without authentication.

So the safe version needs isolation and authentication, which is real work
and needs a couple of decisions from you.

## Proposed design

1. **One builder per (runner, repo)** — `forge-buildkit-<sha256(repo)[:12]>`,
   each with its own state volume. A repo can only ever reach its own cache.
2. **mTLS per builder.** The runner generates a CA and a client cert per
   builder; buildkitd listens with `--tlscacert/--tlscert/--tlskey`; the job
   gets the client cert in its run directory (`/var/run/forge`, the same
   mechanism as the refreshing OIDC token) and `BUILDKIT_HOST=tcp://…`,
   `BUILDX_BUILDER` config pointing at it.
3. **Network reach:** docker executor — connect the builder container to the
   job's network for the job's lifetime (`docker network connect`), then
   disconnect; kubernetes executor — a per-repo builder StatefulSet with a
   PVC and a Service restricted by NetworkPolicy to pods labelled with that
   repo.
4. **Opt-in per job:** `buildkit: persistent` (or a runner tag such as
   `docker-builder`), combined with `runner_affinity: true` so the same runner
   (and builder) is preferred.
5. **Lifecycle:** builders idle-stop after N minutes; `buildctl prune
   --keep-storage <limit>` on a schedule; runner flag
   `--buildkit-cache-limit` (default 20 GiB per repo).
6. **Secrets:** document that build secrets must use `--secret` (never `ARG`),
   since the cache persists.

## Decisions needed

- Is per-repo isolation enough, or must protected branches (production
  builds) get a builder separate from feature branches (to stop a feature
  branch poisoning the cache a production build reuses)? Recommended: yes —
  key builders by (repo, protected-or-not).
- Are privileged builder containers acceptable on your runner hosts, or should
  we require rootless BuildKit (slower, needs user namespaces)?
- Disk budget per runner.

Estimated size once decided: ~1 week (docker executor first, kubernetes after).
