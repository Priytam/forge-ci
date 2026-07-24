#!/usr/bin/env bash
# self-host-ci.sh — register THIS repo with a running Forge server, register the
# repo's .forge-ci.yml pipeline config, and trigger a run: Forge running its own
# CI. See docs/self-hosted-ci.md for the full walkthrough (including pointing a
# docker runner at the server).
#
# Usage:
#   scripts/self-host-ci.sh [ref]
#
# Env:
#   SERVER_URL   Forge control-plane base URL      (default http://localhost:8080)
#   REPO         logical repo name in Forge         (default forge/forge-ci)
#   CLONE_URL    git URL Forge clones               (default file://<this repo>)
#
# Prereqs: a running forge-server (make server) and at least one docker runner
# with Go available in its job images (make runner-docker). jq is required.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
SERVER="${SERVER_URL:-http://localhost:8080}"
REPO="${REPO:-forge/forge-ci}"
REF="${1:-main}"
CLONE_URL="${CLONE_URL:-file://$DIR}"
SHA="$(git -C "$DIR" rev-parse "$REF" 2>/dev/null || git -C "$DIR" rev-parse HEAD)"
CONFIG="$(cat "$DIR/.forge-ci.yml")"

echo "server:    $SERVER"
echo "repo:      $REPO"
echo "clone_url: $CLONE_URL"
echo "ref/sha:   $REF / $SHA"
echo

echo "== register the repo (so Forge can clone it) =="
curl -sS -o /dev/null -w "  repo-registry: HTTP %{http_code}\n" \
  -X POST "$SERVER/api/v1/repo-registry" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg repo "$REPO" --arg url "$CLONE_URL" \
        '{repo:$repo, provider:"other", clone_url:$url, default_branch:"main"}')"

echo "== register the pipeline config (.forge-ci.yml) =="
curl -sS -o /dev/null -w "  repo-configs: HTTP %{http_code}\n" \
  -X PUT "$SERVER/api/v1/repo-configs" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg repo "$REPO" --arg cfg "$CONFIG" \
        '{repo:$repo, config:$cfg, author:"self-host-ci.sh", message:"self-hosted CI"}')"

echo "== trigger a pipeline =="
curl -sS -X POST "$SERVER/api/v1/pipelines" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg repo "$REPO" --arg ref "$REF" --arg sha "$SHA" --arg cfg "$CONFIG" \
        '{repo:$repo, ref:$ref, sha:$sha, config:$cfg}')" | jq '.pipeline | {id, repo, ref, status}'

echo
echo "Watch it in the dashboard (make web → http://localhost:5173) or poll:"
echo "  curl -s $SERVER/api/v1/pipelines?repo=$REPO | jq '.[0]'"
