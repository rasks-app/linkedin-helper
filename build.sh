#!/bin/sh
# Build the helper as one universal binary (Apple silicon and Intel), as the Rasks Mac app ships it.
#   ./build.sh [out] [version]
# Go 1.27 runs on macOS 13 and later. Needs Go and Xcode's command line tools (cgo links the Security framework for the Keychain).
set -eu
cd "$(dirname "$0")"
out="${1:-bin/rasks-linkedin-helper}"
version="${2:-$(git describe --always --dirty 2>/dev/null || echo dev)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for arch in arm64 amd64; do
  CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" MACOSX_DEPLOYMENT_TARGET=13.0 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$tmp/$arch" .
done
mkdir -p "$(dirname "$out")"
lipo -create -output "$out" "$tmp/arm64" "$tmp/amd64"
lipo -archs "$out"
