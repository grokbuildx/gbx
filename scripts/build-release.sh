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
# GROK_BINARY: an open-source Grok Build from scripts/build-grok.sh for this
# GOOS/GOARCH. It is compiled into gbx; without it gbx uses a system install.
root="$(cd "$(dirname "$0")/.." && pwd)"
TAGS=""
bundle="$root/internal/grokbundle/grok.gz"
rm -f "$bundle"
if [[ -n "${GROK_BINARY:-}" ]]; then
  revision="$(tr -d '[:space:]' < "$root/third_party/grok-build/REVISION")"
  grok_version="$(tr -d '[:space:]' < "$GROK_BINARY.version")"
  gzip -9 -n -c "$GROK_BINARY" > "$bundle"
  trap 'rm -f "$bundle"' EXIT
  TAGS="grokbundle"
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/grokbundle.Version=$grok_version"
  LDFLAGS="$LDFLAGS -X github.com/grokbuildx/gbx/internal/grokbundle.Revision=$revision"
fi
mkdir -p "$(dirname "$OUTPUT")"
OUTPUT="$(cd "$(dirname "$OUTPUT")" && pwd)/$(basename "$OUTPUT")"
(cd "$root" && go build -trimpath -tags="$TAGS" -ldflags="$LDFLAGS" -o "$OUTPUT" .)
