#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LLAMA_VERSION="v0.0.11"
BUILD_TMP="${RAGFY_LLAMA_BUILD_DIR:-${TMPDIR:-/tmp}/ragfy-go-llama-${LLAMA_VERSION}}"
VERSION="${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty)}"
COMMIT="${COMMIT:-$(git -C "$ROOT" rev-parse --short HEAD)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

clone_llama() {
  git clone --depth 1 --branch "$LLAMA_VERSION" \
    https://github.com/mutablelogic/go-llama.git "$BUILD_TMP"
}

if [[ -e "$BUILD_TMP" ]]; then
  if ! git -C "$BUILD_TMP" rev-parse --verify HEAD >/dev/null 2>&1 ||
      [[ -n "$(git -C "$BUILD_TMP" rev-parse --show-prefix)" ]]; then
    printf 'Ignoring invalid go-llama checkout at %s\n' "$BUILD_TMP" >&2
    BUILD_TMP="$(mktemp -d "${TMPDIR:-/tmp}/ragfy-go-llama-${LLAMA_VERSION}.XXXXXX")"
    clone_llama
  elif ! pinned_commit="$(git -C "$BUILD_TMP" rev-parse --verify "refs/tags/$LLAMA_VERSION^{commit}" 2>/dev/null)" ||
      [[ "$(git -C "$BUILD_TMP" rev-parse HEAD)" != "$pinned_commit" ]]; then
    printf 'Expected a go-llama checkout at %s in %s\n' "$LLAMA_VERSION" "$BUILD_TMP" >&2
    exit 1
  fi
else
  clone_llama
fi

cd "$BUILD_TMP"
BUILD_TMP="$(pwd -P)"
build_pinned_llama_cpp_revision() {
  local cmake_flags="-DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF"
  local -a build_arguments=(-o submodule -o go-tidy)

  if [[ "${RAGFY_PORTABLE:-0}" == "1" ]]; then
    cmake_flags+=" -DGGML_NATIVE=OFF"
  fi
  if [[ -n "${BUILD_JOBS:-}" ]]; then
    build_arguments+=("BUILD_JOBS=$BUILD_JOBS")
  fi
  build_arguments+=("CMAKE_FLAGS=$cmake_flags" wrapper)

  git submodule update --init --recursive
  make "${build_arguments[@]}"
}

build_pinned_llama_cpp_revision

mkdir -p "$ROOT/dist"
PKG_CONFIG_PATH="$BUILD_TMP/build/install/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  go build -C "$ROOT" \
    -ldflags "-X ragfy/internal/buildinfo.Version=$VERSION -X ragfy/internal/buildinfo.Commit=$COMMIT -X ragfy/internal/buildinfo.Date=$BUILD_DATE" \
    -o "$ROOT/dist/ragfy" ./cmd/ragfy
printf 'Built %s\n' "$ROOT/dist/ragfy"
