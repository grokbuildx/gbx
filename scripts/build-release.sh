#!/usr/bin/env bash
set -euo pipefail

# One target per runner: avoid competing cross-compilers. Keep release flags
# identical locally and in CI; preserve the existing raw-binary install flow.
: "${GOOS:?GOOS must be set}"
: "${GOARCH:?GOARCH must be set}"
: "${OUTPUT:?OUTPUT must be set}"
VERSION="${VERSION:-dev}"
case "$GOOS/$GOARCH" in
  darwin/amd64|darwin/arm64|linux/amd64|linux/arm64|windows/amd64|windows/arm64) ;;
  *) echo "Unsupported release target: $GOOS/$GOARCH" >&2; exit 1 ;;
esac

export CGO_ENABLED=0
LDFLAGS="-s -w -X github.com/grokbuildx/gbx/cmd.Version=$VERSION"
if [[ -n "${GOOGLE_OAUTH_CLIENT_ID:-}" ]]; then
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/cloudsync.googleOAuthClientID=$GOOGLE_OAUTH_CLIENT_ID"
fi
if [[ -n "${GOOGLE_OAUTH_CLIENT_SECRET:-}" ]]; then
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/cloudsync.googleOAuthClientSecret=$GOOGLE_OAUTH_CLIENT_SECRET"
fi
# GROK_VERSION / GROK_SHA256: the open-source Grok Build this gbx pins, as
# published in the grok-build-<commit> release (see scripts/build-grok.sh).
# gbx downloads and verifies it on first use; without them gbx uses a system
# Grok Build.
root="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -n "${GROK_SHA256:-}" ]]; then
  [[ "$GROK_SHA256" =~ ^[0-9a-f]{64}$ ]] || { echo "GROK_SHA256 must be a sha256 hex digest" >&2; exit 1; }
  : "${GROK_VERSION:?GROK_VERSION is required with GROK_SHA256}"
  revision="$(tr -d '[:space:]' < "$root/third_party/grok-build/REVISION")"
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/grokbundle.Version=$GROK_VERSION"
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/grokbundle.Revision=$revision"
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/grokbundle.SHA256=$GROK_SHA256"
fi
mkdir -p "$(dirname "$OUTPUT")"
OUTPUT="$(cd "$(dirname "$OUTPUT")" && pwd)/$(basename "$OUTPUT")"
(cd "$root" && go build -trimpath -ldflags="$LDFLAGS" -o "$OUTPUT" .)
