# Forge Live: what your applications are doing, right now

After a deploy, one click shows the application as it runs — its health,
its resources as a tree, each resource's status, events and live manifest —
for Kubernetes and Cloud Run alike, with changes arriving as they happen.
The layout is the one people know from Argo CD (status panels, a
left-to-right resource tree, a slide-over per resource); everything is
Forge's own and lives inside Forge, next to the repo, its changes and its
pipelines. Phases A and B of batch 9 are in: seeing (A), and comparing with
Git, syncing, rolling back, logs (B). What comes next is at the end.

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
**Sync status** (*Synced* or *Out of sync* against the app's directory in Git,
at which commit, rendered with which tool) and **Last sync** (the latest sync
or rollback from Forge — or the deploy job that last shipped it — who, when,
how it went) — then the resources as a **Tree**, **Pods** (by node),
**Network** (Ingress → Service → workload → Pods) or a **List**. Drifted
resources carry an amber sync mark; resources Git has and the cluster does
not show as *Missing*. Large fan-outs collapse ("+42 pods", click to expand).
Click a resource for **Summary** (status, conditions, images, owner),
**Events**, **Logs** (pods: streamed, filterable, container picker), **Live
manifest**, **Desired manifest** and **Diff** (as YAML).

The header has **Diff** (every drifted resource, live against Git: side by
side or inline, changed lines only, and by default only the fields Git sets,
so defaults and status are never "drift"), **Sync** (revision, resources,
Prune, Dry run, Force; it shows what will change before you confirm),
**History** (every sync, rollback and refresh, with *Roll back to this*) and
**Refresh** (the agent reads the source now). Tabs follow what the provider can
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

## Desired state, sync and rollback (phase B)

- **Desired state is rendered in the agent.** Give it the GitOps repo
  (`--source-url`, `--source-branch`, `--source-path` — default
  `apps/{group}/{name}` — and `GIT_TOKEN` if it needs one). It keeps a bare
  clone, fetches every `--source-poll` (30s) or on Refresh, and re-renders
  only the apps whose directory the new commits touched: `kustomize build`
  for a kustomization, `helm template` for a chart, the YAML files as they
  are otherwise. Git credentials stay in the cluster.
- **Drift is computed where both sides meet.** The agent compares each live
  top-level object with its desired manifest whenever either changes:
  *Synced* when the live object has every field Git sets with the same value
  (quantities by amount, `0.5` = `500m`), *Out of sync* otherwise. Defaults,
  status and fields the API server fills in never count. Desired objects of
  kinds the agent watches and the cluster lacks show as *Missing*. Desired
  manifests go to Forge (one blob per resource) for the Diff view.
- **Operations go through Forge.** Sync, rollback, refresh and logs are rows
  in `live_ops`; the target's agent long-polls for them (woken by Postgres
  LISTEN/NOTIFY), runs them and reports. One sync or rollback per app at a
  time. A sync or rollback to a **frozen** environment is refused; to a
  **protected** one it waits for the environment's approvals — the same rule,
  approver roles, required group and self-approval setting as deploy jobs.
  Every request and result is in the audit log.
- **Sync** is server-side apply as field manager `forge`, in dependency order
  (namespaces, config and storage, services, workloads, autoscalers,
  ingresses). Without Force, a field someone else owns (a `kubectl scale`)
  makes the apply fail and says so; Force takes it over. Prune deletes what
  Forge applied to the app that Git no longer has (see phase C). Dry run asks
  the API server and changes nothing.
- **Rollback** is a sync to an earlier revision of the source. The app then
  shows *Out of sync* against the branch tip until Git catches up — as it
  should.
- **Write access is opt-in.** `deploy/live/agent.yaml` is read-only (plus
  `pods/log`). Sync needs `deploy/live/agent-sync.yaml` and `--allow-sync`;
  without them the agent declares no Sync capability and the button is not
  shown.
- **Cloud Run**: the desired state is Forge's deploy record — the app is
  *Synced* when it runs the image digest Forge last deployed. Sync sets that
  image (a new revision takes the traffic); rollback sends all traffic to an
  earlier revision; dry run validates only. Both need `--allow-sync` and
  `roles/run.developer`.
- **Logs** stream from the cluster through the agent into Postgres (kept an
  hour, 8 MB and 15 minutes per stream at most) and to the page; leaving the
  tab stops the stream.

## Auto-sync, self-heal, waves, hooks and orphans (phase C)

- **Auto-sync** (the *Auto-sync* button on an app; `PUT
  /api/v1/live/apps/{id}/policy`) makes Forge sync each new revision by
  itself. **Prune** lets those syncs delete what Git dropped; **Self-heal**
  undoes changes made in the cluster. The policy is stored per target and app
  key (`live_app_policies`), so it outlives the app's row.
- **It is the same operation a person starts.** The controller runs on every
  server replica (`internal/live/hub/autosync.go`): it looks at out-of-sync
  apps with auto-sync on every 5 s, and at once when an agent batch leaves an
  app out of sync. What it starts is an ordinary `live_ops` row — requested by
  *Forge auto-sync*, marked `auto-sync` or `self-heal` — so the per-app lock,
  freezes, approvals, history and audit log all apply unchanged, and two
  replicas can never start the same sync. Automatic syncs apply with force:
  Git wins.
- **The policy, in full** (`decideAuto`, table-tested):
  - a revision not yet synced is synced;
  - a failed sync is retried after 5, 10, 20, 40 s — five attempts, then it
    stops and says why until a new revision lands or someone syncs by hand;
  - once a revision is synced, drift is left alone unless self-heal is on;
    then it is undone 5 s after the last sync, and more slowly (up to ~5 min)
    when it has had to heal more than three times in ten minutes — the page
    says something keeps changing the app;
  - nothing happens while a sync or rollback is in flight or the environment
    is frozen (the page says *Paused*);
  - a protected environment's auto-syncs wait for approval like anyone's, and
    self-heal is refused there (a heal that waits for a person is not one);
  - when all that differs is what auto-sync would not prune, it does nothing.
  The panel shows what auto-sync is doing or why it waits.
- **Rollback turns auto-sync off** (it would undo the rollback at once), and
  a sync to any revision other than Git's is refused while auto-sync is on.
- **Prune only removes what Forge put there.** A live top-level object Git
  does not have is *Out of sync — requires pruning* only when it carries
  field manager `forge`; anything someone else created (a StatefulSet's
  volume claims, a hand-made ConfigMap) is never pruned, manually or
  automatically. Prune runs after the last Sync wave, dependents first.
  The Sync status panel counts both kinds of difference ("1 changed or
  missing · 1 to prune").
- **Sync waves and hooks.** `forge.dev/sync-wave: "<n>"` orders resources
  (default 0); `forge.dev/hook: PreSync | Sync | PostSync | SyncFail` makes a
  resource a hook (usually a Job) and `forge.dev/hook-delete-policy:
  BeforeHookCreation | HookSucceeded | HookFailed` says when it goes
  (default BeforeHookCreation, so the next sync runs it again). Helm's
  `helm.sh/hook` (pre/post-install and -upgrade), `hook-weight` and
  `hook-delete-policy` are honoured too; test, delete and rollback hooks are
  never applied. A sync runs PreSync hooks, then each wave lowest first,
  then PostSync hooks; it waits for a wave to be healthy and its hooks to
  complete before going on (5 minutes per wave), stops at the first Degraded
  resource or failed hook, and then runs the SyncFail hooks. A dry run
  applies every step server-side and lists hooks as *would run*; a sync of
  selected resources runs no hooks. Hooks are never compared with Git or
  pruned, and the tree tags them. Progress streams to the sync dialog.
- **Orphaned resources.** With `--orphans` (the default) the agent also
  reports, per namespace, the top-level objects no application claims —
  never what an owner controls, what the root selector leaves out, or
  `kube-root-ca.crt`. The *Orphaned* toggle on an app's tree adds its
  namespace's orphans, dashed and tagged, followed live; their panels have
  Summary, Events and Live manifest. They are never listed as applications.

## Canary rollouts, templates and the fleet (phase D)

- **Canary rollouts, no controller in the cluster.** A `Rollout` in Git next
  to the app (never applied) turns a sync that changes a Deployment's pod
  template into a canary:

  ```yaml
  apiVersion: forge.dev/v1
  kind: Rollout
  metadata: {name: storefront}       # the Deployment it rolls out
  spec:
    steps: [{weight: 25}, {pause: {duration: 30s}}, {weight: 50}, {pause: {duration: 30s}}]
    analysis:                         # in the background, every interval
      interval: 5s
      failureLimit: 2                 # failed rounds in a row that abort
      health: {maxRestarts: 0}        # always on: a Degraded or restarted canary pod fails
      http: {path: /healthz, port: 8080, successRate: 0.95}
      prometheus: {address: http://prometheus:9090, query: 'rate(errors{deployment="{{canary}}"}[1m])', max: 0.05}
  ```

  The agent runs the new template as `<name>-canary` beside the stable
  Deployment. Its pods carry `forge.dev/track: canary` and still match the
  stable selector, so the app's Services send them traffic in proportion to
  their pods. A weight of *w*% puts round(*n*·*w*/(100−*w*)) canary pods
  beside *n* stable ones: at 25% of 4 that is one pod, about 20%, and the
  page shows the real share. The agent walks the steps while the analysis
  runs. HTTP checks go straight to the canary pods' IPs, which is why the
  agent runs in the cluster. When every step passes, it **promotes**: the
  stable Deployment rolls out the new template, then the canary goes. A
  failed analysis **aborts**: the canary goes, the stable version keeps
  serving untouched, and auto-sync waits for the next change. *Resume* (end
  a `pause: {}`), *Promote to 100%* and *Abort* are operations of their own,
  each in the history and the audit log. Abort also cleans up a canary left
  behind when an agent restarts mid-rollout. A rollout keeps its operation
  alive by heartbeat, so it can run longer than the 20-minute expiry. First
  deploys, replica-only changes and **rollbacks** apply directly: an
  emergency is not the time to go slowly. Canary objects are never compared
  with Git or pruned. Blue-green and traffic routing through an ingress or
  mesh are not built yet.
- **Auto-sync follows what an app renders to, not the commit.** Each render
  has a digest; auto-sync syncs when that changes and compares the syncs it
  ran by it. A commit to another app in the same repo moves the revision,
  changes nothing here, and does not retry a release that was aborted or
  undo drift that self-heal is not set to undo.
- **Renders see the whole tree** at the revision (extracted once and
  cached): a Kustomize overlay per cluster over a shared `../../base` works.
- **App templates** (*Applications → Templates*, admins) declare one
  application on every target they match, by name or environment.
  `{target}`, `{environment}` and `{name}` are filled in per target in the
  path and namespace, usually as a Kustomize overlay per cluster. Agents ask
  for their declared apps every 30 s and render them before anything of
  them exists: every object shows as Missing, and the template's auto-sync
  brings the app up, creating the namespace if asked. Each app gets the
  template's sync policy once. A change made to one app by hand stands, and
  a template edit only reaches apps nobody changed. A declared app joins
  its objects by labels even when the agent's root selector would leave
  them out. Deleting a template stops declaring the app: its resources stay
  in the cluster.
- **The fleet** (*Applications → Fleet*) lists every application by name,
  one column per cluster or project, with health, sync, the image it runs
  and ready pods. It filters by environment, template and name. A cell
  whose image differs from what most of the fleet runs is outlined
  (version skew), and each column says how many of its apps need
  attention. Pages read it from Postgres, like every live view.

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

Providers that act implement `live.Operator`: `Operate(ctx, op, out)` runs a
sync, rollback, refresh or log stream and returns what it did to each
resource.

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
| Heap at steady state | 150 MB | **82.2 MB** (862 B/object) |
| Heap peak during start-up | 150 MB | **109 MB** (paged lists, 120 MiB soft limit) |
| CPU at 200 pod updates/s | 0.2 vCPU | **0.09 vCPU** (including generating the updates) |
| Bytes sent at that rate | — | **309 B/s per 1k resources** |
| Initial report | — | 6.0 MB gzip, 3.9 s |

(Phase D, with orphans on as the agent runs by default; phase B measured
74 MB steady, 103 MB peak.)

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

**Phase B, on the lab** (agent rendering all 100 apps from the lab's GitOps
repo with Kustomize): every app *Synced* at the branch tip; `kubectl scale
deploy/cart-api --replicas=3` shows *Out of sync* on the page **643 ms**
later, and the Diff shows exactly `replicas: 3` against `replicas: 2`; a pod's
logs stream to the Logs tab through the agent. With write access
(`agent-sync.yaml`, `--allow-sync`): a Sync without Force is refused for
the Deployment — `conflict with "kubectl" with subresource "scale":
.spec.replicas` — while the ConfigMap, Service and Ingress come back
unchanged; with Force the Deployment is configured and the app is *Synced*.
A sync to the repo's first commit rolls cart-api back to image `v1` (the
Diff then shows `echo-service:v1` against `:6c5b7c18` at the tip), and a
sync to the tip brings it forward again.

**Phase C, on the lab.** With auto-sync on and self-heal off, `kubectl scale
deploy/cart-api --replicas=3` leaves cart-api *Out of sync* and the panel
says *Live differs from 8ace259 since it was synced; self-heal is off*.
Turning self-heal on heals it 3 s later. With self-heal on, `kubectl scale
--replicas=5` → *Out of sync* → a self-heal sync → *Synced* at 2 replicas in
**1.1 s** end to end; the replicas field is Forge's again (field manager
`forge`). Waves and hooks, on a demo app in its own namespace (busybox
pods): one sync ran the PreSync `db-migrate` Job to completion, wave -1
(ConfigMap), wave 0 (Deployment, Service), wave 1 (worker Deployment), each
healthy before the next, then the PostSync `smoke-test` Job, which was
deleted once it succeeded (HookSucceeded) — 49 s, mostly pod start-up.
Dropping the worker from Git showed it as *to prune*; auto-sync with prune
synced the new revision (re-running the migration) and deleted it. A
ConfigMap made by hand in that namespace appeared under *Orphaned*, with
its live manifest. The lab was restored afterwards.

**Phase D, on the lab.** The canary demo ran a second in-cluster agent
scoped to a `lab/rollout-demo` repo in the lab's Gitea, so the shared lab
GitOps repo stayed untouched. *storefront* runs 4 replicas. A commit moving
it from echo-service `v1` to `6c5b7c18`, with auto-sync on, rolled out as a
canary: 1 canary pod beside 4 (20%), 30 s, then 4 beside 4 (50%), 30 s, then
promotion. The analysis passed every 5 s (health, plus HTTP `/healthz` to
the canary pods: 5/5, then 20/20). The stable Deployment rolled out the new
image and the canary was removed, in about 60 s. A commit making the
release crash on start rolled out the same way: the canary pod went
`CrashLoopBackOff`, two analysis rounds failed in about 10 s, the rollout
aborted, and the 4 stable pods never changed. Reverting the commit in Git
left *storefront* Synced with no rollout, because the stable version had
never changed.

Before the digest change, two later commits elsewhere in that repo
re-triggered the aborted release twice. That is what the digest now
prevents.

A second kind cluster (`forge-lab-2`, its agent on the Mac) plus the
template `edge` (path `apps/edge/overlays/{target}`, auto-sync, self-heal,
create namespace) brought `edge` up on both clusters in about 2 s once the
agents had it: 2 replicas of `6c5b7c18` on one cluster, 1 of `v1` on the
other, from their overlays. The fleet view marks the `v1` cell as skew.

**Cloud Run:** an unchanged fleet costs one list call per region per
reconcile (720/day per region at the default period, shared by every
service in it) plus one call per service per change; the bucket is never
exceeded (`TestCallBudget`, `TestReconcileIsConditional`).

## Not yet

Blue-green and ingress/mesh traffic routing for canaries; canaries on
Cloud Run (its traffic split is native, not yet used). The Cloud Run
reconcile becomes event-driven (audit logs → Pub/Sub). Applications link to their
target's repo; linking each app to its own source repo is next. Deleting an
application (and its resources) from Forge, and per-app sync options set in
Git (Argo-style annotations), are not built.
