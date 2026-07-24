# Self-hosting Forge's own CI

Forge is a standalone CI engine, so it can build and test **itself**. This doc
covers the Forge-native path (recommended once the repo lives on a VCS Forge can
clone) and the GitHub Actions fallback used in the meantime.

The pipeline lives in [`.forge-ci.yml`](../.forge-ci.yml) at the repo root:

```
stages: [build, test, lint]
  build       →  go build ./...            (golang:1.25, caches the Go build + module cache)
  unit-tests  →  go test  ./...            (needs build; Postgres sidecar = DB + network)
  vet         →  go vet   ./...            (needs build)
  gofmt       →  gofmt -l . must be empty
  web-build   →  npm ci && npm run build   (node:22-alpine)
```

It uses the DSL end to end: `needs` (DAG), content-addressed `cache` keyed by
`go.sum` / `package-lock.json` and shared across pipelines, per-job `image`,
`services` (sidecars), `variables`, and `default.timeout`/`retry`.

The config is **compiler-validated** by the e2e suite
(`test/e2e: TestSelfCIConfigCompiles`), which registers it through
`PUT /api/v1/repo-configs` (the real compiler) and asserts the expected job set.

## Networking: why the sidecars

The docker executor deliberately isolates a **serviceless** job with
`--network none`. A `go build`/`go test`/`npm ci` that must reach the Go module
proxy or the npm registry therefore won't have a network. Two ways around it,
both first-class:

1. **Declare a sidecar** — any `services:` entry moves the job onto a per-job
   bridge network with outbound internet. `.forge-ci.yml` adds a throwaway
   `alpine:3` `net` sidecar to `build`/`vet`/`web-build`, and a
   `postgres:16-alpine` sidecar to `unit-tests` (which doubles as the test DB —
   Forge's own DB-backed tests connect to `$DATABASE_URL` and skip when absent).
2. **Pre-warm the runner's module cache** — a runner whose image ships a
   populated `GOMODCACHE`, combined with the `cache:` blocks, can drop the
   module-proxy sidecars entirely.

## Forge-native path (docker runner)

Prereqs: `make db` (Postgres), a running `forge-server`, and a docker runner
whose images can run Go/Node (the config pins `golang:1.25` / `node:22-alpine`).

```sh
make db
make server                 # control plane on :8080
make runner-docker          # a docker-executor runner (separate terminal)

# Register this repo + config and trigger a run (Forge clones the repo, so it
# must be reachable — a real remote in production, or a local file:// URL here):
scripts/self-host-ci.sh              # ref defaults to main
#   CLONE_URL=git@github.com:you/forge-ci.git scripts/self-host-ci.sh
```

Watch it in the dashboard (`make web` → http://localhost:5173) or poll
`GET /api/v1/pipelines?repo=forge/forge-ci`.

### Verified live on this host

A reduced Forge-native run was executed end to end on the development host with
the **docker executor**, cloning the repo over `file://` and running the real Go
toolchain in `golang:1.25`:

```
pipeline 1 created
 build: created → running → success   (go version; go build ./...)
 vet:   created → pending → running → success   (needs: build; go vet ./...)
FINAL: success
```

This proves the mechanics: clone → docker executor → real toolchain on the
cloned source → DAG (`vet` needs `build`) → shared Go module cache. The full
`.forge-ci.yml` adds `unit-tests` (Postgres sidecar), `gofmt`, and `web-build`.

## GitHub Actions fallback

Because this repo is not yet on a remote a Forge server can clone, working CI is
also provided as a GitHub Actions workflow:
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml). It mirrors
`make test` / `make test-e2e`: `go build`, `go vet`, `gofmt` check, unit tests,
the **e2e suite** (against Postgres + Redis services), and the web build.

Prefer the Forge-native path once the repo is hosted; the Actions workflow is the
stopgap so there is always green CI.
