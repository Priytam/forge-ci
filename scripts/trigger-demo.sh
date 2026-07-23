#!/usr/bin/env bash
# Trigger the demo pipeline. Usage: trigger-demo.sh [repo] [ref] [sha]
set -euo pipefail

SERVER="${SERVER_URL:-http://localhost:8080}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

REPO="${1:-demo/app}"
REF="${2:-main}"
SHA="${3:-$(printf '%08x%08x' $RANDOM$RANDOM $RANDOM$RANDOM)}"

CONFIG="$(cat "$DIR/examples/demo-pipeline.yml")"
BODY="$(jq -n --arg cfg "$CONFIG" --arg repo "$REPO" --arg ref "$REF" --arg sha "$SHA" \
  '{repo: $repo, ref: $ref, sha: $sha, config: $cfg}')"

curl -sS -X POST "$SERVER/api/v1/pipelines" \
  -H 'Content-Type: application/json' \
  -d "$BODY" | jq .
