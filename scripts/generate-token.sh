#!/usr/bin/env sh
set -eu

if command -v uuidgen >/dev/null 2>&1; then
  uuidgen | tr '[:upper:]' '[:lower:]'
else
  cat /proc/sys/kernel/random/uuid 2>/dev/null || {
    echo "uuidgen not found and /proc UUID unavailable"
    exit 1
  }
fi
