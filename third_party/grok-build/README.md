# Bundled Grok Build

gbx release binaries for macOS and Linux embed a `grok` binary compiled from
the open-source [xai-org/grok-build](https://github.com/xai-org/grok-build)
repository (Apache License 2.0, see [LICENSE](LICENSE)). gbx does not modify
its source.

- `REVISION` pins the exact upstream commit that releases build.
- `scripts/build-grok.sh` fetches that commit, runs
  `cargo build --locked --release -p xai-grok-pager-bin`, and records the
  upstream version from `crates/codegen/xai-grok-version`.
- `scripts/build-release.sh` with `GROK_BINARY=<path>` gzips that binary into
  `internal/grokbundle/grok.gz` and builds gbx with `-tags grokbundle`.
- At runtime gbx unpacks it to `~/.gbx/grok/rev-<commit>/grok`, launches it
  with `GROK_DISABLE_AUTOUPDATER=1`, and reports it in `gbx version`.
- Unchanged `REVISION` means unchanged Grok Build: CI reuses the cached
  binary instead of rebuilding, and a gbx upgrade keeps the copy already
  unpacked on disk. Only a new commit is built and unpacked again.
- Release packages ship the upstream `LICENSE` as `GROK_BUILD_LICENSE` and the
  upstream `THIRD-PARTY-NOTICES` as `GROK_BUILD_THIRD_PARTY_NOTICES`.

To move to a newer upstream commit, replace the SHA in `REVISION` and cut a
release. To run a different Grok Build binary, set `GBX_GROK_BIN`.
