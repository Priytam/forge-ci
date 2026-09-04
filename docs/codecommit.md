# AWS CodeCommit

CodeCommit is a first-class source provider: push and pull-request triggering,
`.forge-ci.yml` read at the event commit, IAM-signed source checkout, and result
feedback on pull requests.

It differs from GitHub and Bitbucket in three ways that shape the whole
integration, and all three are AWS's design, not Forge's:

| | GitHub / Bitbucket | CodeCommit |
| --- | --- | --- |
| Events | webhooks posted by the provider | **EventBridge**, forwarded by an API Destination |
| Auth | an HTTPS token Forge stores | **IAM SigV4** — no token exists to store |
| Result feedback | a commit status (green tick / red X) | **a pull-request comment** — there is no status API |

The practical consequence: **Forge stores no credential for a CodeCommit repo.**
The registry row holds a region, an optional profile and an optional role ARN,
all non-secret and all returned by the API.

---

## Two AWS identities

This is the thing to get straight before configuring anything. Forge uses
**two separate AWS identities**, for two different jobs:

| Who | What it does | Where its credentials come from | Permissions needed |
| --- | --- | --- | --- |
| **Control plane** (`forge-server`) | reads `.forge-ci.yml` (GetFile), validates registration (GetRepository), posts PR comments (PostCommentForPullRequest) | its own AWS credential chain — env, shared config, EC2/ECS role — optionally assuming `aws_role_arn` | `codecommit:GetFile`, `codecommit:GetRepository`, `codecommit:PostCommentForPullRequest` |
| **Runner** (`forge-runner`) | clones the source | **keyless**: the job's OIDC token → STS `AssumeRoleWithWebIdentity` | `codecommit:GitPull` |

The runner is keyless by design; the control plane is not, because a per-job
OIDC token belongs to a job and the server is not running one. **The control
plane therefore needs a real AWS identity** — an instance/task role in AWS, or
`AWS_*` environment credentials elsewhere. This is the one deployment
prerequisite that is easy to miss.

---

## 1. Register the repository

The Forge repo key is the **CodeCommit repository name alone** — no `owner/`
prefix. It has to match the `repositoryName` in the EventBridge event.

```bash
curl -X POST https://forge.example.com/api/v1/repo-registry \
  -H 'Content-Type: application/json' \
  -d '{
        "repo": "tablespace-api",
        "provider": "codecommit",
        "aws_region": "ap-south-1",
        "aws_role_arn": "arn:aws:iam::123456789012:role/forge-codecommit",
        "default_branch": "main"
      }'
```

Or use **Add repository** in the dashboard and pick *AWS CodeCommit*.

| Field | Notes |
| --- | --- |
| `aws_region` | required, unless a `clone_url` encodes it |
| `clone_url` | optional; derived as `codecommit::<region>://<repo>` when omitted. The GRC HTTPS form is also accepted, and the region is read back out of either |
| `aws_role_arn` | optional; assumed by the control plane, and the default role the runner assumes |
| `aws_profile` | optional named profile in the control plane's shared AWS config |
| `token` | **rejected** — CodeCommit has none |

Registration is verified with `GetRepository` before saving (skip with
`?validate=0`). `git ls-remote` is not used: git cannot reach CodeCommit without
IAM-signed git auth on the server, which Forge does not require.

## 2. Wire EventBridge to the webhook route

Forge exposes `POST /api/v1/webhooks/codecommit`, auth-exempt like the other
webhook routes. Point an EventBridge **API Destination** at it.

```bash
# One connection (API Destinations require an auth type; Forge does not read it)
aws events create-connection --name forge-ci \
  --authorization-type API_KEY \
  --auth-parameters 'ApiKeyAuthParameters={ApiKeyName=X-Forge,ApiKeyValue=unused}'

aws events create-api-destination --name forge-ci \
  --connection-arn <connection-arn> \
  --invocation-endpoint https://forge.example.com/api/v1/webhooks/codecommit \
  --http-method POST

# Push + pull-request events for one repository
aws events put-rule --name forge-ci-tablespace-api --event-pattern '{
  "source": ["aws.codecommit"],
  "detail-type": [
    "CodeCommit Repository State Change",
    "CodeCommit Pull Request State Change"
  ],
  "resources": ["arn:aws:codecommit:ap-south-1:123456789012:tablespace-api"]
}'

aws events put-targets --rule forge-ci-tablespace-api \
  --targets 'Id=forge,Arn=<api-destination-arn>,RoleArn=<events-invoke-role-arn>'
```

The **whole event envelope** must reach Forge (the default — do not set an input
transformer that strips it): the envelope `id` is the dedup key that makes a
redelivered event a no-op, exactly as `X-GitHub-Delivery` does for GitHub.

What triggers what:

| `detail.event` | Result |
| --- | --- |
| `referenceCreated`, `referenceUpdated` | push pipeline at `detail.commitId` |
| `pullRequestCreated`, `pullRequestSourceBranchUpdated` | `merge_request` pipeline on the PR source commit, with the `CI_MERGE_REQUEST_*` context |
| `referenceDeleted` | ignored (202) — no commit to build |
| `pullRequestStatusChanged`, `pullRequestMergeStatusUpdated` | ignored (202) — closed/merged |

## 3. IAM roles

**The repo role** (`aws_role_arn`) is assumed by *both* identities, so its trust
policy carries two statements — the control plane's principal, and the OIDC
federation for runners:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "AWS": "arn:aws:iam::123456789012:role/forge-server" },
      "Action": "sts:AssumeRole"
    },
    {
      "Effect": "Allow",
      "Principal": { "Federated": "arn:aws:iam::123456789012:oidc-provider/forge.example.com" },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": { "forge.example.com:aud": "sts.amazonaws.com" },
        "StringLike": { "forge.example.com:sub": "repo:tablespace-api:*" }
      }
    }
  ]
}
```

The `sub` condition is what makes the keyless path safe: Forge's OIDC token
carries `repo:<repo>:ref:<ref>`, so a runner can only clone the repository whose
pipeline it is actually running. See [oidc.md](oidc.md) for creating the IAM
OIDC provider — the same one deploy jobs already use.

Permissions policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "codecommit:GitPull",
      "codecommit:GetFile",
      "codecommit:GetRepository",
      "codecommit:PostCommentForPullRequest"
    ],
    "Resource": "arn:aws:codecommit:ap-south-1:123456789012:tablespace-api"
  }]
}
```

Split it into two roles if you would rather the runner hold only `GitPull`; set
the runner's with `FORGE_AWS_ROLE_ARN` on the runner.

## 4. Runner configuration

Nothing, in the default case. The runner signs the clone itself, so
`git-remote-codecommit` does **not** need to be installed.

| Env var | Effect |
| --- | --- |
| — (default) | keyless: `FORGE_OIDC_TOKEN` → `AssumeRoleWithWebIdentity` on the repo's role |
| `FORGE_AWS_ROLE_ARN` | overrides the role from the registry |
| `FORGE_AWS_AUTH=instance` | skip OIDC; use the EC2/ECS/env credential chain |

The clone URL Forge sends the runner carries **no credential**. The runner
resolves credentials, signs a short-lived SigV4 URL, and registers the signature
and session token for log redaction *before* running git — so a failed clone
that echoes the remote back cannot leak them.

---

## Result feedback: a comment, not a check

**CodeCommit has no commit-status API.** There is no green tick to set, and
Forge does not fake one. When a pipeline that came from a pull request finishes,
Forge posts a comment on that pull request:

```
**forge-ci** — ✅ Pipeline succeeded

https://forge.example.com/pipelines/482
```

Consequences worth knowing before you rely on it:

- **Only pull-request runs report.** A push pipeline has nowhere to comment;
  it posts nothing and errors nowhere.
- **One comment per run, at the final result** (success / failed / canceled).
  A commit status is overwritten in place as a run progresses; a comment cannot
  be, so narrating every phase would spam the pull request.
- **CodeCommit has no required-checks mechanism keyed on this.** Approval rule
  templates cannot gate a merge on a Forge comment. If you need a hard merge
  gate, that must come from Forge's own [approval gates](rbac-approvals.md), not
  from CodeCommit.

## Config from the repo

`.forge-ci.yml` is read with `GetFile` at the exact event commit, honouring the
repo's `config_source` toggle and `config_path` override — the same behaviour as
the other providers. A missing file (or unknown commit/repo) falls back to the
registered config rather than failing the pipeline.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `could not reach the CodeCommit repository` on registration | the control plane has no AWS identity, or it lacks `codecommit:GetRepository` |
| Pipelines use the registered config, never the in-repo one | control plane lacks `codecommit:GetFile`; the fall-back is logged with the AWS error code |
| `checkout failed: codecommit checkout: … AccessDenied` | the runner's role lacks `codecommit:GitPull`, or the trust policy's `sub` condition does not match this repo |
| `checkout failed: … no AWS credentials resolved` | no `FORGE_OIDC_TOKEN` and no ambient role — set `FORGE_AWS_AUTH=instance` with an instance role, or check the OIDC setup |
| No pipeline on push | the EventBridge rule is not matching, or an input transformer stripped the envelope; check `resources` in the rule |
| Duplicate pipelines | the envelope `id` is missing — the event is being reshaped before delivery |
| No comment on a PR | the run was a push run (expected), or the identity lacks `codecommit:PostCommentForPullRequest` |

## Environment variables

| Var | Component | Purpose |
| --- | --- | --- |
| `CODECOMMIT_API_BASE` | server | override the CodeCommit endpoint (testing) |
| `FORGE_AWS_ROLE_ARN` | runner | override the role assumed for the clone |
| `FORGE_AWS_AUTH` | runner | `instance` pins the ambient credential chain |
| `AWS_REGION`, `AWS_PROFILE`, … | both | standard AWS SDK configuration |
