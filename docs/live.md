# Forge Live: what your applications are doing, right now

After a deploy, one click shows the application as it runs — its health,
its resources as a tree, each resource's status, events and live manifest —
for Kubernetes and Cloud Run alike, with changes arriving as they happen.
The layout is the one people know from Argo CD (status panels, a
left-to-right resource tree, a slide-over per resource); everything is
Forge's own and lives inside Forge, next to the repo, its changes and its
pipelines. This is phase A of batch 9: seeing. Diff, sync and rollback come
next (see the end).

## Where it shows up

- **Applications** in the sidebar: every application on every cluster and
  project, as tiles or a list, filtered on the left (health, sync,
  environment, repo, cluster/project, namespace/region, part-of) with a count
  per value and a search box.
- **A repo's Applications tab**: what that repo runs as — so a repo's
  changes, pipelines, environments and running applications are all one click
  from the repo.
- **The pipeline graph**: every deploy job gets a **Live** node after it,
  with the environment's health ("2 of 100 degraded"); click it to see them.
- **The deploy job page**: *Open Live view* once the job has succeeded.
- **Environments** (the board and a repo's environment cards): a Live badge
  per environment.
- **Settings → Live agents**: add a cluster or project, get its agent token,
  see each agent's health (connected, objects watched, memory, bytes sent).

An application page has three status panels — **App health** (the worst of
its resources', naming the one at fault: `Pod indexer-…: ImagePullBackOff`),
**Sync status** (the revision Forge last deployed there, with its commit; the
comparison with Git arrives with Diff, so until then it reads *Unknown*) and
**Last sync** (that deploy job, who ran it, when) — then the resources as a
**Tree** or a **List**. Large fan-outs collapse ("+42 pods", click to expand).
Click a resource for **Summary** (status, conditions, images, owner),
**Events** and **Live manifest** (as YAML). Tabs follow what the provider can
show — its capabilities — never its name.

## How it works

```
cluster / cloud                         Forge server                         browser
┌──────────────────────┐   HTTPS POST   ┌──────────────────────────────┐ SSE ┌──────────┐
│ forge-agent          │ ─────────────▶ │ /live/agent/batches          │ ──▶ │ pages    │
│  provider (watch or  │  gzip batches, │  Postgres: apps, status,     │     │ (patches │
│  reconcile) → health │  one in flight │  events · blobs: trees,      │     │  only)   │
│  → tracker (coalesce)│  (epoch, seq)  │  manifests · LRU: hot trees  │     └──────────┘
└──────────────────────┘                └──────────────────────────────┘
```

- **The agent dials out; Forge holds no credentials.** `forge-agent` runs
  inside the cluster under a read-only ServiceAccount (or anywhere with a
  Cloud Run Viewer credential) and POSTs to Forge with a token scoped to its
  target. Forge needs no inbound access to any cluster.
- **Watch, not poll (Kubernetes).** One client-go reflector per resource type
  for the whole cluster — never per application. Each object is trimmed
  (managed fields, the last-applied annotation and data values over 1 KiB
  dropped), its health and summary computed, and then forgotten: the agent
  keeps a compact projection (under 800 bytes per object), not an informer cache
  of full objects. Lists are paged 500 at a time and applied page by page, so
  start-up never holds a whole kind in memory.
- **Health, tree and grouping are computed in the agent.** Rules follow Argo
  CD's semantics for Deployment, ReplicaSet, StatefulSet, DaemonSet, Pod,
  Service, Ingress, Job, HorizontalPodAutoscaler and PersistentVolumeClaim
  (Healthy, Progressing, Degraded — including CrashLoopBackOff and
  ImagePullBackOff — Suspended, Unknown). Top-level objects are grouped into
  applications by label (`--app-labels`, default
  `app.kubernetes.io/instance,app.kubernetes.io/name`; `--group-label`,
  default `app.kubernetes.io/part-of`; `--root-selector` to narrow);
  ReplicaSets and Pods follow their controller.
- **Compact, coalesced deltas with backpressure.** Changes wait at least
  250 ms per application and coalesce per resource (a pod that changes ten
  times is sent once, in its last state). One batch is in flight at a time;
  while it is, changes keep coalescing, and when 16 MiB is waiting the
  provider blocks — Kubernetes watches pause, lists page more slowly. Memory
  is bounded by the number of resources, never by the event rate.
- **Resume tokens.** Each run of the provider is an *epoch*; batches are
  numbered within it. A retried batch is acknowledged without being applied
  twice; a gap makes Forge ask for a fresh epoch. A fresh epoch streams in and
  merges; when it is complete each application sends the full list of its
  resource ids (`keep`) and Forge drops anything else, then the applications
  the epoch never mentioned.
- **The server is an aggregator.** Per application the indexable status is a
  Postgres row; the resource tree is a gzip'd blob, content-addressed, one per
  version (old versions are collected 10 minutes after they are replaced);
  each resource's live manifest is a blob of its own. Hot trees stay in an
  in-memory LRU (`LIVE_TREE_CACHE_BYTES`, default 256 MiB).
- **Pages never call a cluster or a cloud.** The server binary contains no
  provider code at all — `TestServerHasNoProviderCode` fails the build if
  `cmd/server` ever links the Kubernetes client, a cloud SDK or a provider
  package — and every live read route is walked by
  `TestLiveReadRoutesServeFromForgeOnly`.
- **Live pages get patches.** One SSE stream per page
  (`/api/v1/live/stream?app=`), fed by Postgres LISTEN/NOTIFY: summary rows as
  they change, and for an open application the patch from one tree version to
  the next (changed resources and removed ids only). A page that falls behind
  is disconnected and refetches on reconnect; a slow page never slows others.
- **Big trees stay fast.** Layout is pure and O(n log n) (2,000 resources:
  about 1 ms), recomputed only when the tree's shape changes — a health
  change moves nothing — and moved to a Web Worker above 5,000 resources,
  where it starts to cost more than a worker round trip. Only the nodes and
  edges in view are in the DOM.
- **Cloud Run** (phase A: a rate-budgeted conditional reconcile, as the plan
  allows). Every period (default 2 minutes; 15 seconds while a service is
  rolling out) the agent sends one `services.list` per region and, only for a
  service whose content changed, one `revisions.list`. Every call goes
  through a per-credential token bucket (`--calls-per-minute`, default 60).
  A service is an application; its revisions — those taking traffic, the
  latest created and ready, then up to five more — are its children, with the
  traffic split shown on each. The event-driven design (Cloud Audit Logs →
  Pub/Sub) replaces the timer later without changing what is reported.

## Providers

A provider implements `live.Provider` (`internal/live`): `Watch(ctx, sink)`
reports every resource once, calls `Synced`, then reports each change;
`Capabilities()` says what the UI may show (events, manifests, watch or
reconcile period). Every provider runs the same conformance suite
(`internal/live/livetest`, wired in `internal/live/conformance_test.go`)
against a fake of its platform: identified resources with valid health and
in-app parents, manifests exactly when declared, the right application
health with a reason, a warning event for a degraded app, a change sent as a
compact delta (not a resend), removal, and stable identity across fresh
reports.

| Provider | Package | Live state | Fake |
|---|---|---|---|
| Kubernetes | `internal/live/kube` | reflectors (watch) | client-go fake clientset |
| Cloud Run | `internal/live/cloudrun` | conditional reconcile | Cloud Run Admin API v2 fake |

## Running an agent

Add the cluster or project under **Settings → Live agents** (name,
environment, the repo it deploys from) and copy the token — it is shown
once. Then, for Kubernetes:

```bash
docker build -f deploy/Dockerfile.agent -t <registry>/forge-agent:<version> .
kubectl create namespace forge-live
kubectl -n forge-live create secret generic forge-agent \
  --from-literal=FORGE_URL=https://forge.example.com \
  --from-literal=FORGE_AGENT_TOKEN=<token>
kubectl apply -f deploy/live/agent.yaml   # set the image first
```

`deploy/live/agent.yaml` grants `get/list/watch` on the watched kinds only
(no Secrets), runs as non-root with a read-only filesystem, and limits the
agent to 150 MiB and 0.2 CPU with `GOMEMLIMIT=120MiB`.

For Cloud Run, anywhere with Application Default Credentials:

```bash
FORGE_URL=https://forge.example.com FORGE_AGENT_TOKEN=<token> \
  forge-agent --provider cloudrun --project acme-prod --regions asia-south1,us-central1
```

An application's environment and repo are its target's. That links a
cluster deployed from one GitOps repo — the common case — to that repo's
pipelines and Applications tab.

## Numbers

**Agent footprint, 100,000 watched objects** — `LIVE_LOAD=100000 go test
./internal/live/kube -run TestAgentFootprint -v`: a synthetic cluster of
2,000 apps of 50 objects (Deployment, two ReplicaSets, Service, ConfigMap,
HPA, 44 pods; realistic pod manifests with managed fields), fed through
the real provider with paged lists and watches, every delta shipped as the
agent does (JSON + gzip). Apple M-series, Go 1.27.

| | Budget | Measured |
|---|---|---|
| Heap at steady state | 150 MB | **74.0 MB** (776 B/object) |
| Heap peak during start-up | 150 MB | **103 MB** (paged lists, 120 MiB soft limit) |
| CPU at 200 pod updates/s | 0.2 vCPU | **0.09 vCPU** (including generating the updates) |
| Bytes sent at that rate | — | **300 B/s per 1k resources** |
| Initial report | — | 5.9 MB gzip, 3.9 s |

The default `go test` runs 20,000 objects and fails if the per-object cost
would not fit the budget at 100,000.

**Server and pages**

| | Measured |
|---|---|
| Read routes making provider calls | **0** (no provider code in the server binary) |
| 2,000-resource app, click → tree painted (20 runs, dev build) | **p50 41 ms, p95 53 ms** (budget 300 ms) |
| Expanding all 1,991 pods of it | 81 ms; 13–19 nodes in the DOM for a 131,880 px tree |
| Layout of 2,000 resources | ~1 ms |

**The lab** (`forge-kube-lab`: kind, 3 nodes, 100 apps in 10 namespaces):
the agent's initial report — 100 applications, 600 objects — reaches Forge
in under a second in 2–3 batches; the agent uses 5.5 MB of heap and sends
about 1 KB/s on average. `kubectl -n checkout scale deploy/cart-api
--replicas=3` reaches an open cart-api page **476 ms** later as one patch
(the Deployment at 2/3, its ReplicaSet and the new Pod); the app is back to
Healthy 3/3 on the page at 1.5 s. `payments/fraud-scorer` shows Degraded
(`CrashLoopBackOff`), `search/indexer` Degraded (`ImagePullBackOff`).

**Cloud Run:** an unchanged fleet costs one list call per region per
reconcile (720/day per region at the default period, shared by every
service in it) plus one call per service per change; the bucket is never
exceeded (`TestCallBudget`, `TestReconcileIsConditional`).

## Not yet (phase B and later)

Desired manifests (Git: plain, Helm, Kustomize, or the deploy record),
**Diff**, the **Sync** dialog, **History & rollback**, the Pods and Network
views and the Logs tab (phase B); drift detection, auto-sync, self-heal,
prune, sync waves (phase C); progressive delivery and fleet views (phase D).
The Cloud Run reconcile becomes event-driven (audit logs → Pub/Sub); per-app
source repos (beyond the target's) come with desired state.
