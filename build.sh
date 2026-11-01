#!/usr/bin/env bash
# Cross-compiles sclogsync.exe for Windows into dist/ and writes dist/SHA256SUMS.
#   VERSION=1.0.0 ./build.sh
#   VERSION=1.0.0 SERVER=https://sclog.example.com ./build.sh   # also bakes in a default server address
set -euo pipefail
cd "$(dirname "$0")"
VERSION="${VERSION:-dev}"
LDFLAGS="-s -w -X main.version=$VERSION"
if [ -n "${SERVER:-}" ]; then
  LDFLAGS="$LDFLAGS -X main.defaultServer=$SERVER"
fi
mkdir -p dist
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o dist/sclogsync.exe ./cmd/sclogsync
cd dist
if command -v sha256sum >/dev/null; then sha256sum sclogsync.exe > SHA256SUMS; else shasum -a 256 sclogsync.exe > SHA256SUMS; fi
echo "built dist/sclogsync.exe (version $VERSION)"
cat SHA256SUMS
