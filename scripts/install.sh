#!/usr/bin/env sh
set -eu

REPO_URL="https://github.com/antoinecorbel7/plex-tunnel"
VERSION="${PLEXTUNNEL_VERSION:-latest}"

if [ "$VERSION" = "latest" ]; then
  echo "Set PLEXTUNNEL_VERSION to a release tag before using this installer."
  exit 1
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

BIN="plextunnel-agent-${OS}-${ARCH}"
URL="$REPO_URL/releases/download/$VERSION/$BIN"

curl -fsSL "$URL" -o plextunnel-agent
chmod +x plextunnel-agent
sudo mv plextunnel-agent /usr/local/bin/plextunnel-agent

echo "Installed plextunnel-agent from $URL"
