# Pinned Grok Build

gbx release binaries for macOS and Linux pin a `grok` binary compiled from the
open-source [xai-org/grok-build](https://github.com/xai-org/grok-build)
repository (Apache License 2.0, see [LICENSE](LICENSE)). gbx does not modify
its source.

- `REVISION` pins the exact upstream commit.
- `scripts/build-grok.sh` fetches that commit, runs
  `cargo build --locked --release -p xai-grok-pager-bin`, and writes
  `OUTPUT.gz`, its `.sha256`, and the upstream version.
- The release workflow publishes each commit once, as the GitHub release
  `grok-build-<commit>` (archives, checksums, `GROK_BUILD_LICENSE`,
  `GROK_BUILD_THIRD_PARTY_NOTICES`). Later releases with the same `REVISION`
  find it there and skip the build.
- `scripts/build-release.sh` with `GROK_VERSION` and `GROK_SHA256` stamps the
  pin into gbx (`internal/grokbundle`).
- At runtime gbx downloads `grok-<os>-<arch>.gz` from that release on first
  launch, verifies the stamped SHA-256, unpacks it to
  `~/.gbx/grok/rev-<commit>/grok`, and launches it with
  `GROK_DISABLE_AUTOUPDATER=1`. Upgrading gbx downloads nothing unless
  `REVISION` changed.

To move to a newer upstream commit, replace the SHA in `REVISION` and cut a
release. To run a different Grok Build binary, set `GBX_GROK_BIN`.
