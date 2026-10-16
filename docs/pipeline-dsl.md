# Pipeline DSL: rules, include, extends, parallel/matrix

This is the reference for Forge's GitLab-style authoring extensions. The core
DSL (`stages`, `default`, `auto_cancel`, `fail_fast`, per-job `stage`/`image`/`script`/
`needs`/`environment`/`variables`/`only`/`except`/`tags`/`artifacts`/`cache`/
`timeout`/`retry`) is documented in the [README](../README.md#pipeline-dsl).
Everything
here is **backward compatible**: a config that uses none of these features
compiles to exactly the same DAG as before.

The compiler runs each config through this pipeline:

```
include (templates) → extends (inheritance) → drop hidden jobs
   → per-job inclusion (rules, else only/except) → parallel/matrix expansion
   → needs wiring (with matrix fan-in) → validation
```

Compilation is ref- and source-aware: `Compile(yaml, ref, source, resolver, [extra])`.
`source` is `CI_PIPELINE_SOURCE` (`api` for `POST /api/v1/pipelines`, `webhook`
for a VCS push webhook, `merge_request` for a GitHub/Bitbucket pull-request
webhook, `schedule` for a cron-scheduled run — see
[docs/schedules.md](docs/schedules.md), `push` for config validation on save).
The optional `extra` map carries source-specific context — for a `merge_request`
pipeline the webhook handler passes the `CI_MERGE_REQUEST_*` variables here; they
are visible to `rules:` `if:` expressions **and** injected into every job's env
(see [vcs-integration.md](vcs-integration.md#prmr-triggered-pipelines)).

---

## 1. `rules:`

A per-job ordered list. The **first matching rule wins** and decides the job's
inclusion, its `when`, and optional `allow_failure`. If **no rule matches**, the
job is not added to the pipeline (GitLab semantics).

```yaml
stages: [test, deploy]
jobs:
  deploy-prod:
    stage: deploy
    script: [./deploy.sh]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main" && $CI_PIPELINE_SOURCE == "webhook"'
        when: manual                     # gated: runs only after a manual "play"
      - if: '$CI_COMMIT_BRANCH =~ /^release\//'
        when: on_success
        allow_failure: true
      - when: never                      # anything else: excluded
```

### Clause forms

| Clause | Meaning |
| --- | --- |
| `if: '<expr>'` | Boolean expression over pipeline variables (see grammar below). |
| `when:` | `on_success` (default), `manual`, `never`, or `always`. `never` excludes the job. |
| `allow_failure:` | `true`/`false`; overrides the job-level `allow_failure`. |
| `variables:` | Extra variables injected into the job when this rule matches. |
| `changes: [globs]` | Matches when the pipeline's commit changed a file matching any glob — see *`changes:`* below. |
| `exists: [globs]` | **Documented no-op** — see limitations. |

A rule with multiple clauses matches only when **all** its clauses are
satisfied (logical AND).

### Precedence vs `only`/`except`

If a job declares `rules:`, they **fully supersede** `only`/`except` for that
job. A job with no `rules:` keeps the existing `only`/`except` behavior
unchanged. (A job may also set a top-level `when: manual`/`never` without rules.)

### The `if:` expression language

A small, **safe**, hand-written evaluator — no arbitrary code execution, no
function calls, no arithmetic. Grammar:

```
expr    := or
or      := and ( '||' and )*
and     := cmp ( '&&' cmp )*
cmp     := unary ( ('==' | '!=' | '=~' | '!~') unary )?
unary   := '!' unary | primary
primary := '(' expr ')' | $VARIABLE | 'string' | /regex/ | null
```

- **Operands**: string literals (`'...'` or `"..."`), `$VARIABLE` references,
  `/regex/` literals, and `null`.
- **`==` / `!=`**: string equality. An unset variable equals `null`
  (`$X == null` is true when `X` is unset) and is never equal to a string
  (`$X == "y"` is false when `X` is unset).
- **`=~` / `!~`**: regex match / non-match. An unset variable never matches.
- **`&&` / `||` / `!`**: boolean composition; `&&` binds tighter than `||`;
  parentheses override precedence.
- **Bare `$X`**: truthy only when `X` is set to a non-empty string.
- A malformed expression is a **hard compile error**, never a silent `false`.

### Available variables

The base context exposes:

| Variable | Value |
| --- | --- |
| `CI_COMMIT_REF`, `CI_COMMIT_REF_NAME` | the pipeline ref |
| `CI_COMMIT_BRANCH` | the branch — **undefined for a tag push** |
| `CI_COMMIT_TAG` | the tag — **defined only for a tag push** |
| `CI_PIPELINE_SOURCE` | `api` \| `webhook` \| `schedule` \| `push` \| `merge_request` |

Plus the job's own `variables:` (and any variables set by an earlier resolved
rule), and on merge-request pipelines the `CI_MERGE_REQUEST_*` set. Every git
host's push webhook (GitHub, GitLab, Bitbucket, Gitea/Forgejo, CodeCommit) says
whether a tag or a branch moved, so `rules: - if: $CI_COMMIT_TAG` works the same
everywhere. A pipeline started from the API or a schedule is a branch pipeline.

### `when: manual` — the manual gate

A manual job is created in the **`blocked`** state with an internal `manual`
flag (distinct from the environment-approval block). It does not run until it is
explicitly played:

```
POST /api/v1/jobs/{id}/play
```

Play returns the job to `created` and clears the manual flag, so the normal
scheduler flow applies from there — including protected-environment **approval**
if the job targets a protected environment. (A manual job also composes with the
approval gate: play first, then approve.) Manual jobs are exempt from the
approval-timeout expiry, and a manual job whose upstream fatally fails is
canceled like any other dead-ended job.

---

## 2. `include:` — reusable templates

An include pulls YAML in from somewhere else and merges it **under** the main
config. Four sources, all through the same list:

```yaml
include:
  - template: go-common                 # a template registered for this repo, or a built-in
  - template: deploy/kubernetes@1.4.0   # the org template catalog, pinned to a version
  - project: platform/ci-templates      # a file from another connected repo (any git host)
    ref: v2.1.0                         # tag, branch or commit: required; pin a tag or sha
    file: templates/lint.yml
  - remote: https://ci.example.com/audit.yml
    integrity: sha256-q1w2e3...          # optional: refuse anything but this exact content
```

Each entry names exactly one of `template:`, `project:` or `remote:`.

### Repo templates

Named fragments registered for one repo:

```bash
curl -X PUT $FORGE/api/v1/repo-templates -d '{
  "repo": "org/app",
  "name": "go-common",
  "yaml": "stages: [lint, build]\njobs:\n  lint:\n    stage: lint\n    script: [golangci-lint run]\n"
}'
curl $FORGE/api/v1/repo-templates?repo=org/app
```

### The template catalog — `template: name@version`

Org-wide templates with immutable versions, browsable on the **Template
catalog** page (admins publish there, or with the API). `name@1.4.0` is that
exact version; a bare `name` that isn't a repo template or built-in resolves to
the catalog's newest version (`1.10.0` sorts after `1.9.2`; `1.0.0-rc1` before
`1.0.0`). Pin a version for anything that deploys.

```bash
curl -X POST $FORGE/api/v1/template-catalog -H "Authorization: Bearer $FORGE_TOKEN" -d "$(jq -n \
  --arg y "$(cat deploy.yml)" '{name:"deploy/kubernetes", version:"1.4.0", description:"Rolling deploy", yaml:$y}')"
curl $FORGE/api/v1/template-catalog                                  # every version
curl "$FORGE/api/v1/template-catalog?name=deploy/kubernetes&version=1.4.0"
curl -X DELETE "$FORGE/api/v1/template-catalog?name=deploy/kubernetes&version=1.4.0"   # admin
```

Republishing an existing version is refused (409): a fix is a new version.

### `project:` — a file from another repo

Reads `file` at `ref` from another **connected** repository, through that
repo's own connection, so it works on every git host Forge supports (API hosts
and plain git alike). `ref` is required — pin a tag or a commit sha so a change
to the template repo can't silently change your pipeline.

Because the read uses Forge's credential for the other repo, a pipeline may
only include from repos with the **same owner/namespace** (`acme/api` →
`acme/ci-templates`). To share a template repo across owners, list it in
`FORGE_INCLUDE_SHARED_PROJECTS` on the server (comma-separated; `platform/*`
for a whole namespace, `*` for every connected repo).

### `remote:` — a URL

Fetched over **https only**, at compile time, from the server:

- Internal addresses (loopback, private ranges, link-local and cloud metadata,
  carrier-grade NAT) are refused, checked at connect time so DNS tricks can't
  get round it. `FORGE_INCLUDE_REMOTE_ALLOW_PRIVATE=true` lifts this for an
  internal template server.
- `FORGE_INCLUDE_REMOTE_HOSTS=templates.example.com,*.example.org` restricts
  remote includes to those hosts.
- `integrity:` (`sha256-<base64>` as in browser SRI, or `sha256:<hex>`) pins the
  exact bytes; `FORGE_INCLUDE_REMOTE_REQUIRE_INTEGRITY=true` makes it mandatory.
- Redirects must stay on https; bodies over 1 MiB are refused; credentials in
  the URL are refused.

### Template inputs — `spec: inputs:`

A template (from any source) can declare inputs in a header document, then use
them as `$[[ inputs.name ]]`:

```yaml
spec:
  inputs:
    environment: {default: staging, options: [staging, production]}
    replicas:    {default: 2}
    cluster:     {description: "Where to deploy"}    # no default = required
---
jobs:
  deploy:
    stage: deploy
    environment: $[[ inputs.environment ]]
    script:
      - ./deploy.sh --cluster $[[ inputs.cluster ]] --replicas $[[ inputs.replicas ]]
```

```yaml
include:
  - template: deploy/kubernetes@1.4.0
    inputs: {cluster: eu-west, environment: production}
```

A missing required input, an unknown input, or a value outside `options` is a
compile error naming the input. A template without a `spec:` header takes no
inputs. Inputs are substituted into the template text before it is parsed, so
they can appear anywhere — job names, images, scripts, rules.

### Merge semantics

Included fragments are applied **in listed order**, each overriding the previous;
then the **main config overrides all includes** on any conflict.

- **jobs** — deep-merged by name (same rules as `extends`; main wins per key).
- **stages** — unioned preserving order (includes first, then any new main
  stages).
- **default / auto_cancel / fail_fast** — main wins when it sets them.

Nested includes (a template that itself has `include:`) are supported up to a
depth bound that guards against include cycles. Compile errors name the include
that failed (`include: project platform/ci@v2:lint.yml not found`).

### Built-in security templates

Forge ships a set of **built-in templates** so you get GitLab-style one-line
security scanning with **zero setup** — you do not have to register anything in
the repo first. They resolve through the same `include:` mechanism and merge
with the same semantics (stages unioned, jobs deep-merged, main config wins).

| `template:` name | Scanner (image) | Job emitted | Runs |
| --- | --- | --- | --- |
| `security/sast` | semgrep (`returntocorp/semgrep`) | `sast` | `semgrep --config auto --error .` |
| `security/dependency` | trivy (`aquasec/trivy`) | `dependency-scan` | `trivy fs --exit-code 1 --no-progress .` |
| `security/container` | trivy (`aquasec/trivy`) | `container-scan` | `trivy image --exit-code 1 --no-progress "$SCAN_IMAGE"` |
| `security/secrets` | gitleaks (`zricethezav/gitleaks`) | `secret-detection` | `gitleaks detect --source . --verbose --redact` |

```yaml
include:
  - template: security/sast
  - template: security/secrets
stages: [build]
jobs:
  build:
    stage: build
    image: golang:1.23
    script: [go build ./...]
```

See [`examples/security-pipeline.yml`](../examples/security-pipeline.yml) for a
complete runnable config.

**Defaults**

- Every scan job lands in the **`test`** stage and is **`allow_failure: true`**
  by default — a finding is surfaced in the pipeline (the job reports `failed`)
  but does **not** block dependents or mark the pipeline failed.
- A stage that exists **only** in an include is unioned **ahead** of stages that
  appear only in your main config, so an unpositioned `test` stage (scans) sorts
  **before** main-only stages such as `build`/`deploy` — scans run
  "security-first". To place scans anywhere else, **declare `test` in your main
  `stages:` list**: when the main config names a stage, the main config's
  position for it wins.
- Every scanner **needs network egress** (semgrep fetches its rule set from
  `semgrep.dev`; trivy downloads its vulnerability database). Jobs get egress by
  default — see [`network:`](#network--container-network) — so a scan job must
  not be given `network: none`.
- `security/container` scans the image named by the **`SCAN_IMAGE`** variable
  (default `alpine:3.19`). Point it at the image your pipeline builds by
  overriding that variable (see below).

**Precedence & overriding**

Resolution consults the **per-repo template store first** and only falls back to
a built-in when the repo has **no** template by that name. So:

- A repo can **shadow** any built-in by registering a `repo_templates` entry
  under the same name (e.g. `security/sast`) — its YAML then wins wholesale.
- Built-ins are available even for repos with **zero** registered templates, and
  even when the compiler is invoked with **no template store** at all.
- You can tune a built-in inline from your main config via the normal
  `include:` deep-merge (main overrides the included job per key). Two common
  overrides:

  ```yaml
  include:
    - template: security/dependency
    - template: security/container
  stages: [test]
  jobs:
    dependency-scan:
      allow_failure: false          # make the dependency scan BLOCKING
    container-scan:
      variables:
        SCAN_IMAGE: myorg/app:latest # scan the image this pipeline ships
  ```

The full list of built-in names is also available programmatically via the
compiler's `ListBuiltinTemplates()` Go function.

---

## 3. `extends:` — job inheritance

A job may inherit from one or more base jobs. Base jobs are conventionally
**hidden** (leading `.`) so they are never emitted as real jobs.

```yaml
stages: [test]
jobs:
  .go-base:                      # hidden template — not emitted
    image: golang:1.22
    variables: {CGO_ENABLED: "0"}
    script: [go build ./...]
  build:
    stage: test
    extends: .go-base            # inherits image + variables + script
  unit:
    stage: test
    extends: [.go-base, .test-base]   # multiple parents, left-to-right
```

- **Single or multiple** parents. Multiple parents are merged left-to-right, so
  a **later parent overrides an earlier** one; the **child overrides all**.
- **Chains** are supported (`.grand` ← `.parent` ← `child`).
- **Cycles** and **unknown targets** are compile errors.

### Deep-merge rules (shared by `extends` and `include`)

| Field kind | Rule |
| --- | --- |
| scalars (`stage`, `image`, `environment`, `timeout`, `when`) | child non-empty overrides |
| pointers (`retry`, `allow_failure`, `parallel`) | child value overrides when set |
| lists (`script`, `needs`, `only`, `except`, `tags`, `rules`, artifact paths) | child replaces when set (arrays are not element-merged) |
| `cache` (block) | child replaces the whole block when it declares one (signaled by `cache.paths`) |
| `variables` (map) | union-merged, child key wins |

---

## 4. `parallel` / `matrix`

### `parallel: N`

Expands a job into N instances named `job 1/N` … `job N/N`, each with
`CI_NODE_INDEX` and `CI_NODE_TOTAL` injected.

```yaml
jobs:
  spec:
    stage: test
    parallel: 3
    script: [run-tests]
# -> "spec 1/3", "spec 2/3", "spec 3/3"
```

### `parallel: {matrix: [...]}`

`matrix` is a **list of hashes**; within each hash the variable value-lists are
cartesian-multiplied, and the per-hash products are concatenated. Each instance
gets its combination injected as variables and a stable, unique name.

```yaml
jobs:
  build:
    stage: build
    script: [make]
    parallel:
      matrix:
        - OS: [linux, darwin]
          ARCH: [amd64, arm64]
# -> 4 jobs:
#    "build: [amd64, darwin]"  (OS=darwin ARCH=amd64)
#    "build: [amd64, linux]"   (OS=linux  ARCH=amd64)
#    "build: [arm64, darwin]"  (OS=darwin ARCH=arm64)
#    "build: [arm64, linux]"   (OS=linux  ARCH=arm64)
```

The name suffix `: [v1, v2]` lists the combination's values in **key-sorted**
order for determinism. Names are unique per pipeline (the store enforces this);
a collision between two matrix hashes is a compile error. Expansion is bounded
(`parallel: N` and matrix products are capped) to prevent runaway pipelines.

### `needs` fan-in

When a job `needs` a job that was expanded (parallel or matrix), the dependency
**fans in to every instance**:

```yaml
jobs:
  build:
    stage: build
    parallel: 2
    script: [make]
  deploy:
    stage: deploy
    needs: [build]          # -> needs BOTH "build 1/2" and "build 2/2"
    script: [./deploy.sh]
```

Implicit needs (a job with no `needs`, depending on the nearest earlier
non-empty stage) fan in the same way, since they are computed over the expanded
instances.

---

## 5. `cache:` — GitLab-style caching

A job can restore a cache before its script and save it after, so warmed
dependency directories persist **across pipelines**. This is the key difference
from artifacts: artifacts are per-job and per-pipeline (passed to downstream
jobs via `needs`), whereas a cache is keyed by **repo + cache-key** and shared
by every pipeline of that repo that uses the same key.

```yaml
jobs:
  build:
    stage: build
    script: [go build ./...]
    cache:
      key:
        files: [go.sum, go.mod]   # content-addressed; hash of these files
        prefix: v1                # optional prefix on the hashed key
      paths: [vendor/, .cache/go-build]
      policy: pull-push           # pull-push (default) | pull | push
```

### `key`

- **Literal** — `key: my-key` uses the string verbatim.
- **Content-addressed** — `key: {files: [<lockfiles>], prefix: <optional>}`. The
  **runner** hashes the listed files' contents (they only exist after checkout,
  so hashing happens runner-side, not at compile time) and forms the key as
  `<prefix>-<hash>`. A **changed lockfile produces a different key** (a clean
  miss); an **unchanged one hits**. Missing key-files are excluded from the hash
  with a log note.
- **Fallback chain** — a `files:` restore tries the exact hashed key first, then
  falls back to the bare `prefix` (or `default` when no prefix), so a first-ever
  build for a new lockfile can still warm from the last prefix cache. A save
  always writes the exact (hashed) key.
- An empty/omitted key defaults to `default`.

### `paths`

Workspace paths (files or directories) tar'd into the cache. **Required** when a
cache is declared — a `cache:` with a key/policy but no `paths` is a compile
error. Unsafe paths (absolute, `..`, or missing at save time) are skipped with a
log line.

### `policy`

- `pull-push` (default) — restore before the script **and** save after success.
- `pull` — restore only (e.g. a test job that consumes but never updates a cache).
- `push` — save only (e.g. a dedicated warm-the-cache job).

### Scoping, storage & safety

- **Blob storage** — caches live in the same blob store as artifacts, under
  `cache/{repo}/{sha256(repo,key)}.tar.gz`. A `cache_entries` table
  (`repo, cache_key, blob_key, size_bytes, updated_at`, unique on `repo+key`)
  tracks them for listing and retention. A save overwrites the previous cache
  for that key.
- **Server-scoped repo** — the runner sends only the resolved key + job id; the
  server derives the repo from the job, so a runner can never write outside its
  repo's cache namespace.
- **Never fails the job** — a cache miss, a restore failure, or a save failure
  (including exceeding `MAX_CACHE_BYTES`, which returns `413`) is **logged and
  swallowed**. The job runs and reports its real status regardless.
- **Caps & retention** — `MAX_CACHE_BYTES` (default 500 MiB, `0` disables) caps
  each save; over-cap saves are rejected and the partial blob cleaned up. The
  retention sweep deletes cache blobs + rows not updated within `RETENTION_DAYS`
  (age-based and independent of pipelines, since a cache outlives them).

### Limitations

- Keys hash **file contents**, not globs — list concrete lockfiles.
- There is no per-path cache; all `paths` share one archive under one key.
- Caches are best-effort and may be evicted by retention at any time; treat a
  hit as an optimization, never a correctness dependency.
- No cross-repo cache sharing (the repo dimension is enforced server-side).

---

## 6. `artifacts:` — archives, expiry & test reports

Artifacts are the per-job, per-pipeline outputs a job archives after it
succeeds. Downstream jobs that `needs:` it get them restored into the workspace
before their script runs. (Contrast with `cache:`, which is keyed by repo+key
and shared *across* pipelines — see §5.)

```yaml
jobs:
  build:
    stage: build
    script: [make]
    artifacts:
      paths: [dist/, bin/]     # workspace paths tar'd + uploaded after success
      expire_in: 7d            # optional per-artifact TTL
  test:
    stage: test
    script: [go test -v ./... 2>&1 | go-junit-report > report.xml]
    artifacts:
      reports:
        junit: [report.xml, "**/junit-*.xml"]   # one glob or a list
```

### `paths`

Workspace paths (files or directories) tar'd and uploaded when the job
succeeds. Unsafe paths (absolute, `..`) and paths missing at upload time are
skipped with a log line. A job with no `paths` uploads no artifact archive.

### `expire_in`

A per-artifact time-to-live. Accepted units:

| Suffix | Meaning | Example |
|--------|---------|---------|
| `s` / `m` / `h` | seconds / minutes / hours (Go duration) | `90s`, `30m`, `24h` |
| compound Go form | any `time.ParseDuration` string | `1h30m` |
| `d` | days | `7d` |
| `w` | weeks | `2w` |

The value is parsed at **compile time** into a whole number of seconds and
stored on the job. At **upload time** the server stamps
`artifacts.expires_at = now() + expire_in` (computed server-side, not trusted
from the runner, so the TTL is authoritative and clock-consistent with GC).

- **GC is independent of `RETENTION_DAYS`.** A background sweep (cadence
  `GC_INTERVAL`, default `1h`) deletes the artifact **blob and row** once
  `expires_at` passes — even when the retention window is much larger, or
  retention is disabled (`RETENTION_DAYS=0`). So a short `expire_in` expires
  promptly regardless of retention.
- **No `expire_in` = unchanged behavior.** The artifact has a NULL `expires_at`
  and lives until the `RETENTION_DAYS` sweep collects its pipeline (the
  historical backstop).

### `reports.junit`

One or more workspace globs of JUnit XML. After the job succeeds the **runner**
resolves the globs, concatenates the matched file(s), and uploads them; the
**server** parses them (`encoding/xml`) into a per-job summary:

- Handles a `<testsuites>` collection, a single `<testsuite>`, and nested
  `<testsuite>` elements.
- Each `<testcase>` is a pass unless it carries a `<failure>`, `<error>` (both
  counted as failed, with the failing test name + message captured) or
  `<skipped>` child.
- Multiple concatenated documents (one per matched file, each with its own
  `<?xml?>` prolog) are summed in a single pass.

Fetch the summary at **`GET /api/v1/jobs/{id}/report`**:

```json
{ "job_id": 42, "total": 3, "passed": 2, "failed": 1, "skipped": 0,
  "duration_seconds": 1.5,
  "failures": [ { "name": "test_payment", "classname": "billing",
                  "type": "failure", "message": "expected 200 got 500" } ] }
```

`404` when the job has no report. **Malformed or empty XML is not a job
failure**: the server records no report and the job still reports `success`.

`reports.junit` is independent of `paths` — a job can publish a test report
without archiving any other artifact.

### `reports.dotenv` and `environment: url:`

```yaml
build:
  artifacts:
    reports:
      dotenv: build.env        # KEY=VALUE file the job writes
deploy:
  needs: [build]
  environment: {name: staging, url: "https://staging.$APP_DOMAIN"}
```

Dotenv variables are passed to the jobs that need the reporting job;
`DEPLOY_URL`, `DEPLOY_VERSION` and `DEPLOY_DIGEST` fill in the deployment
record. Details and limits: [environments.md](environments.md#url-version-and-digest).

### Coverage — `coverage:` and `reports.coverage_report`

```yaml
unit:
  script: [go test -coverprofile=c.out ./... && go tool cover -func=c.out]
  coverage: '/total:\s+\(statements\)\s+(\d+\.\d+)%/'    # last match in the log wins
python:
  script: [pytest --cov --cov-report=xml]
  artifacts:
    reports:
      coverage_report: {coverage_format: cobertura, path: coverage.xml}
```

`coverage:` is a `/regex/` applied to the job's (masked) log when it finishes;
the first capture group (or the whole match) holds the percentage. A
`coverage_report` in Cobertura XML — which coverage.py, gocover-cobertura,
JaCoCo and Istanbul converters write — sets it from the report's overall
`line-rate` instead. The pipeline shows the mean of its jobs; the repo's
**Tests** page charts it over recent pipelines
(`GET /api/v1/coverage?repo=&ref=&limit=`).

### Merge (`extends` / `include`)

Each artifacts sub-field merges independently (child overrides when it sets it):
`paths`, `expire_in`, and `reports.junit` are each inherited from a base job
unless the child redeclares that field — so a base template can set
`expire_in`/`reports` that concrete jobs reuse.

---

## 7. `services:` — sidecar containers

A job can declare **service containers** — databases, caches, message brokers —
that start alongside it, are reachable over the network, and are torn down when
the job finishes. This is the GitLab `services:` model.

```yaml
jobs:
  integration:
    stage: test
    image: postgres:16-alpine
    services:
      - image: postgres:16-alpine   # long form ('name:' is also accepted)
        alias: db                    # network hostname (optional)
        env: {POSTGRES_PASSWORD: pw}
        cmd: [postgres, -c, max_connections=50]  # optional command override
      - redis:7                      # shorthand: scalar = image
    script:
      - until pg_isready -h db -U postgres; do sleep 1; done
      - PGPASSWORD=pw psql -h db -U postgres -c 'select 1'
```

### Fields

- **`image`** *(required)* — the service container image. `name:` is accepted as
  a synonym for `image:` (GitLab long-form compatibility).
- **`alias`** — the hostname the job reaches the service by. Defaults to the
  image's short name with the tag stripped and sanitized to a DNS label
  (`postgres:16-alpine` → `postgres`, `docker.io/library/redis:7` → `redis`).
  Must be a lowercase RFC1123 label (`[a-z0-9]` and `-`, 1–63 chars) and unique
  within the job.
- **`env`** — environment variables for the service container (e.g.
  `POSTGRES_PASSWORD`).
- **`cmd`** — optional command/args override (docker CMD / k8s container args).

### Executor support

| Executor | Mechanism | How the script reaches a service |
|----------|-----------|----------------------------------|
| **docker** | A dedicated per-job bridge network `forge-net-<id>`. Each service runs as `forge-svc-<id>-<i>` attached with `--network-alias <alias>`. The **job container joins the same network** (instead of its own `network:`). | By alias via docker DNS: `psql -h db`. `$FORGE_SERVICE_ALIASES` lists the aliases. |
| **kubernetes** | Extra containers in the job's pod (`svc-<i>`), sharing the pod network namespace. `spec.hostAliases` maps every alias to `127.0.0.1`. | By alias (→ `127.0.0.1`) **or** `localhost:<port>` — the same `-h <alias>` script works on both executors. |
| **shell** | **Unsupported** — no container runtime. | The job **fails immediately** with `shell executor cannot run service containers…`; route it to a docker/kubernetes runner (matching tags). |

### Readiness, cleanup & caps

- **Readiness** is best-effort: the executor waits for each service container to
  be *running* (docker: bounded to 60s; k8s: `kubectl wait --for=condition=Ready`
  covers all pod containers, bounded to 180s), and for *healthy* when the image
  ships a `HEALTHCHECK`. Container-running does **not** guarantee the service
  inside is accepting connections, so **scripts should still poll** the protocol
  (`until pg_isready`, `redis-cli ping`, …).
- **Cleanup is guaranteed** on every exit path — success, failure, timeout, and
  cancel. Docker tears down the job container, every service container, and the
  network; Kubernetes deletes the pod (which removes all its containers). No
  networks, containers, or pods are leaked.
- **Caps** — at most **5 services per job**. Each service requires an image and a
  valid, unique alias; violations are compile-time errors.
- **Isolation** — a job that declares **no** services runs a single container on
  the network its `network:` field selects (default `bridge` — see
  [`network:`](#network--container-network)); the k8s executor keeps its
  single-container `kubectl run` pod. `network:` and `services:` are mutually
  exclusive: a job with services must be on its own network to resolve the
  aliases, so declaring both is a compile-time error.

### Merge & matrix

`services:` participates in `extends`/`include` deep-merge like other arrays: a
child's list **replaces** the parent's wholesale (arrays are not element-merged).
Services survive `parallel`/`matrix` expansion — every generated instance carries
the same resolved services.

---

## `network:` — container network

Selects the container network for a job on the **docker** executor.

```yaml
jobs:
  scan:
    image: aquasec/trivy
    script: [trivy fs --exit-code 1 --no-progress .]
    # network: bridge      # the default — the job has DNS and egress

  airgapped:
    image: alpine:3
    network: none          # no network namespace at all
    script: [./offline-build.sh]
```

| Value | Effect |
|-------|--------|
| *(unset)* | **`bridge`** — the docker default network: DNS and outbound egress |
| `bridge` | the same, stated explicitly |
| `none` | no network namespace — no DNS, no egress |
| `host` | the host's network namespace |
| *any other name* | an existing docker network the operator manages (e.g. one with an egress proxy) |

**The default is `bridge`, and deliberately so.** A job with no network cannot
install dependencies, fetch a rule set, or download a vulnerability database, so
an isolated default silently broke every network-dependent tool — including the
[built-in security templates](#built-in-security-templates), where semgrep pulls
rules from `semgrep.dev` and trivy downloads its vuln DB. Use `network: none` for
jobs that genuinely should have no egress; it is a real isolation boundary, not a
hint.

**Notes**

- **docker only.** The shell executor runs in the host's network namespace, and
  kubernetes pods always have pod networking; `network:` is ignored there.
- **Mutually exclusive with `services:`** — a job with services is placed on its
  own per-job network so it can resolve the service aliases. Declaring both is a
  compile-time error rather than a silently-ignored key.
- Participates in `extends`/`include` merge like the other scalars: a child's
  non-empty value overrides the parent's.
- The value is charset-checked at compile time and may not begin with `-`, so it
  can never be mistaken for a flag in the container runtime's argv.

---

## `allow_failure`

`allow_failure: true` (job-level, or from a matching rule) means the job's
failure does **not** block its dependents and is **not** counted as a pipeline
failure:

- **Dependents proceed** — a `needs` on an allowed-failure job is satisfied when
  it succeeds *or* fails.
- **The pipeline is not marked failed** — status derivation folds an allowed
  failure into `success` for the overall and per-stage status (the individual
  job still reports its true `failed` status).
- **Does not trip `fail_fast`** — because an allowed failure is not a pipeline
  failure, it never triggers `fail_fast` cancellation of siblings (see below).

---

## `fail_fast` — stop the run on the first genuine failure

Top-level `fail_fast: true` (default **false**; opt-in, and unlike some CI
systems GitLab is *not* fail-fast by default) makes the scheduler cancel a
pipeline's other non-terminal jobs the moment any job reaches a **final**
`failed` state that is **not** `allow_failure` — the whole run, not just
downstream dependents:

- **created / pending / blocked** siblings → `canceled` immediately.
- **running** siblings → asked to stop via the heartbeat cancel path (the same
  mechanism as an explicit job/pipeline cancel): `cancel_requested` is set, the
  runner kills the process and reports `canceled`. This includes running
  `allow_failure` jobs — the run is already doomed, so they are cancelled too.
- The already-failed job and any terminal job are left untouched.

Semantics and scope:

- **Only a final failure counts.** A failing attempt with `retry` budget left is
  requeued (`pending`) and never written as `failed`, so retries do **not** trip
  fail-fast until they are exhausted.
- **`allow_failure` failures never trip it** (see above).
- **Pipeline-level only.** There is no per-stage `fail_fast`.
- **Idempotent & replica-safe.** The cancellation is a scheduler transition
  (`fail_fast_cancel`) applied every tick as idempotent SQL; a job already
  canceled/terminal or already flagged is skipped.
- Leaving it unset (or `false`) is fully backward compatible: independent
  siblings run to completion and only jobs whose needs died are canceled.

---

## `changes:` — run jobs only when their files changed

```yaml
web-build:
  stage: build
  rules:
    - changes: ["web/**/*", "package-lock.json"]
  script: [npm ci, npm run build]
api-deploy:
  stage: deploy
  rules:
    - if: '$CI_COMMIT_BRANCH == "main"'
      changes: ["api/**", "go.mod", "go.sum"]
  script: [./deploy-api.sh]
```

A `changes:` clause matches when **any file the pipeline's commit changed**
matches any of its globs (`**` = any directories, `*` = within one directory,
`?` = one character, `{a,b}` = alternatives, a trailing `/` = everything
under that directory). It combines with `if:` by AND, like every clause.

What "changed" is compared against:

| Pipeline | Compared with |
|---|---|
| push to a branch with an earlier **successful** pipeline | that pipeline's commit (so a fix after a red run still rebuilds what the red run touched) |
| push to a branch with no successful pipeline yet | the repo's default branch, from the merge base |
| merge request / pull request | the target commit of the merge request |

The file list comes from the provider (GitHub compare, Bitbucket diffstat,
CodeCommit GetDifferences) for repos connected under Repos → Add repository.

**When Forge can't tell, nothing is skipped:** the change set is *unknown* and
every `changes:` clause matches for API / manual runs, schedules, the first
run on the default branch, repos that aren't connected, a provider error, or a
diff too large for the provider to list (GitHub: 300 files). That's the safe
direction — a job runs rather than silently not running.

Preview it: `POST /api/v1/lint {"config": …, "ref": "main", "changed_files":
["web/src/App.tsx"]}` shows which jobs that change would run.

`exists:` is still a documented no-op (always true): Forge compiles without a
checkout of the tree.

---

## Validation & errors

The compiler rejects, with clear messages:

- unknown `stage`, missing `script` (after extends), duplicate stages;
- `needs` on an unknown/hidden/ref-excluded job, or one not in an earlier stage;
- `extends` cycles and unknown `extends` targets;
- malformed `rules: if:` expressions and invalid `when:` values;
- `parallel` out of range, non-list/non-scalar matrix values, empty matrix, and
  colliding matrix combinations;
- `include:` of an unregistered template, a non-`{template}` include form, and
  over-deep include nesting.
- `cache:` with `paths` but no `key`, a `key`/`policy` with no `paths`, an empty
  `key.files` list, or an invalid `policy` (not `pull`/`push`/`pull-push`).

## Predefined variables in jobs

Every job's environment carries these, set by the server when a runner picks
the job up (all executors). Repo variables and the job's own `variables:`
override them, as in GitLab.

| Variable | Value |
|---|---|
| `CI`, `FORGE_CI` | `true` |
| `CI_COMMIT_SHA` / `CI_COMMIT_SHORT_SHA` | full / first 8 characters of the commit |
| `CI_COMMIT_REF_NAME` | branch or tag name (`refs/heads/` / `refs/tags/` stripped) |
| `CI_COMMIT_BRANCH` | branch name — **unset for tag pipelines** |
| `CI_COMMIT_TAG` | tag name — **set only for tag pushes** (every git host) |
| `CI_COMMIT_REF_SLUG` | ref name lowercased, non-alphanumerics → `-`, max 63 chars (for image tags, hostnames) |
| `CI_COMMIT_MESSAGE` / `CI_COMMIT_TITLE` | full message / first line |
| `CI_COMMIT_AUTHOR` | who wrote the commit, when the provider says |
| `CI_TRIGGERED_BY` (alias `GITLAB_USER_LOGIN`) | who started the run |
| `CI_PIPELINE_ID`, `CI_PIPELINE_SOURCE` | pipeline id; `api` / `push` / `webhook` / `merge_request` / `schedule` |
| `CI_JOB_ID`, `CI_JOB_NAME` | the job |
| `CI_PROJECT_PATH`, `CI_PROJECT_NAMESPACE`, `CI_PROJECT_NAME` | `acme/cos`, `acme`, `cos` |
| `CI_ENVIRONMENT_NAME` | the job's environment (registered name), when it has one |
| `CI_DEFAULT_BRANCH` | the repo's default branch, when it is connected |
| `CI_REPOSITORY_URL` | the repo's clone URL as registered — **never carries credentials** (the runner's checkout does its own auth) |
| `CI_SERVER_URL`, `CI_API_V1_URL` | this Forge server (`EXTERNAL_URL`, else `FRONTEND_URL`) and its API |
| `CI_PIPELINE_URL`, `CI_JOB_URL` | links to the pipeline / job page (`FRONTEND_URL`, else `EXTERNAL_URL`) |
| `CI_MERGE_REQUEST_ID`, `CI_MERGE_REQUEST_IID` | the merge/pull request number (hosts without a separate global id use the number for both) |
| `CI_MERGE_REQUEST_SOURCE_BRANCH`, `CI_MERGE_REQUEST_TARGET_BRANCH`, `CI_MERGE_REQUEST_TITLE` | merge-request pipelines |
| `CI_MERGE_REQUEST_PROJECT_PATH`, `CI_MERGE_REQUEST_SOURCE_BRANCH_SHA` | the repo and head commit of the merge request |
| `CI_MERGE_REQUEST_DIFF_BASE_SHA` | the target commit the diff is against, when the host sends it |
| `CI_DEBUG_TRACE` | `true` on a **debug rerun** (below) |
| `FORGE_OIDC_TOKEN` (`CI_JOB_JWT`) | per-job OIDC token for keyless cloud auth (docs/oidc.md) |

The URL variables are unset when neither `EXTERNAL_URL` nor `FRONTEND_URL` is
configured.

## Debug reruns — `CI_DEBUG_TRACE`

**Retry with debug trace** on a failed or canceled job (or
`POST /api/v1/jobs/{id}/retry {"debug": true}`) reruns that job with
`CI_DEBUG_TRACE=true`, and every executor (shell, docker, kubernetes) then runs
the script with `sh -x`, so the log shows each command as it is executed, with
variables expanded. Only the job you asked for gets the trace; the jobs after it
rerun normally. You can also set `CI_DEBUG_TRACE: "true"` in a job's
`variables:`.

Masked variables stay masked in traced output. The trace prints a value as
the shell re-quotes it — bash writes `it's` as `it'\''s`, busybox ash (Alpine)
as `it'"'"'s`, dash as is — and Forge masks every one of those forms, on the
shell, docker and kubernetes executors alike (masking happens on the server as
log chunks arrive, whichever executor produced them). What masking cannot
catch is a value *derived* from a secret — base64-encoded, URL-encoded, sliced
— which the trace will show once the script computes it. Use debug reruns
deliberately on jobs that transform credentials.

## Validating a config without running it

`POST /api/v1/lint {"config": "<yaml>", "ref": "main", "repo": "acme/cos"}`
compiles the YAML exactly as a run on `ref` would — includes from the repo's
templates, `rules:` evaluated for that branch, and this server's environment
branch rules — and returns `{valid, error, stages, jobs}`. Each job lists its
stage, environment, needs and, if the environment rules would refuse it on
that branch, why (`refused`). Nothing is started. The **Validate** button on
Run pipeline and on Settings → Pipeline config calls it.

```sh
curl -s -H "Authorization: Bearer $FORGE_TOKEN" -X POST $FORGE/api/v1/lint \
  -d "$(jq -n --rawfile c .forge-ci.yml '{config:$c, ref:"main"}')"
```

## `resource_group:` — one deploy at a time

```yaml
deploy:
  stage: deploy
  environment: production
  resource_group: deploy-$CI_ENVIRONMENT_NAME   # → "deploy-production"
  script: [./deploy.sh]
```

At most **one job per (repo, `resource_group`) runs at a time**. Other jobs of
the same group wait in the queue (status *pending*) and start, oldest first, as
soon as the running one finishes. Different repos and different groups don't
wait on each other. `$CI_ENVIRONMENT_NAME` (or `${CI_ENVIRONMENT_NAME}`)
resolves to the job's registered environment name, so one line gives "one
deploy per environment". Enforced when a runner picks the job up, under a
database lock, so it holds across server replicas and for jobs released by an
approval. Allowed characters: letters, digits, `_ . - /`.

## Retrying one job

**Retry job** on a failed or canceled job (UI, or `POST /api/v1/jobs/{id}/retry`)
re-runs it **inside the same pipeline**, together with every job after it (via
`needs`, transitively) that did not succeed. Jobs that succeeded are not re-run.

- Each re-run job is a **new job** (its own log and attempt count) with
  `retry_of` pointing at the attempt it replaces; the old attempt stays
  visible from the job page ("retry of job N") but no longer counts towards
  the pipeline's status.
- The scheduler runs the new jobs normally: approvals, deploy freezes,
  resource groups and environment branch rules all apply again — a job the
  branch rules refuse can't be brought back by a retry (`403`).
- Same permission as retrying a whole pipeline: anyone with a role on the
  repo. The pipeline-level **Retry** still starts a fresh copy of the whole
  pipeline.

## `runner_affinity:` — prefer the runner with warm caches

`runner_affinity: true` on a job makes its next run prefer the runner that
last ran it successfully, for up to `RUNNER_AFFINITY_WAIT` (default 1m) before
any runner may take it. See docs/runners.md → *Cache affinity*.
