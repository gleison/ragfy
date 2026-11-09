#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET_DIR="${RAGFY_BIN_DIR:-$HOME/.local/bin}"
TARGET="$TARGET_DIR/ragfy"
TEMPORARY="$TARGET_DIR/.ragfy.new.$$"

mkdir -p "$TARGET_DIR"
trap 'rm -f "$TEMPORARY"' EXIT
install -m 0755 "$ROOT/dist/ragfy" "$TEMPORARY"
mv -f "$TEMPORARY" "$TARGET"
trap - EXIT

case "$(uname -s)" in
  Darwin)
    PLIST="$HOME/Library/LaunchAgents/com.ragfy.service.plist"
    if [[ -f "$PLIST" ]]; then
      launchctl unload "$PLIST" 2>/dev/null || true
      launchctl load "$PLIST"
    fi
    ;;
  Linux)
    systemctl --user try-restart ragfy.service 2>/dev/null || true
    ;;
esac

printf 'Installed %s atomically\n' "$TARGET"
