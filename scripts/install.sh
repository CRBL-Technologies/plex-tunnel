#!/usr/bin/env sh
set -eu

REPO_URL="https://github.com/CRBL-Technologies/plex-tunnel"
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

BIN="plextunnel-client-${OS}-${ARCH}"
URL="$REPO_URL/releases/download/$VERSION/$BIN"
CHECKSUM_URL="${URL}.sha256"
CHECKSUM_FILE="${BIN}.sha256"

if ! curl -fsSL "$URL" -o "$BIN"; then
  echo "Failed to download $URL"
  rm -f "$BIN" "$CHECKSUM_FILE"
  exit 1
fi

if ! curl -fsSL "$CHECKSUM_URL" -o "$CHECKSUM_FILE"; then
  echo "Failed to download $CHECKSUM_URL"
  rm -f "$BIN" "$CHECKSUM_FILE"
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  if ! sha256sum -c "$CHECKSUM_FILE"; then
    echo "Checksum verification failed for $BIN"
    rm -f "$BIN" "$CHECKSUM_FILE"
    exit 1
  fi
elif command -v shasum >/dev/null 2>&1; then
  if ! shasum -a 256 -c "$CHECKSUM_FILE"; then
    echo "Checksum verification failed for $BIN"
    rm -f "$BIN" "$CHECKSUM_FILE"
    exit 1
  fi
else
  echo "Neither sha256sum nor shasum is available to verify the download."
  rm -f "$BIN" "$CHECKSUM_FILE"
  exit 1
fi

chmod +x "$BIN"
sudo mv "$BIN" /usr/local/bin/plextunnel-client
rm -f "$CHECKSUM_FILE"

echo "Installed plextunnel-client from $URL"
