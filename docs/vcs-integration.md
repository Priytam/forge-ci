# GitHub / Bitbucket integration

Forge is a standalone CI system — it does not host repositories. Two things
connect it to your VCS:

1. **A registered pipeline config** — Forge stores the pipeline YAML per repo
   (it can't read `.forge-ci.yml` out of a repo it doesn't host):
   ```sh
   curl -X PUT $FORGE/api/v1/repo-configs \
     -d "$(jq -n --arg cfg "$(cat forge-ci.yml)" '{repo:"acme/checkout-service", config:$cfg}')"
   ```
   Re-PUT to update; the YAML is validated on save.

2. **A push webhook** from the provider.

## GitHub — step by step

1. Register the repo's pipeline config (above). Use the exact
   `owner/repo` full name.
2. (Recommended) set a shared secret on the server: start `forge-server` with
   `WEBHOOK_SECRET=<random>` — GitHub's `X-Hub-Signature-256` HMAC is then
   verified on every delivery.
3. In GitHub: **Repo → Settings → Webhooks → Add webhook**
   - Payload URL: `https://<forge-host>/api/v1/webhooks/github`
   - Content type: `application/json`
   - Secret: the same `WEBHOOK_SECRET`
   - Events: *Just the push event*
4. Push a commit. GitHub's "Recent Deliveries" should show **201**, and the
   pipeline appears under the repo card in Forge with the pusher recorded as
   `triggered_by` (which drives the self-approval rule).

## Bitbucket Cloud — step by step

1. Register the repo's pipeline config with the Bitbucket
   `workspace/repo-slug` full name.
2. In Bitbucket: **Repository settings → Webhooks → Add webhook**
   - URL: `https://<forge-host>/api/v1/webhooks/bitbucket`
   - Triggers: *Repository push*
3. Push a commit; the pipeline triggers with the actor as `triggered_by`.
   (Bitbucket Cloud has no HMAC signing — restrict by network/allowlist or a
   token in the URL if exposure is a concern.)

## Behavior notes

- The ref from the webhook drives `only`/`except`, so pushes to `main` and to
  feature branches compile different DAGs from the same registered YAML.
- Non-push events (GitHub) and empty change lists (Bitbucket) are acknowledged
  with **202** and ignored.
- Commit status write-back (green tick on the commit) is on the roadmap — see
  the feature comparison doc.
