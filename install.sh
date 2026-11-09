#!/bin/sh
set -eu

REPOSITORY="${RAGFY_REPOSITORY:-gleison/ragfy}"
VERSION="latest"
PREFIX="/usr/local"
USER_INSTALL=0
RUN_SETUP=1

usage() {
  echo "usage: install.sh [--user] [--prefix <path>] [--version <version>] [--no-setup]"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --user)
      USER_INSTALL=1
      PREFIX="${HOME}/.local"
      shift
      ;;
    --prefix)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      PREFIX="$2"
      USER_INSTALL=0
      shift 2
      ;;
    --version)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      VERSION="$2"
      shift 2
      ;;
    --no-setup)
      RUN_SETUP=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

case "$(uname -s)" in
  Darwin) OS="darwin" ;;
  Linux) OS="linux" ;;
  *) echo "Ragfy installer does not support this operating system yet." >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64|amd64) ARCH="amd64" ;;
  *) echo "Ragfy installer does not support architecture $(uname -m)." >&2; exit 1 ;;
esac

ARCHIVE="ragfy-${OS}-${ARCH}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  RELEASE_URL="https://github.com/${REPOSITORY}/releases/latest/download"
else
  case "$VERSION" in v*) TAG="$VERSION" ;; *) TAG="v$VERSION" ;; esac
  RELEASE_URL="https://github.com/${REPOSITORY}/releases/download/${TAG}"
fi

TEMPORARY_DIRECTORY="$(mktemp -d 2>/dev/null || mktemp -d -t ragfy)"
trap 'rm -rf "$TEMPORARY_DIRECTORY"' EXIT INT TERM

echo "Downloading ${ARCHIVE}…"
curl -fL --retry 3 --proto '=https' --tlsv1.2 -o "$TEMPORARY_DIRECTORY/$ARCHIVE" "$RELEASE_URL/$ARCHIVE"
curl -fL --retry 3 --proto '=https' --tlsv1.2 -o "$TEMPORARY_DIRECTORY/checksums.txt" "$RELEASE_URL/checksums.txt"

EXPECTED="$(awk -v archive="$ARCHIVE" '$2 == archive || $2 == "*" archive { print $1; exit }' "$TEMPORARY_DIRECTORY/checksums.txt")"
[ -n "$EXPECTED" ] || { echo "Checksum for $ARCHIVE was not published." >&2; exit 1; }
if command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "$TEMPORARY_DIRECTORY/$ARCHIVE" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "$TEMPORARY_DIRECTORY/$ARCHIVE" | awk '{print $1}')"
else
  echo "A SHA-256 utility is required (shasum or sha256sum)." >&2
  exit 1
fi
[ "$EXPECTED" = "$ACTUAL" ] || { echo "Checksum verification failed." >&2; exit 1; }
echo "Checksum verified."

tar -xzf "$TEMPORARY_DIRECTORY/$ARCHIVE" -C "$TEMPORARY_DIRECTORY"
[ -x "$TEMPORARY_DIRECTORY/ragfy" ] || { echo "Release archive does not contain the ragfy executable." >&2; exit 1; }

TARGET_DIRECTORY="$PREFIX/bin"
TARGET="$TARGET_DIRECTORY/ragfy"
TEMPORARY_TARGET="$TARGET_DIRECTORY/.ragfy.new.$$"

install_binary() {
  install -d "$TARGET_DIRECTORY"
  install -m 0755 "$TEMPORARY_DIRECTORY/ragfy" "$TEMPORARY_TARGET"
  mv -f "$TEMPORARY_TARGET" "$TARGET"
}

if [ -d "$TARGET_DIRECTORY" ] && [ -w "$TARGET_DIRECTORY" ]; then
  install_binary
elif [ "$USER_INSTALL" -eq 1 ]; then
  install_binary
elif command -v sudo >/dev/null 2>&1; then
  sudo install -d "$TARGET_DIRECTORY"
  sudo install -m 0755 "$TEMPORARY_DIRECTORY/ragfy" "$TEMPORARY_TARGET"
  sudo mv -f "$TEMPORARY_TARGET" "$TARGET"
else
  echo "Cannot write to $TARGET_DIRECTORY. Re-run with --user or --prefix." >&2
  exit 1
fi

echo "Installed Ragfy at $TARGET"

case "$OS" in
  darwin)
    PLIST="$HOME/Library/LaunchAgents/com.ragfy.service.plist"
    if [ -f "$PLIST" ] && grep -Fq "<string>$TARGET</string>" "$PLIST"; then
      launchctl unload "$PLIST" 2>/dev/null || true
      launchctl load "$PLIST"
    fi
    ;;
  linux)
    systemctl --user try-restart ragfy.service 2>/dev/null || true
    ;;
esac

if [ "$RUN_SETUP" -eq 0 ]; then
  echo "Update complete."
elif [ -r /dev/tty ] && [ -w /dev/tty ]; then
  printf "Run the setup wizard now? [Y/n] " > /dev/tty
  read answer < /dev/tty
  case "$answer" in
    n|N|no|NO) echo "Run '$TARGET setup' when you are ready." ;;
    *) exec "$TARGET" setup < /dev/tty > /dev/tty 2> /dev/tty ;;
  esac
else
  echo "Run '$TARGET setup' to finish configuration."
fi
