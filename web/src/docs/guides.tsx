import type { ReactElement } from "react";
import { Link } from "react-router-dom";
import { CodeBlock, DocTable, Note, Steps } from "../components/DocBlocks";

export interface Guide {
  slug: string;
  group: string;
  title: string;
  render: () => ReactElement;
}

export const GUIDE_GROUPS = [
  "Getting started",
  "Runners",
  "Pipelines",
  "Security",
] as const;

/* ================= Getting started ================= */

function AddARepo() {
  return (
    <>
      <p>
        Forge is a standalone CI system — it does not host your repository.
        Connecting a repo takes a registered connection + pipeline config plus
        a push webhook; the rest is policy. End to end:
      </p>
      <Steps>
        <li>
          <strong>Connect the repository</strong> (Repos →{" "}
          <Link to="/repos/new">Add repository</Link>): pick the provider,
          enter the <code>owner/name</code> full name, and optionally an
          access token — none needed for public repos; for private ones use a
          fine-grained PAT (GitHub) or repository access token (Bitbucket)
          with read access. The token is stored server-side and never shown
          again. Forge verifies access with <code>git ls-remote</code> on
          save, and once connected, <strong>runners clone the source
          automatically</strong> at the pipeline's SHA before running jobs.
        </li>
        <li>
          <strong>Register the pipeline YAML.</strong> Forge stores the config
          per repo (it cannot read <code>.forge-ci.yml</code> out of a repo it
          does not host). Paste it in repo Settings → Pipeline config, or:
          <CodeBlock
            code={`curl -X PUT $FORGE/api/v1/repo-configs \\
  -d "$(jq -n --arg cfg "$(cat forge-ci.yml)" '{repo:"acme/checkout-service", config:$cfg}')"`}
          />
          Use the exact <code>owner/repo</code> (GitHub) or{" "}
          <code>workspace/repo-slug</code> (Bitbucket) full name. Re-PUT to
          update; the YAML is validated on save. See{" "}
          <Link to="/docs/writing-yaml">Writing pipeline YAML</Link>.
        </li>
        <li>
          <strong>Wire the push webhook.</strong>
          <p>
            GitHub: (recommended) start <code>forge-server</code> with{" "}
            <code>WEBHOOK_SECRET=&lt;random&gt;</code> so the{" "}
            <code>X-Hub-Signature-256</code> HMAC is verified on every
            delivery. Then in GitHub: <strong>Repo → Settings → Webhooks →
            Add webhook</strong> — Payload URL{" "}
            <code>https://&lt;forge-host&gt;/api/v1/webhooks/github</code>,
            Content type <code>application/json</code>, Secret = the same{" "}
            <code>WEBHOOK_SECRET</code>, Events: <em>Just the push event</em>.
          </p>
          <p>
            Bitbucket Cloud: <strong>Repository settings → Webhooks → Add
            webhook</strong> — URL{" "}
            <code>https://&lt;forge-host&gt;/api/v1/webhooks/bitbucket</code>,
            Triggers: <em>Repository push</em>. Bitbucket Cloud has no HMAC
            signing — restrict by network/allowlist or a token in the URL if
            exposure is a concern.
          </p>
        </li>
        <li>
          <strong>Add members and roles</strong> (Settings → Members) so the
          approval gate has someone to enforce against. Roles: admin, owner,
          developer. See <Link to="/docs/approvals">Approvals & RBAC</Link>.
        </li>
        <li>
          <strong>Define the approval rule for production</strong> (Settings →
          Members & approval rules): required approvals, allowed approver
          roles, self-approval, timeout. A global default (production, 1
          approval, admin/owner) ships out of the box; a repo rule of the same
          name overrides it.
        </li>
        <li>
          <strong>Add variables and secrets</strong> (Settings → Variables) —
          mark deploy credentials protected + masked. See{" "}
          <Link to="/docs/variables-secrets">Variables & secrets</Link>.
        </li>
        <li>
          <strong>Set repo runner tags</strong> (Settings → Runner tags) if
          this repo's jobs need a specific runner group, e.g.{" "}
          <code>gpu</code>. Leave empty to run on any runner. See{" "}
          <Link to="/docs/runner-groups">Runner groups & tags</Link>.
        </li>
        <li>
          <strong>Push a commit and watch.</strong> GitHub's "Recent
          Deliveries" should show <strong>201</strong>, and the pipeline
          appears under the repo card with the pusher recorded as{" "}
          <code>triggered_by</code> (which drives the self-approval rule).
        </li>
      </Steps>
      <Note tone="info" title="Ref-aware compilation">
        The ref from the webhook drives <code>only</code>/<code>except</code>,
        so pushes to main and to feature branches compile different DAGs from
        the same registered YAML — see{" "}
        <Link to="/docs/triggers">Configuring what runs for dev vs main</Link>.
      </Note>
    </>
  );
}

function Triggers() {
  return (
    <>
      <p>
        Every push webhook carries a <strong>ref</strong> (branch or tag).
        At compile time Forge matches each job's <code>only</code> /{" "}
        <code>except</code> glob patterns against that ref and{" "}
        <em>drops excluded jobs from the DAG entirely</em> — they never enter
        the pipeline, not even as skipped rows. One registered YAML therefore
        yields different pipelines for dev branches vs main.
      </p>
      <Steps>
        <li>
          <strong>Run a deploy only on main:</strong>
          <CodeBlock
            code={`deploy-prod:
  stage: deploy
  only: [main]
  environment: production
  script: [./deploy.sh prod]`}
          />
        </li>
        <li>
          <strong>Run a dev deploy on every non-main branch:</strong>
          <CodeBlock
            code={`deploy-dev:
  stage: deploy
  except: [main]
  script: [./deploy.sh dev]`}
          />
        </li>
        <li>
          <strong>Run a job only for release refs (globs):</strong>
          <CodeBlock
            code={`publish:
  stage: deploy
  only: [release-*]     # release-1.2, release-2024-q3, ...
  script: [./publish.sh]`}
          />
        </li>
      </Steps>
      <h2>What runs when</h2>
      <p>For this sample config:</p>
      <CodeBlock
        code={`stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    script: [make build]
  unit-tests:
    stage: test
    script: [make test]
  deploy-dev:
    stage: deploy
    except: [main]
    script: [./deploy.sh dev]
  deploy-prod:
    stage: deploy
    only: [main, release-*]
    environment: production
    script: [./deploy.sh prod]`}
      />
      <DocTable
        head={["Job", "push to main", "push to feature/x"]}
        rows={[
          ["build-app", "✓ runs", "✓ runs"],
          ["unit-tests", "✓ runs", "✓ runs"],
          ["deploy-dev", "— excluded", "✓ runs"],
          [
            "deploy-prod",
            "✓ runs (blocks for approval)",
            "— excluded",
          ],
        ]}
      />
      <Note tone="warn" title="Explicit needs vs excluded jobs">
        Implicit stage dependencies skip over stages left empty for a ref, but
        an explicit <code>needs:</code> pointing at a ref-excluded job is a
        compile error — the pipeline POST returns 400.
      </Note>
    </>
  );
}

/* ================= Runners ================= */

function RunnerDocker() {
  return (
    <>
      <p>
        The docker executor runs each job in its own container of the job's{" "}
        <code>image:</code> (default <code>alpine:3</code>), with the
        workspace bind-mounted and networking disabled. Runners are pull-based
        — they long-poll the control plane, so no inbound ports and no
        server-side pre-registration; a runner appears in the registry on its
        first poll.
      </p>
      <Steps>
        <li>
          Host needs Docker installed and the runner user in the{" "}
          <code>docker</code> group.
        </li>
        <li>
          Start the runner:
          <CodeBlock
            code={`./forge-runner --server=https://forge.internal.example.com \\
  --id=docker-01 --executor=docker --tags=docker,linux`}
          />
        </li>
        <li>
          Jobs pick their image per job:
          <CodeBlock
            code={`unit-tests:
  stage: test
  image: golang:1.24
  script: [go test ./...]`}
          />
        </li>
        <li>
          Verify it shows <strong>online</strong> on the{" "}
          <Link to="/runners">Runners page</Link> (or{" "}
          <code>GET /api/v1/runners</code>).
        </li>
      </Steps>
      <Note tone="info">
        Give every long-lived runner a stable <code>--id</code>; one runner
        processes one job at a time, so run one runner process per desired
        concurrent job.
      </Note>
    </>
  );
}

function RunnerKubernetes() {
  return (
    <>
      <p>
        The native <code>kubernetes</code> executor runs each job in its own
        ephemeral Pod, GitLab-style. Nothing fixed runs per job: one resident{" "}
        <strong>manager</strong> process acquires jobs and forks each one into
        its own Pod (created on acquire, deleted after).
      </p>
      <Steps>
        <li>
          The manager machine/pod needs <code>kubectl</code> with access to
          the target cluster.
        </li>
        <li>
          Start the manager — <code>KUBE_CONTEXT</code> is{" "}
          <strong>required</strong> (it refuses the kubeconfig
          current-context to protect shared kubeconfigs):
          <CodeBlock
            code={`KUBE_CONTEXT=my-ci-cluster KUBE_NAMESPACE=ci \\
./forge-runner --executor=kubernetes --id=k8s-manager-1 --tags=k8s \\
  --concurrency=4    # up to 4 pods in flight from one manager`}
          />
        </li>
        <li>
          <strong>Per-job lifecycle</strong> — the manager:
          <Steps>
            <li>
              creates pod <code>forge-job-&lt;id&gt;</code> from the job's{" "}
              <code>image:</code>;
            </li>
            <li>
              ships the prepared workspace in (clone + restored upstream
              artifacts);
            </li>
            <li>
              execs the script — logs stream live to the job log page, and
              the exit code propagates;
            </li>
            <li>copies declared artifact paths back out;</li>
            <li>deletes the pod.</li>
          </Steps>
        </li>
        <li>
          Job images need <code>sh</code> and <code>tar</code> (alpine and
          typical build images do).
        </li>
      </Steps>
      <Note tone="info" title="Operations">
        All job pods carry the <code>app=forge-ci-job</code> label — easy to
        watch (<code>kubectl get pods -l app=forge-ci-job</code>) and to
        policy. Orphaned pods self-terminate within 2h.{" "}
        <code>--concurrency N</code> lets one manager keep up to N pods in
        flight.
      </Note>
      <h2>Alternative: resident fleet</h2>
      <p>
        When per-job pod overhead isn't wanted, run N resident shell/docker
        runners <em>on</em> Kubernetes as a Deployment:
      </p>
      <Steps>
        <li>
          Build and push the runner image:
          <CodeBlock
            code={`docker build -f deploy/Dockerfile.runner -t <registry>/forge-runner:latest .
docker push <registry>/forge-runner:latest`}
          />
        </li>
        <li>
          Deploy N replicas:
          <CodeBlock
            code={`apiVersion: apps/v1
kind: Deployment
metadata: {name: forge-runners, namespace: ci}
spec:
  replicas: 4
  selector: {matchLabels: {app: forge-runner}}
  template:
    metadata: {labels: {app: forge-runner}}
    spec:
      containers:
      - name: runner
        image: <registry>/forge-runner:latest
        env:
        - {name: SERVER_URL, value: "http://forge-server.ci.svc:8080"}
        - {name: EXECUTOR, value: "shell"}
        - {name: RUNNER_TAGS, value: "k8s,linux"}
        - name: RUNNER_ID
          valueFrom: {fieldRef: {fieldPath: metadata.name}}`}
          />
        </li>
        <li>
          Apply and scale:
          <CodeBlock
            code={`kubectl apply -f runners.yaml
kubectl scale deploy/forge-runners --replicas=10`}
          />
        </li>
        <li>
          Each pod registers itself by pod name — the{" "}
          <Link to="/runners">Runners page</Link> shows one row per pod
          (ephemeral pods use pod names as runner ids).
        </li>
      </Steps>
      <Note tone="info">
        A runner that dies mid-job stops heartbeating; the scheduler fails the
        job after 90s and the registry shows the runner offline after 30s —
        pod churn is safe.
      </Note>
    </>
  );
}

function RunnerVM() {
  return (
    <>
      <p>
        The shell executor runs job scripts directly on the host — fastest,
        least isolated. Use for trusted, internal workloads only.
      </p>
      <Steps>
        <li>
          Build or copy the binary:{" "}
          <code>go build -o forge-runner ./cmd/runner</code> (cross-compile
          with <code>GOOS=linux GOARCH=amd64 go build ...</code>).
        </li>
        <li>
          Start it:
          <CodeBlock
            code={`./forge-runner --server=https://forge.internal.example.com \\
  --id=vm-build-01 --executor=shell --tags=linux,amd64`}
          />
        </li>
        <li>
          Make it survive reboots — systemd unit{" "}
          <code>/etc/systemd/system/forge-runner.service</code>:
          <CodeBlock
            code={`[Unit]
Description=Forge CI runner
After=network-online.target
[Service]
ExecStart=/usr/local/bin/forge-runner --server=https://forge.internal.example.com --id=%H --executor=shell --tags=linux
Restart=always
User=forge
[Install]
WantedBy=multi-user.target`}
          />
          Then <code>systemctl enable --now forge-runner</code>.
        </li>
        <li>
          Verify it shows <strong>online</strong> on the{" "}
          <Link to="/runners">Runners page</Link>.
        </li>
      </Steps>
      <Note tone="warn" title="Stable identity">
        Give every long-lived runner a stable <code>--id</code> (the systemd
        unit above uses <code>%H</code>, the hostname). Without one, the id
        defaults to host-pid and changes on every restart, leaving stale
        offline rows in the registry.
      </Note>
    </>
  );
}

function RunnerGroups() {
  return (
    <>
      <p>
        Runners are deployed independently of repos — the fleet is global, and{" "}
        <strong>tags</strong> are how work is routed to it. A set of
        same-tagged runners is a "group".
      </p>
      <Steps>
        <li>
          <strong>Deploy runners with capability tags:</strong>{" "}
          <code>--tags=gpu</code>, <code>--tags=docker,linux</code>,{" "}
          <code>--tags=prod-deploy</code>.
        </li>
        <li>
          <strong>A repo selects its group with default runner tags</strong>{" "}
          (repo Settings → Runner tags, or the API):
          <CodeBlock
            code={`curl -X PUT $FORGE/api/v1/repo-settings \\
  -d '{"repo": "acme/ml", "default_runner_tags": ["gpu"]}'`}
          />
          Every job of that repo without its own <code>tags:</code> inherits
          them.
        </li>
        <li>
          <strong>A job-level <code>tags:</code> list overrides</strong> the
          repo default for that job:
          <CodeBlock
            code={`train-model:
  stage: train
  tags: [gpu]          # this job only: needs a gpu runner
  script: [python train.py]`}
          />
        </li>
        <li>
          <strong>Matching rule:</strong> a job runs only on a runner
          advertising <em>all</em> of its effective tags; a job with no
          effective tags runs on any runner.
        </li>
        <li>
          <strong>Pause / drain:</strong> the Pause button on the{" "}
          <Link to="/runners">Runners page</Link> (or{" "}
          <code>POST /api/v1/runners/&#123;id&#125;/pause</code>) drains a
          runner without killing it — it finishes its current job and stops
          claiming new ones.
        </li>
      </Steps>
      <Note tone="warn">
        Tags are capability labels, not queues — if no online runner
        advertises all of a job's tags, the job sits in{" "}
        <code>pending</code> until one appears.
      </Note>
      <Note tone="info" title="Concurrency">
        A shell/docker runner processes one job at a time — run one process
        per desired concurrent job. The kubernetes executor instead takes{" "}
        <code>--concurrency N</code>: one manager process keeps up to N job
        pods in flight. See{" "}
        <Link to="/docs/runner-kubernetes">Kubernetes runner fleet</Link>.
      </Note>
    </>
  );
}

/* ================= Pipelines ================= */

function WritingYaml() {
  return (
    <>
      <p>
        A pipeline config declares ordered <code>stages</code> and a map of{" "}
        <code>jobs</code>. Full annotated example:
      </p>
      <CodeBlock
        code={`stages: [build, test, deploy]       # ordered stage list
jobs:
  build-app:
    stage: build
    image: alpine:3                  # docker executor: container image
    script:                          # required: shell lines, run in order
      - echo "compiling..."
    artifacts:
      paths: [dist/, report.txt]     # archived on success (workspace-relative)
  unit-tests:
    stage: test                      # no explicit needs -> depends on the
    script: [echo testing]           #   previous stage's jobs (implicit)
  deploy-dev:
    stage: deploy
    except: [main]                   # ref-aware: dropped from the DAG on main
    script: [echo dev deploy]
  deploy-prod:
    stage: deploy
    needs: [unit-tests]              # explicit needs: must be earlier-stage jobs
    environment: production          # protected env -> blocks for approval
    only: [main, release-*]          # ref-aware: glob-matched against the ref
    tags: [prod-deploy]              # route to runners with ALL these tags
    variables: {REGION: ap-south-1}  # per-job env vars (merged over repo vars)
    script: [echo deploying]`}
      />
      <h2>Keys</h2>
      <DocTable
        head={["Key", "Meaning"]}
        rows={[
          [<code>stages</code>, "Ordered list; jobs in a stage start together"],
          [<code>script</code>, "Required. Shell lines executed in order; non-zero exit fails the job"],
          [<code>image</code>, <>Container image for the docker executor (default <code>alpine:3</code>); ignored by shell runners</>],
          [<code>needs</code>, "Explicit upstream job names (must be in earlier stages). Omitted: implicit dependency on the previous stage"],
          [<code>variables</code>, "Per-job env vars; merged over repo-level variables"],
          [<code>environment</code>, "Names a deployment target; blocks for approval when it matches a protected environment"],
          [<code>tags</code>, "Runner routing: job runs only on a runner advertising all tags (overrides repo default tags)"],
          [<code>artifacts.paths</code>, "Workspace-relative paths archived as artifacts.tar.gz on success; missing paths are skipped"],
          [<><code>only</code> / <code>except</code></>, "Ref globs; excluded jobs are dropped from the DAG at compile time"],
        ]}
      />
      <h2>Artifact passing</h2>
      <p>
        A job automatically receives the artifacts of the jobs it{" "}
        <code>needs</code> (implicit needs = the previous stage), restored
        into its workspace before the script runs — no download step needed:
      </p>
      <CodeBlock
        code={`stages: [build, test]
jobs:
  build:
    stage: build
    artifacts:
      paths: [dist/]
    script:
      - mkdir -p dist
      - echo "hello from build" > dist/hello.txt
  verify:
    stage: test                  # implicit needs: build
    script:
      - cat dist/hello.txt       # dist/ was restored before this runs`}
      />
      <h2>Example 1 — simple build → test → deploy</h2>
      <CodeBlock
        code={`stages: [build, test, deploy]
jobs:
  build:
    stage: build
    script: [make build]
  test:
    stage: test
    script: [make test]
  deploy:
    stage: deploy
    only: [main]
    environment: production
    script: [make deploy]`}
      />
      <h2>Example 2 — DAG with needs fan-out</h2>
      <CodeBlock
        code={`stages: [build, test, package]
jobs:
  build:
    stage: build
    script: [make build]
  lint:
    stage: test
    needs: [build]
    script: [make lint]
  unit:
    stage: test
    needs: [build]
    script: [make unit]
  integration:
    stage: test
    needs: [build]
    script: [make integration]
  package:
    stage: package
    needs: [lint, unit, integration]   # waits for all three in parallel
    script: [make package]`}
      />
      <h2>Example 3 — monorepo-style with tags + artifacts</h2>
      <CodeBlock
        code={`stages: [build, deploy]
jobs:
  build-api:
    stage: build
    image: golang:1.24
    tags: [docker, linux]
    artifacts:
      paths: [bin/api]
    script: [go build -o bin/api ./cmd/api]
  build-web:
    stage: build
    image: node:22-alpine
    tags: [docker, linux]
    artifacts:
      paths: [web/dist/]
    script: [npm ci --prefix web, npm run build --prefix web]
  deploy:
    stage: deploy
    needs: [build-api, build-web]
    only: [main]
    environment: production
    tags: [prod-deploy]
    script: [./deploy.sh]`}
      />
      <Note tone="info">
        Test a config without a webhook: POST it directly from the{" "}
        <Link to="/new">New Pipeline</Link> form — the same compiler runs, so
        400 errors surface immediately.
      </Note>
    </>
  );
}

function ArtifactsGuide() {
  return (
    <>
      <p>
        Runners archive the <code>artifacts.paths</code> of a successful job
        as <code>artifacts.tar.gz</code> and stream it to the control plane,
        which writes it to the configured blob store. Runners never talk to
        the store directly and need no storage credentials.
      </p>
      <Note tone="info" title="Artifact passing between jobs">
        Downstream jobs automatically receive the artifacts of the jobs they{" "}
        <code>needs</code> (implicit needs = the previous stage) — restored
        into their workspace before the script runs. See{" "}
        <Link to="/docs/writing-yaml">Writing pipeline YAML → Artifact
        passing</Link>.
      </Note>
      <Steps>
        <li>
          <strong>Declare paths in the job:</strong>
          <CodeBlock
            code={`build-app:
  stage: build
  artifacts:
    paths: [dist/, report.txt]   # workspace-relative; missing paths are skipped
  script: [make build]`}
          />
        </li>
        <li>
          <strong>Download from the UI</strong> — the job log page shows an
          Artifacts box, and repo Settings → Artifacts lists every artifact
          with a Download button.
        </li>
        <li>
          <strong>Or from the API:</strong>
          <CodeBlock
            code={`GET /api/v1/artifacts?repo=<repo>          # list (add &job=<jobId> to filter)
GET /api/v1/artifacts/{id}/download        # streams the .tar.gz`}
          />
        </li>
      </Steps>
      <h2>Storage backends</h2>
      <p>
        Chosen with environment variables on <code>forge-server</code>. The{" "}
        <code>s3</code> backend speaks the S3 API and therefore covers AWS S3,
        MinIO, and Google Cloud Storage (interoperability mode) with the same
        configuration.
      </p>
      <DocTable
        head={["Env var", "Meaning", "Default"]}
        rows={[
          [<code>ARTIFACT_STORE</code>, <><code>local</code> or <code>s3</code></>, <code>local</code>],
          [<code>ARTIFACTS_DIR</code>, "local backend: directory for blobs", <code>data/artifacts</code>],
          [<code>S3_ENDPOINT</code>, "s3 backend: host[:port], no scheme", "—"],
          [<code>S3_BUCKET</code>, "bucket name", "—"],
          [<><code>S3_ACCESS_KEY</code> / <code>S3_SECRET_KEY</code></>, "credentials", "—"],
          [<code>S3_USE_SSL</code>, <><code>true</code>/<code>false</code></>, <code>true</code>],
          [<code>S3_REGION</code>, "optional region", <code>""</code>],
        ]}
      />
      <p>Quick pointers per backend:</p>
      <Steps>
        <li>
          <strong>Local disk (default, dev):</strong> nothing to configure;
          blobs land in <code>data/artifacts/job-&lt;id&gt;/</code>. Single-server
          dev setups only.
        </li>
        <li>
          <strong>MinIO (self-hosted):</strong> create bucket{" "}
          <code>forge-artifacts</code> and a dedicated access key, then:
          <CodeBlock
            code={`ARTIFACT_STORE=s3 S3_ENDPOINT=localhost:9000 S3_BUCKET=forge-artifacts \\
S3_ACCESS_KEY=<key> S3_SECRET_KEY=<secret> S3_USE_SSL=false ./bin/forge-server`}
          />
        </li>
        <li>
          <strong>AWS S3:</strong> least-privilege IAM (PutObject, GetObject,
          HeadObject on the bucket), then{" "}
          <code>S3_ENDPOINT=s3.&lt;region&gt;.amazonaws.com S3_REGION=&lt;region&gt;</code>.
          Enable default encryption and add a lifecycle rule expiring{" "}
          <code>job-*</code> prefixes — Forge does not garbage-collect
          artifacts yet.
        </li>
        <li>
          <strong>GCS (interoperability mode):</strong> service account with{" "}
          <code>roles/storage.objectAdmin</code> on the bucket, create an HMAC
          key (<code>gcloud storage hmac create ...</code>), then{" "}
          <code>S3_ENDPOINT=storage.googleapis.com</code> with the HMAC
          accessId/secret as the S3 keys.
        </li>
      </Steps>
    </>
  );
}

/* ================= Security ================= */

function Approvals() {
  return (
    <>
      <p>
        A job with <code>environment: &lt;name&gt;</code> blocks before
        running when the name matches a protected environment. Rules live
        server-side — a PR editing pipeline YAML cannot weaken them.
      </p>
      <h2>Roles</h2>
      <DocTable
        head={["Role", "Intent"]}
        rows={[
          [<code>admin</code>, "Manages the repo's CI: variables, members, approval rules"],
          [<code>owner</code>, "Senior reviewer; typically an allowed production approver"],
          [<code>developer</code>, "Runs pipelines; cannot approve protected deployments by default"],
        ]}
      />
      <Steps>
        <li>
          <strong>Add members</strong> (repo Settings → Members, or the API):
          <CodeBlock
            code={`curl -X POST $FORGE/api/v1/members \\
  -d '{"repo":"acme/checkout-service","username":"sre-lead","role":"owner"}'`}
          />
        </li>
        <li>
          <strong>Define the rule</strong> (Settings → Members & approval
          rules). The rule with <code>repo</code> set overrides the global
          (<code>repo:""</code>) default of the same name:
          <CodeBlock
            code={`curl -X POST $FORGE/api/v1/protected-environments -d '{
  "repo": "acme/checkout-service",
  "name": "production",
  "required_approvals": 2,
  "approval_timeout_hours": 24,
  "approver_roles": ["admin", "owner"],
  "allow_self_approval": false
}'`}
          />
          <DocTable
            head={["Field", "Behavior"]}
            rows={[
              [<code>required_approvals</code>, "Votes needed before the job is released to a runner"],
              [<code>approver_roles</code>, "Only members with one of these roles may vote"],
              [<code>allow_self_approval</code>, <>When false, the pipeline's <code>triggered_by</code> user cannot approve their own deployment (separation of duties)</>],
              [<code>approval_timeout_hours</code>, "Blocked jobs fail automatically after this window"],
            ]}
          />
        </li>
        <li>
          <strong>Vote</strong> from the pipeline view (Approve / Reject on
          the blocked job) or{" "}
          <code>POST /api/v1/jobs/&#123;id&#125;/approvals</code> with{" "}
          <code>{`{"approver","verdict","comment"}`}</code>. Server-enforced,
          audited in <code>job_approvals</code>:
          <DocTable
            head={["Situation", "Response"]}
            rows={[
              [<>approver's role not in <code>approver_roles</code></>, <strong>403</strong>],
              ["approver == pipeline author and self-approval disabled", <strong>403</strong>],
              ["second vote by the same approver", <strong>409</strong>],
              [<>any <code>rejected</code> vote</>, "job → failed"],
              [<><code>required_approvals</code> approvals reached</>, "job → pending (released)"],
            ]}
          />
        </li>
      </Steps>
      <Note tone="warn" title="Bootstrap mode">
        A repo with <strong>zero</strong> members skips role checks entirely —
        anyone may approve. Enforcement turns on the moment the first member
        is added. Add members before you rely on the gate.
      </Note>
      <Note tone="warn" title="Identity caveat">
        Forge does not yet authenticate users — the approver name is
        client-asserted. RBAC is fully enforced server-side, but until an OIDC
        proxy fronts the API, identity itself is trust-based. Front{" "}
        <code>forge-server</code> with an authenticating reverse proxy before
        production use.
      </Note>
    </>
  );
}

function VariablesSecrets() {
  return (
    <>
      <p>
        Repo variables (Settings → Variables) are injected into job
        environments. Three switches control exposure:
      </p>
      <Steps>
        <li>
          <strong>Protected</strong> — the variable is only exposed to jobs
          running on protected refs like <code>main</code>. Use for deploy
          credentials so feature-branch pipelines never see them.
        </li>
        <li>
          <strong>Masked</strong> — the value is redacted in job logs at
          ingestion. Masked values must be at least 8 characters with no
          whitespace (so the scanner can match them reliably). The UI hides
          masked values behind <code>********************</code> until you hit
          "Reveal values".
        </li>
        <li>
          <strong>Environment scope</strong> — <code>*</code> applies
          everywhere; a specific scope (e.g. <code>production</code>) applies
          only to jobs with that <code>environment:</code>. Precedence:
          job-level <code>variables:</code> in YAML override repo variables.
        </li>
      </Steps>
      <Note tone="warn" title="Masking limitations">
        Masking is a log-scrubbing measure, not encryption. Logs are streamed
        in chunks, and a masked value split across two log chunks can escape
        redaction. Anything that base64-encodes or otherwise transforms a
        secret before printing defeats masking entirely.
      </Note>
      <Note tone="warn" title="Plaintext at rest">
        Variable values are currently stored plaintext in the database
        (envelope encryption / Vault integration is on the roadmap). Treat DB
        access as secret access; do not put long-lived, high-blast-radius
        credentials in variables yet.
      </Note>
    </>
  );
}

export const GUIDES: Guide[] = [
  {
    slug: "add-a-repo",
    group: "Getting started",
    title: "Add a new repo end to end",
    render: AddARepo,
  },
  {
    slug: "triggers",
    group: "Getting started",
    title: "Configuring what runs for dev vs main",
    render: Triggers,
  },
  {
    slug: "runner-docker",
    group: "Runners",
    title: "Docker runner",
    render: RunnerDocker,
  },
  {
    slug: "runner-kubernetes",
    group: "Runners",
    title: "Kubernetes runner fleet",
    render: RunnerKubernetes,
  },
  {
    slug: "runner-vm",
    group: "Runners",
    title: "VM / bare-metal runner",
    render: RunnerVM,
  },
  {
    slug: "runner-groups",
    group: "Runners",
    title: "Runner groups & tags",
    render: RunnerGroups,
  },
  {
    slug: "writing-yaml",
    group: "Pipelines",
    title: "Writing pipeline YAML",
    render: WritingYaml,
  },
  {
    slug: "artifacts",
    group: "Pipelines",
    title: "Artifacts & storage",
    render: ArtifactsGuide,
  },
  {
    slug: "approvals",
    group: "Security",
    title: "Approvals & RBAC",
    render: Approvals,
  },
  {
    slug: "variables-secrets",
    group: "Security",
    title: "Variables & secrets",
    render: VariablesSecrets,
  },
];
