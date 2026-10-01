#!/bin/sh
# Register this machine as a Forge CI runner. macOS and Linux.
#
# Usage (the Runner tokens page gives you this exact line, filled in):
#   curl -fsSL <server>/api/v1/downloads/install-runner.sh | SERVER=<url> TOKEN=<token> TAGS=<tags> sh
#
# Env:
#   SERVER   required — the Forge CI control-plane URL
#   TOKEN    the runner auth token (only needed when the server runs RUNNER_AUTH=on)
#   TAGS     comma-separated runner tags (default: qa-laptop)
#   ID       runner id shown in the Runners page (default: <hostname>-<timestamp>)
#   DIR      where to put the binary (default: current directory)
#
# On Windows, use install-runner.ps1 instead.
set -e

: "${SERVER:?SERVER is required, e.g. SERVER=https://forge-ci.example.com}"
TAGS="${TAGS:-qa-laptop}"
ID="${ID:-$(hostname)-$(date +%s)}"
DIR="${DIR:-.}"

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS-$ARCH" in
  Darwin-arm64)  BIN=forge-runner-darwin-arm64 ;;
  Darwin-x86_64) BIN=forge-runner-darwin-amd64 ;;
  Linux-x86_64)  BIN=forge-runner-linux-amd64 ;;
  *)
    echo "forge-runner: no prebuilt binary for $OS/$ARCH." >&2
    echo "Build one yourself: GOOS=... GOARCH=... go build ./cmd/runner (see the forge-ci repo)." >&2
    exit 1
    ;;
esac

echo "==> downloading $BIN"
curl -fsSL "$SERVER/api/v1/downloads/$BIN" -o "$DIR/forge-runner"
chmod +x "$DIR/forge-runner"

echo "==> registered as:  id=$ID  tags=$TAGS  server=$SERVER"
echo
if [ -n "${TOKEN:-}" ]; then
  echo "Starting the runner (Ctrl-C to stop):"
  echo
  exec "$DIR/forge-runner" --server="$SERVER" --id="$ID" --executor=shell --tags="$TAGS" --token="$TOKEN"
else
  echo "Run this to start it:"
  echo
  echo "  $DIR/forge-runner --server=$SERVER --id=$ID --executor=shell --tags=$TAGS"
fi
