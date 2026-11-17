# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Common Development Commands

Go 1.26 (see `go.mod`). CI (`.github/workflows/ci.yml`) is the source of truth:

```bash
gofmt -l .                 # must be empty
go test -race ./...
go vet ./...
go build ./...
```

Local / targeted:

```bash
go build -o /tmp/gbx-verify .
go test ./cmd -run TestRunAuthGrokWithoutAliasDerivesName
go test ./internal/oauthproxy ./internal/codex ./cmd
```

Release binary (matches CI): `scripts/build-release.sh` with `GOOS`/`GOARCH`/`OUTPUT`/`VERSION`; uses `-trimpath -ldflags="-s -w -X github.com/grokbuildx/gbx/cmd.Version=..."`. `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` inject the Google Drive Desktop client (required by the release workflow; dev builds use `GBX_GOOGLE_OAUTH_CLIENT_ID/_SECRET`).

Pinned Grok Build: `scripts/build-grok.sh` (`OUTPUT`, optional `TARGET`) compiles the open-source xai-org/grok-build commit in `third_party/grok-build/REVISION` and writes `OUTPUT.gz` plus its `.sha256` and `.version`. `scripts/build-release.sh` with `GROK_VERSION`/`GROK_SHA256` stamps that pin into `internal/grokbundle`; gbx downloads `grok-build-<commit>` release assets on first launch and only again when `REVISION` changes. Plain `go build` pins nothing and uses a system `grok`. Never pin a closed-source upstream binary.

### Do not run the `gbx` binary as a test harness

Unknown first args are **not** errors: `cmd/root.go` forwards them to Grok Build and can start a billed session (`gbx --resume`, `gbx -p "..."`, a quoted typo like `./gbx "provider --help"`). Prefer `go test` and `--help` with unquoted subcommand args.

Bare `gbx lang` is interactive; use `gbx lang zh` / `gbx lang en`. Do not run `set` / `use` / `oauth` / `cloud` / `provider` (mutating subcommands) against the developer's real `~/.gbx/` unless asked.

`~/.gbx/config.yaml` stores plaintext API keys — do not dump it.

## Architecture and subsystem instructions

GBX launches Grok Build and connects Codex to API/OAuth providers. Shared runtimes serve Responses and Chat Completions on loopback.

Before editing a subsystem, read its relevant section in [docs/development-architecture.md](docs/development-architecture.md). Use `rg -n '^##|^###' docs/development-architecture.md` to locate it, then read only the relevant section:

- Standalone proxy and Codex: **High-Level Architecture**.
- Provider converters, OAuth, retries and model discovery: **Protocol ownership** and `internal/oauthproxy/doc.go`.
- Configuration, environment/settings precedence and TUI: **Config, launch, TUI**.
- Encrypted cloud state and Google OAuth: **Cloud sync**.

Keep these invariants across changes:

- All config writes use `config.Update(fn)` (cmd helpers: `updateProvider` / `replaceProvider`); never hand-build YAML or mutate persisted providers while preparing a session.
- Bind runtime/control services to `127.0.0.1`; use per-session credentials and never persist them into providers. OAuth reads only its bound credential. Preserve the separate native/GBX Codex routing allow-list and credential boundary.
- Test proxy lifecycle and inference with isolated profiles, local fake upstreams and fake credentials.
- Codex GBX models follow the provider context setting: Default 200K / marked `[1m]` 1M, Balanced 500K / 800K, with an 85% compact threshold. Native Responses routes pass through. Converted models adapt native client `tool_search` to function calls and load definitions from `tool_search_output`; Codex owns search, execution and permissions.
- User-facing Chinese belongs in `cmd/` through `locale.T`; `internal/` has no Chinese literals except `internal/locale`.
- Provider management lives under `gbx provider` (with root shortcuts `gbx set`, `gbx ls`, `gbx use`, and `gbx preview`), cloud sync under `gbx cloud`; preserve the existing public command layout. Unknown root arguments launch Grok Build, so use isolated tests rather than ad hoc binary invocations.

When a subsystem contract changes, update the matching reference section. Keep implementation detail in the reference instead of growing these startup instructions.
