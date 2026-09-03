# Contributing to Forge CI

Forge CI is licensed under the [Apache License 2.0](LICENSE). **Any
organisation — commercial, enterprise, government, academic or individual — may
clone, run, modify, self-host, redistribute and build on this project, for
internal or commercial use, at no cost and without asking permission.** The
Apache-2.0 grant includes an express patent licence, and there is **no
Contributor License Agreement to sign**.

Contributions are welcome from anyone, including on company time under a
company email address.

## How contributions are licensed

Per Apache-2.0 section 5, any contribution you intentionally submit for
inclusion is licensed under Apache-2.0, without additional terms. We do not ask
for copyright assignment, and you keep the copyright in your contribution.

Instead of a CLA, we use the **Developer Certificate of Origin** (the same
mechanism as the Linux kernel and CNCF projects). Sign off each commit:

```sh
git commit -s -m "scheduler: fix stale-job sweep off-by-one"
```

That appends a `Signed-off-by:` line, which certifies you wrote the patch or
otherwise have the right to submit it under Apache-2.0. Read the full text at
<https://developercertificate.org/>.

If you are contributing on behalf of an employer, make sure you have their
authorisation — a `Signed-off-by` with a company email is taken as asserting
exactly that.

## Development setup

**Prerequisites:** Go 1.25+ (see `go.mod`), Node 22+, and Docker for Postgres.

```sh
make setup      # postgres container + go deps + npm install
make server     # control plane on :8080
make runner     # shell-executor runner (separate terminal)
make web        # vite dev server on :5173 (separate terminal)
make demo       # trigger examples/demo-pipeline.yml
```

Then open <http://localhost:5173>. The demo pipeline runs build → test and
then blocks on `deploy-prod` until you approve it in the UI.

Whole stack in containers instead: `make up` (dashboard on :3000).

> **Node path note.** The Makefile's `NODE` variable defaults to an `nvm`
> directory. If you manage Node another way (Homebrew, `fnm`, `asdf`, Volta),
> override it rather than editing the Makefile:
> `make web NODE="$(dirname "$(command -v node)")"`

## Tests and checks

```sh
make test        # go vet ./... && go test ./...
make fmt-check   # gofmt gate; fails listing unformatted files
make test-e2e    # build-tagged integration suite; needs Postgres on :5433
```

`make test-e2e` builds the real binaries, provisions a throwaway database,
starts a server plus a shell runner, and drives pipelines through the HTTP API.
Docker-gated subtests skip themselves when `docker info` fails, so it is safe
to run without Docker — you just get less coverage.

CI runs the same checks; `gofmt` and `go vet` are gates, so run `make fmt-check`
before pushing.

## Repository map

| Path | What lives there |
|---|---|
| `cmd/server` | control plane: REST API, YAML→DAG compiler, scheduler, approvals |
| `cmd/runner` | pull-based build agent; `shell` and `docker` executors |
| `internal/compiler` | pipeline YAML → job DAG, `only`/`except` ref resolution |
| `internal/scheduler` | promote/block/cancel/expire ticks (idempotent SQL) |
| `internal/store` | Postgres access; the single source of truth |
| `internal/api` | HTTP handlers, auth/SSO, audit log, CSRF |
| `web/` | React + Vite dashboard |
| `deploy/` | Dockerfiles and Terraform |
| `docs/` | design and operations documentation |

## Pull requests

- Keep them focused; one behavioural change per PR reviews far faster.
- Add or update tests. The scheduler and compiler are the load-bearing parts
  and both have real coverage — please keep it that way.
- Update the relevant file in `docs/` when you change behaviour, and the README
  when you change the DSL or the API surface.
- Describe the failure mode you fixed, not only the change you made.

## Reporting security issues

Please do **not** open a public issue for a vulnerability. See
[SECURITY.md](SECURITY.md) for the private reporting process.

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
