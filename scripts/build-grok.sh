#!/usr/bin/env bash
# Build the open-source Grok Build CLI (xai-org/grok-build) at the revision
# pinned in third_party/grok-build/REVISION.
#
#   OUTPUT        path of the produced grok binary (required); its upstream
#                 version is written to OUTPUT.version
#   NOTICES_DIR   where to copy GROK_BUILD_LICENSE and
#                 GROK_BUILD_THIRD_PARTY_NOTICES (default: dirname OUTPUT)
#   TARGET        optional Rust target triple
#   WORK_DIR      checkout directory (default: .grok-build-src)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
: "${OUTPUT:?OUTPUT is required}"
revision="$(tr -d '[:space:]' < "$root/third_party/grok-build/REVISION")"
work="${WORK_DIR:-$root/.grok-build-src}"
notices="${NOTICES_DIR:-$(dirname "$OUTPUT")}"

if [[ ! -d "$work/.git" ]]; then
  git init -q "$work"
  git -C "$work" remote add origin https://github.com/xai-org/grok-build.git
fi
git -C "$work" fetch -q --depth 1 origin "$revision"
git -C "$work" checkout -q --force FETCH_HEAD
if [[ "$(git -C "$work" rev-parse HEAD)" != "$revision" ]]; then
  echo "grok-build checkout does not match $revision" >&2
  exit 1
fi

# The repository's bin/protoc needs DotSlash; build.rs falls back to $PROTOC.
if [[ -z "${PROTOC:-}" ]] && ! command -v dotslash >/dev/null 2>&1; then
  PROTOC="$(command -v protoc || true)"
  [[ -n "$PROTOC" ]] || { echo "protoc (or dotslash) is required" >&2; exit 1; }
  export PROTOC
fi

args=(build --locked --release -p xai-grok-pager-bin)
target_dir="$work/target/release"
if [[ -n "${TARGET:-}" ]]; then
  args+=(--target "$TARGET")
  # rust-toolchain.toml in the checkout selects the toolchain.
  if command -v rustup >/dev/null 2>&1; then
    (cd "$work" && rustup target add "$TARGET")
  fi
  target_dir="$work/target/$TARGET/release"
fi
(cd "$work" && cargo "${args[@]}")

binary="$target_dir/xai-grok-pager"
[[ -f "$binary.exe" ]] && binary="$binary.exe"
mkdir -p "$(dirname "$OUTPUT")" "$notices"
cp "$binary" "$OUTPUT"
chmod 0755 "$OUTPUT"
cp "$work/LICENSE" "$notices/GROK_BUILD_LICENSE"
cp "$work/THIRD-PARTY-NOTICES" "$notices/GROK_BUILD_THIRD_PARTY_NOTICES"
sed -n 's/^version *= *"\(.*\)"/\1/p' "$work/crates/codegen/xai-grok-version/Cargo.toml" | head -1 > "$OUTPUT.version"
[[ -s "$OUTPUT.version" ]] || { echo "cannot read Grok Build version" >&2; exit 1; }
echo "built Grok Build $(cat "$OUTPUT.version") ($revision) -> $OUTPUT"
