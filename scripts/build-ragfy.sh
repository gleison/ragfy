#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_TMP="${TMPDIR:-/tmp}/ragfy-go-llama"

if [[ ! -d "$BUILD_TMP/.git" ]]; then
  git clone --depth 1 --branch v0.0.11 --recurse-submodules \
    https://github.com/mutablelogic/go-llama.git "$BUILD_TMP"
fi

cd "$BUILD_TMP"
build_pinned_llama_cpp_revision() {
  git submodule update --init --recursive
  make -o submodule -o go-tidy wrapper \
    CMAKE_FLAGS="-DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF"
}

build_pinned_llama_cpp_revision

mkdir -p "$ROOT/dist"
PKG_CONFIG_PATH="$BUILD_TMP/build/install/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  go build -C "$ROOT" -o "$ROOT/dist/ragfy" ./cmd/ragfy
printf 'Built %s\n' "$ROOT/dist/ragfy"
