#!/usr/bin/env sh
set -eu

STACK_FILE="${STACK_FILE:-docker-compose.debug.yml}"
HOST_PORT="${HOST_PORT:-18080}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-45}"
TEST_HOST="${TEST_HOST:-myserver.local.test}"
SERVER_CONTEXT="${PLEXTUNNEL_SERVER_CONTEXT:-/tmp/plex-tunnel-server}"
SERVER_REPO_URL="${PLEXTUNNEL_SERVER_REPO_URL:-git@github.com:antoinecorbel7/plex-tunnel-server.git}"
SERVER_REF="${PLEXTUNNEL_SERVER_REF:-main}"

cleanup() {
  rm -f /tmp/plextunnel-e2e-response.txt
  if [ "${KEEP_STACK:-0}" = "1" ]; then
    return
  fi
  docker compose -f "$STACK_FILE" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

prepare_server_source() {
  if [ -d "$SERVER_CONTEXT/.git" ]; then
    echo "Updating server source at $SERVER_CONTEXT (ref: $SERVER_REF)..."
    git -C "$SERVER_CONTEXT" fetch --all --prune
    git -C "$SERVER_CONTEXT" checkout "$SERVER_REF"
    git -C "$SERVER_CONTEXT" pull --ff-only origin "$SERVER_REF"
    return 0
  fi

  if [ -e "$SERVER_CONTEXT" ] && [ ! -d "$SERVER_CONTEXT/.git" ]; then
    echo "Server context exists but is not a git repository: $SERVER_CONTEXT"
    return 1
  fi

  echo "Cloning server source into $SERVER_CONTEXT (ref: $SERVER_REF)..."
  git clone --depth 1 --branch "$SERVER_REF" "$SERVER_REPO_URL" "$SERVER_CONTEXT"
}

if [ -z "${PLEXTUNNEL_SERVER_IMAGE:-}" ]; then
  if [ -d "$SERVER_CONTEXT/.git" ] || [ ! -f "$SERVER_CONTEXT/Dockerfile.server" ]; then
    prepare_server_source || {
      echo "Unable to prepare server source from $SERVER_REPO_URL"
      exit 1
    }
  fi

  if [ -f "$SERVER_CONTEXT/Dockerfile.server" ]; then
    PLEXTUNNEL_SERVER_IMAGE="plextunnel/server:debug-local"
    export PLEXTUNNEL_SERVER_IMAGE
    echo "Building local server image from $SERVER_CONTEXT..."
    docker build -f "$SERVER_CONTEXT/Dockerfile.server" -t "$PLEXTUNNEL_SERVER_IMAGE" "$SERVER_CONTEXT"
  else
    echo "No server Dockerfile at $SERVER_CONTEXT and PLEXTUNNEL_SERVER_IMAGE is not set."
    echo "Set PLEXTUNNEL_SERVER_IMAGE to a pullable image, or configure PLEXTUNNEL_SERVER_CONTEXT/PLEXTUNNEL_SERVER_REPO_URL."
    exit 1
  fi
fi

echo "Starting local debug stack..."
docker compose -f "$STACK_FILE" up -d --build

echo "Waiting for end-to-end tunnel response..."
i=0
while [ "$i" -lt "$TIMEOUT_SECONDS" ]; do
  if curl -fsS -H "Host: $TEST_HOST" "http://127.0.0.1:$HOST_PORT/" >/tmp/plextunnel-e2e-response.txt 2>/dev/null; then
    if grep -q "mock-plex-ok" /tmp/plextunnel-e2e-response.txt; then
      echo "E2E check passed"
      echo "Response: $(cat /tmp/plextunnel-e2e-response.txt)"
      exit 0
    fi
    echo "Got HTTP 200 but unexpected body: $(cat /tmp/plextunnel-e2e-response.txt)"
  fi
  i=$((i + 1))
  sleep 1
done

echo "E2E check failed after ${TIMEOUT_SECONDS}s"
docker compose -f "$STACK_FILE" logs --tail=200
exit 1
