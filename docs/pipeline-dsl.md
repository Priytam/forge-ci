# Pipeline DSL: rules, include, extends, parallel/matrix

This is the reference for Forge's GitLab-style authoring extensions. The core
DSL (`stages`, `default`, `auto_cancel`, per-job `stage`/`image`/`script`/
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

Compilation is ref- and source-aware: `Compile(yaml, ref, source, resolver)`.
`source` is `CI_PIPELINE_SOURCE` (`api` for `POST /api/v1/pipelines`, `webhook`
for a VCS webhook trigger, `push` for config validation on save).

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
| `changes: [globs]` | **Documented no-op** — see limitations. |
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
| `CI_COMMIT_BRANCH` | the pipeline ref (see limitation below) |
| `CI_PIPELINE_SOURCE` | `api` \| `webhook` \| `push` |

Plus the job's own `variables:` (and any variables set by an earlier resolved
rule). **Limitation:** Forge cannot distinguish a tag ref from a branch ref at
compile time (a webhook carries only the ref string), so `CI_COMMIT_BRANCH` is
set to the ref and `CI_COMMIT_TAG` is left undefined.

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

Forge does not host a repo file tree, and webhooks carry no file contents, so
there is no filesystem path to `include: local:` against. Instead, reusable YAML
fragments are **registered per repo** and referenced by name:

```bash
# Register a template
curl -X PUT /api/v1/repo-templates -d '{
  "repo": "org/app",
  "name": "go-common",
  "yaml": "stages: [lint, build]\njobs:\n  lint:\n    stage: lint\n    script: [golangci-lint run]\n"
}'

# List a repo's templates
curl /api/v1/repo-templates?repo=org/app
```

```yaml
include:
  - template: go-common          # merged UNDER the main config
stages: [build, test]
jobs:
  build:
    image: golang:1.23           # overrides any build.image from the template
```

### Merge semantics

Included fragments are applied **in listed order**, each overriding the previous;
then the **main config overrides all includes** on any conflict.

- **jobs** — deep-merged by name (same rules as `extends`; main wins per key).
- **stages** — unioned preserving order (includes first, then any new main
  stages).
- **default / auto_cancel** — main wins when it sets them.

Nested includes (a template that itself has `include:`) are supported up to a
depth bound that guards against include cycles.

**Limitation:** remote/URL includes are intentionally **not** supported — the
compiler does no network fetch. An unregistered template name, or an `include:`
form other than `{template: name}`, is a clear compile error.

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

## `allow_failure`

`allow_failure: true` (job-level, or from a matching rule) means the job's
failure does **not** block its dependents and is **not** counted as a pipeline
failure:

- **Dependents proceed** — a `needs` on an allowed-failure job is satisfied when
  it succeeds *or* fails.
- **The pipeline is not marked failed** — status derivation folds an allowed
  failure into `success` for the overall and per-stage status (the individual
  job still reports its true `failed` status).

---

## `changes:` / `exists:` — honest limitations

Both are **parsed and validated** but always treated as **satisfied (true)** —
they are documented no-ops. Forge compiles pipelines **without a repo checkout
or a file diff**: a webhook delivers only `repo`, `ref`, and `sha`, and Forge
does not clone at compile time. There is therefore no honest way to know which
files changed (`changes:`) or which files exist in the tree (`exists:`), so
Forge does **not** fake them. A rule that relies solely on `changes:`/`exists:`
will always match; combine them with an `if:` clause to get meaningful gating.

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
