#!/usr/bin/env bash
# Trigger the demo pipeline against a locally running forge-server.
set -euo pipefail

SERVER="${SERVER_URL:-http://localhost:8080}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

CONFIG="$(cat "$DIR/examples/demo-pipeline.yml")"
BODY="$(jq -n --arg cfg "$CONFIG" \
  '{repo: "demo/app", ref: "main", sha: "deadbeefcafe1234", config: $cfg}')"

curl -sS -X POST "$SERVER/api/v1/pipelines" \
  -H 'Content-Type: application/json' \
  -d "$BODY" | jq .
