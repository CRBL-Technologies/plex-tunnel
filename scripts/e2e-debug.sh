#!/usr/bin/env sh
set -eu

STACK_FILE="${STACK_FILE:-docker-compose.debug.yml}"
HOST_PORT="${HOST_PORT:-18080}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-45}"
TEST_HOST="${TEST_HOST:-myserver.local.test}"
SERVER_CONTEXT="${PLEXTUNNEL_SERVER_CONTEXT:-/tmp/plex-tunnel-server}"

cleanup() {
  if [ "${KEEP_STACK:-0}" = "1" ]; then
    return
  fi
  docker compose -f "$STACK_FILE" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [ -z "${PLEXTUNNEL_SERVER_IMAGE:-}" ]; then
  if [ -f "$SERVER_CONTEXT/Dockerfile.server" ]; then
    PLEXTUNNEL_SERVER_IMAGE="plextunnel/server:debug-local"
    export PLEXTUNNEL_SERVER_IMAGE
    echo "Building local server image from $SERVER_CONTEXT..."
    docker build -f "$SERVER_CONTEXT/Dockerfile.server" -t "$PLEXTUNNEL_SERVER_IMAGE" "$SERVER_CONTEXT"
  else
    echo "No local server source found at $SERVER_CONTEXT and PLEXTUNNEL_SERVER_IMAGE is not set."
    echo "Set PLEXTUNNEL_SERVER_IMAGE to a pullable image or set PLEXTUNNEL_SERVER_CONTEXT to the server repo path."
    exit 1
  fi
fi

echo "Starting local debug stack..."
docker compose -f "$STACK_FILE" up -d --build

echo "Waiting for end-to-end tunnel response..."
i=0
while [ "$i" -lt "$TIMEOUT_SECONDS" ]; do
  if curl -fsS -H "Host: $TEST_HOST" "http://127.0.0.1:$HOST_PORT/" >/tmp/plextunnel-e2e-response.txt 2>/dev/null; then
    echo "E2E check passed"
    echo "Response: $(cat /tmp/plextunnel-e2e-response.txt)"
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done

echo "E2E check failed after ${TIMEOUT_SECONDS}s"
docker compose -f "$STACK_FILE" logs --tail=200
exit 1
