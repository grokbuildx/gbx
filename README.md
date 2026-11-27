# gbx

**[中文](#中文) · [English](#english)**

---

## 中文

`gbx` 是围绕 [Grok Build](https://x.ai/cli) 的启动器与 provider 代理：为 Grok Build 和 Codex 接入 OpenAI 兼容网关，以及 GPT、Grok、Cursor 等订阅账号，并通过本地代理转换协议。

### 完全开源：你运行的 agent 就是你能读到的代码

**gbx 本身和它运行的编程 agent 都是开源的，没有任何闭源组件。**

| 组件 | 许可 | 源码 |
|---|---|---|
| gbx（启动器、代理、WebUI） | MIT | [grokbuildx/gbx](https://github.com/grokbuildx/gbx) |
| Grok Build agent | Apache-2.0 | [xai-org/grok-build](https://github.com/xai-org/grok-build)，固定在 [`third_party/grok-build/REVISION`](third_party/grok-build/REVISION) 记录的提交 |

为什么这很重要：Grok Build 的开源仓库自 2026-09-29 起停留在 **1.0.45**，而官方通过 `x.ai/cli` 分发、并会自动更新的二进制已经到了 1.0.88 —— 这些新版本**没有公开源码**，你无法审计一个能读写你代码、执行 shell 命令的 agent 到底做了什么。

gbx 的做法：

- **从源码编译**：每个固定提交只在公开的 GitHub Actions 中编译一次（macOS / Linux），发布为 Release `grok-build-<提交>`。同一提交、同一脚本（[`scripts/build-grok.sh`](scripts/build-grok.sh)），任何人都可以复现。
- **钉死并校验**：每个 gbx 版本都写死了它使用的 Grok Build 提交和压缩包 SHA-256。首次启动时下载到 `~/.gbx/grok/rev-<提交>/`，校验不通过就拒绝使用。
- **没变就不更新**：升级 gbx 时，只要固定的 Grok Build 提交没变，就不会重新下载或替换；只有提交变化时才下载新版本（约 70 MB）。
- **不会被悄悄替换**：gbx 启动它固定的 Grok Build 时关闭其自动更新，闭源的上游版本不会覆盖它。
- **可以随时核对**：`gbx version` 显示 gbx 版本、固定的 Grok Build 版本与源码提交以及是否已下载；`gbx doctor` 显示当前实际运行的是哪个 Grok Build。

```text
$ gbx version
gbx version vX.Y.Z
Grok Build 1.0.45 (open source, built from xai-org/grok-build@2bdd1d6a6369)
  source: https://github.com/xai-org/grok-build/tree/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8
  installed: /Users/you/.gbx/grok/rev-2bdd1d6a6369/grok
```

### 安装

```bash
npm install -g @grokbuildx/gbx                 # 推荐，需 Node.js >= 22
go install github.com/grokbuildx/gbx@latest  # 或 Go
```

也可从 [Releases](https://github.com/grokbuildx/gbx/releases) 下载对应平台的 `.gz`（macOS / Linux / Windows，x64 与 arm64），解压后放入 `PATH`，`SHA256SUMS` 校验的是 `.gz`。npm 安装请勿加 `--omit=optional`，否则缺少平台二进制。

```bash
gbx version
gbx doctor     # 只读体检；加 --probe 才会逐个测模型（计费）
```

macOS / Linux 的 npm 包与 Release 二进制固定了一个开源 Grok Build，无需另装：首次启动时自动下载并校验，之后只有固定提交变化才会再下载。Windows 构建与 `go install` 不固定版本，会依次使用 `GBX_GROK_BIN`、`PATH` 中的 `grok`、`~/.grok/bin/grok`；设置 `GBX_GROK_BIN` 也可以覆盖固定版本。Codex 接入使用你现有的 Codex 客户端。

### 快速开始

```bash
# A. 订阅账号（示例登录 GPT 账号并设别名）
gbx oauth gpt work

# B. API Key / 第三方网关：单页 TUI，填 Endpoint 与 Key → Auto Configure → Save & Activate
gbx set deepseek

gbx            # 用当前 provider 启动 Grok Build
gbx --provider deepseek -p "你好"   # 仅本次使用 deepseek，不改变默认选择
```

新安装默认使用 Grok Build 自身的登录。通过 `gbx oauth`、`gbx set` 中的 Save & Activate 或 `gbx use <name>` 选择 provider，运行 `gbx use default` 可切回 Grok Build 自身的登录。

额外参数会透传给 Grok Build，可能启动计费会话。`--provider` 放在 Grok 参数前，亦可写成 `--provider=deepseek`。用 `gbx grok doctor` 调用 Grok 自己的同名子命令。

### 常用命令

| 命令 | 作用 |
|---|---|
| `gbx set [name]` | 新增 / 修改 provider（TUI）；`-m <model>` 直接设置默认模型 |
| `gbx ls [-a]` / `gbx use <name>` / `gbx preview [name]` | 列出 / 切换 / 预览 Grok 配置；`gbx use default` 使用 Grok Build 自身登录 |
| `gbx --provider <name> [Grok 参数]` | 本次启动指定 provider，不改变默认选择 |
| `gbx oauth <name> [alias]` / `gbx oauth prune` | 订阅登录 / 清理未使用凭据 |
| `gbx provider cp\|mv\|rm\|models\|preview` | provider 管理与模型池探测 |
| `gbx codex run` / `start` / `status` / `stop` | Codex 接入：启动代理并打开 ChatGPT / Codex（关客户端自动停） |
| `gbx serve run` / `start` / `status` / `stop` | 在本地浏览器中使用 Grok Build（默认关浏览器自动退出，或 `start` 后台常驻） |
| `gbx mcp-server [-w <dir>]` | 通过 MCP `stdio` 向外部 AI（如 Gemini App）暴露本地编码工具（无需配置模型） |
| `gbx cloud login\|push\|pull\|status\|remote\|tag\|logout` | 加密云同步（iCloud / Google Drive） |
| `gbx bypass [on\|off]` | 启动时附加 `--always-approve`，仅在可信环境使用 |
| `gbx log [on\|off] [--level debug]` | 会话日志（默认关闭，写入 `~/.gbx/logs/`） |
| `gbx lang [zh\|zh-TW\|en]` | 界面语言（`GBX_LANG` > 配置 > 系统） |
| `gbx doctor` / `update` / `version` / `completion` | 诊断 / 升级 / 版本 / 补全 |

每个子命令都支持 `--help`。

### 浏览器会话

```bash
gbx serve                        # 等同 gbx serve run：打开浏览器，关页面自动退出
gbx serve run -p deepseek -w .   # 指定 provider 与项目目录，关闭所有页面自动停止
gbx serve start                  # 后台常驻启动（关闭浏览器不退出）
gbx serve status / gbx serve stop
```

在浏览器里新建会话、流式查看回复、停止回合，并允许或拒绝 Grok 发出的工具审批。页面还提供项目文件与 Git 面板、网页终端，以及 Provider、OAuth 登录和云同步设置。默认模式（`gbx serve` / `gbx serve run`）启动后会立即打开浏览器，并在关闭所有已打开页面后自动退出（带短暂刷新宽限）；`gbx serve start` 在后台常驻运行（加 `-o` 可同时打开浏览器），关闭浏览器不会停止服务，直到执行 `gbx serve stop`。`-p` 指定新会话默认 Provider，`-w`/`--workspace` 可重复指定项目目录（默认当前目录），`--workspace-root` 将其直接子目录都作为项目，`-m`/`--model` 与 `--effort` 设置默认模型与推理强度。服务仅监听本机 `127.0.0.1`，访问需使用终端打印的会话链接。`--no-open` 只打印链接，`--port` 指定端口，`--socket` 改用本机 Unix socket，`--no-shell` 关闭网页终端。

会话历史保存在 `~/.gbx/serve/sessions/`，重启后仍在。需要安装支持 ACP v1 会话加载的 Grok Build。

### Codex 模型接入

```bash
gbx codex start        # 使用 gbx use 当前选择的配置
gbx codex -p deepseek  # 或显式指定本次代理的 provider
# 重启 Codex 后选择 GBX 模型
gbx codex stop        # 恢复 Codex 原配置
```

- 已绑定 provider 的配置变化会自动刷新模型目录和路由；新请求使用新 Runtime，正在进行的输出继续完成。客户端是否立即显示新模型由 Codex 决定，本地 JSON 目录不承诺进程内重载，必要时重启 Codex。
- 多提供商接入：`gbx codex start deepseek --with-provider gpt --with-provider grok`。每个模型绑定各自账号，后续启动沿用这些额外绑定；`--with-provider ''` 清空。`gbx codex start --refresh` 主动重新获取模型。
- 默认使用本地目录。独立模式可试用 `gbx codex start --independent --catalog remote`；`--catalog local` 回退。Shared 模式不能把内置 OpenAI 的远程目录 URL 指向 localhost，因此继续使用合并 JSON。远程目录已验证获取与后台更新，但不承诺 Desktop 选择器即时刷新。所有模式都区分 GBX 路由/目录更新与客户端列表更新。
- OpenAI 额度耗尽后要使用其他提供商，可运行 `gbx codex start --independent` 并重启 Codex。独立模式使用 GBX 自己的 provider 和会话凭据，列表只显示 GBX 模型；`gbx codex start --shared` 切回原生与 GBX 共用的列表。代理运行期间，后续启动、菜单切换沿用当前模式；停止后不保存模式，下次不带模式参数启动默认使用 shared。
- Codex 会话绑定创建时的 model provider。独立模式会话使用 `gbx_proxy`，切回 shared 或停止代理后，该定义会移除，旧会话可能提示 provider 未配置或 `Model unavailable`。继续原会话需恢复 `gbx codex start --independent` 并重启 Codex；跨模式继续相同历史请分叉新会话，不要修改会话记录。参见 [OpenAI Docs](https://learn.chatgpt.com/docs/codex-sdk#resume-a-thread-without-loading-its-history)。
- `gbx use` 只影响 `gbx` 启动，不会切换运行中的代理。
- 先打开 Codex 一次建立配置；启动代理会更新 `~/.codex/config.toml`（支持 `CODEX_HOME`）。重启 Codex 后，同一模型选择器中同时显示原生模型和当前配置的第三方模型。列表只显示模型名称；与原生模型同名时加简短的 `GBX` 标记。WorkBuddy 的同名内部版本合并为一项。
- Codex 的 GBX 模型沿用“上下文与压缩”设置：Default 为 200K，模型池中带 `[1m]` 的模型为 1M；Balanced 为所选的 500K / 800K，均在 85% 自动压缩。转换协议的模型由 GBX 生成可回放摘要，原生 Responses 模型保留上游压缩。
- 转换协议的模型支持 Codex 客户端工具搜索：GBX 将搜索转换为普通函数调用，由 Codex 检索本地工具并返回选中的定义，避免一次发送全部延迟加载工具。实际工具仍由 Codex 执行并检查权限；模型需要能正确使用搜索函数和返回的工具定义。
- 支持图片的模型按上游声明开放截图读取；转换模型沿用本机原生目录提供的客户端工具模式，computer use 仍由 Codex 客户端执行并检查权限。shared 模式内，常规对话可在转换模型与原生 GPT 之间切换；GPT 的原生加密压缩内容无法解密；转换模型会校验同一会话的本地记录与压缩项，再恢复有边界的可见历史摘录，并提示旧细节可能缺失。缺少匹配记录时明确报错。
- 转换模型的 effort 档位来自 models.dev 元数据缓存，按模型匹配，不再统一为低、中、高。OAuth 登录和 `gbx set` 保存时按需刷新；也可运行 `gbx set --refresh-models` 单独更新。网络失败使用 `~/.gbx/cache/modelsdev.json`；未匹配或尚无缓存时保留兼容设置。运行中的代理会刷新元数据，本地 JSON 模式重启 Codex 加载新目录。原生 Responses 模型保留自身元数据。
- Codex 保留原生 OpenAI Provider 与登录。原生请求继续走原有 ChatGPT / OpenAI 服务，只有独立白名单中的 `gbx/<provider>/<model>` 发往 GBX；转发到 GBX 时移除 Codex 凭据，WebSocket 用 426 回退 HTTP Responses。
- 原生模型信息来自现有目录或 `models_cache.json`，必要时读取已安装 Codex 的 `debug models --bundled`，不写死原生模型名称。缺少缓存时需安装较新的 Codex CLI，或让 GBX 能找到桌面 App 自带的 Codex。原有自定义 Provider 或默认 profile 的目录覆盖会明确报错，避免改变其路由。
- macOS 状态栏菜单可切换配置或停止 Codex 接入。切换成功后同步更新 GBX 的当前选择；Codex 保持运行，需要手动重启以加载新目录。新配置启动失败会恢复原代理并提示错误。代理退出或异常停止后图标消失。无桌面环境可用 `GBX_PROXY_NO_MENUBAR=1` 关闭图标。
- 代理仅管理 Codex 配置，普通 Grok Build 启动独立运行。停止或异常恢复时只恢复仍等于代理写入值的 Codex 字段，保留后续用户修改；升级旧版共用代理时会先恢复其遗留的 Claude 设置。启动、切换或停止后，请重启 Codex 以重新载入目录。
- 异常退出后，下次运行任意 `gbx` 命令会按 `~/.gbx/proxy/state.json` 自动恢复；启动诊断见 `~/.gbx/proxy/proxy.log`。

### 协议与接入

GBX 在 `127.0.0.1` 提供 Responses 和 Chat Completions。每个代理使用随机会话凭据。Responses 是转换各后端时使用的协议。

| 来源 | 上游协议 |
|---|---|
| OpenAI Chat 网关 / Kimi / WorkBuddy | Chat Completions |
| OpenAI Responses 网关 / GPT / Grok | Responses，GBX 使用对应后端的请求标识 |
| models.dev 混合网关 | 按模型选择：Responses、Chat Completions 或 Anthropic Messages（`@ai-sdk/anthropic` 模型，按 models.dev 的单模型地址发送） |
| Cursor | Connect 双向 Agent 流 |
| Kiro / Qoder | Amazon Q / COSY |
| AutoClaw | 受管 Chat Completions 代理 |

429 / 5xx 默认最多快速重试两次（500ms、1s）；上游提供更长的 `Retry-After` 时立即透传等待提示，不提前重试。401 由各后端刷新 OAuth 凭据，Qoder 刷新失败会保留原来的认证错误。Qoder / WorkBuddy 的认证 API、刷新和模型请求拒绝跨来源重定向；账号绑定到指定凭据文件，停用或变更类型后不继续刷新。

### 配置

- `~/.gbx/config.yaml` 保存 provider、当前选择、语言、权限绕过和日志设置。**含明文 API Key，勿提交或外传。**
- `~/.gbx/auth/*.json` 保存 OAuth 凭据，权限为 0600。
- API-key provider 的 `type` 可选 `openai`（Chat Completions）或 `openai_responses`。OAuth provider 由其后端决定。
- 每个 provider 可设置默认模型：`gbx set -m <model>`，或在 TUI 中设置「默认模型」。Grok Build 启动、Codex 与 `gbx serve` 优先使用它；未设置时取模型池中的第一个模型。
- `gbx preview`（或 `gbx provider preview`）显示传给 Grok Build 的默认模型、辅助模型和 reasoning effort。
- Codex 上下文预设为 Default、Balanced 500K 和 Balanced 800K。


### 云同步

`gbx cloud login`（默认 Google Drive；macOS 也可 `gbx cloud login icloud`）之后 `push` / `pull`。没有浏览器的环境（SSH、Cloud Shell、无图形界面的 Linux）会自动改为打印授权链接：在任意设备的浏览器中打开并授权，随后浏览器跳转到一个打不开的 `127.0.0.1` 地址，把地址栏里的完整地址粘贴回终端即可；`--no-browser` / `--no-browser=false` 可强制选择。同步 `config.yaml` 与 `auth/*.json`，gzip 后用 AES-256-GCM 加密；Google Drive 仅申请 `drive.appdata` 权限。密钥随云端 profile 一起保存，新设备只需用同一账号 `gbx cloud login` 即可 `pull`，无需密码或配对；相应地，能登录该云账号（或持有其 gbx 授权）的人也能解密同步的 provider 凭据。冲突时 push / pull 会停止，不会静默覆盖。

### 开发

```bash
gofmt -l .            # 必须为空
go vet ./... && go test -race ./...
go build -o /tmp/gbx-verify .
node --test scripts/npm-packages.test.js
```

- 不要把 `gbx` 当测试工具直接跑：未知参数会启动计费的 Grok 会话；用 `go test` 与 `--help`，并设置临时 `HOME` / `USERPROFILE`。
- 架构与约定见 [AGENTS.md](AGENTS.md) 与 [docs/development-architecture.md](docs/development-architecture.md)；协议说明见 `internal/oauthproxy/doc.go`；npm 发布见 [docs/npm-release.md](docs/npm-release.md)；云同步设计见 [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md)。
- 推送 `v*` tag 触发 CI 构建 6 个平台二进制并发布到 GitHub Releases 与 npm。

### 许可

gbx 采用 MIT。Grok Build 采用 Apache-2.0（[`third_party/grok-build/LICENSE`](third_party/grok-build/LICENSE)），每个 `grok-build-<提交>` Release 都附带 `GROK_BUILD_LICENSE` 和 `GROK_BUILD_THIRD_PARTY_NOTICES`。其他第三方参考实现见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。gbx 是独立的开源项目，与 xAI 无隶属关系。

---

## English

`gbx` is a launcher and provider proxy built around [Grok Build](https://x.ai/cli). It connects Grok Build and Codex to OpenAI-compatible gateways and subscription accounts such as GPT, Grok, and Cursor, translating protocols through a local proxy.

### Fully open source: the agent you run is the code you can read

**gbx and the coding agent it runs are both open source. There is no closed-source component.**

| Component | License | Source |
|---|---|---|
| gbx (launcher, proxy, WebUI) | MIT | [grokbuildx/gbx](https://github.com/grokbuildx/gbx) |
| Grok Build agent | Apache-2.0 | [xai-org/grok-build](https://github.com/xai-org/grok-build), pinned to the commit in [`third_party/grok-build/REVISION`](third_party/grok-build/REVISION) |

Why it matters: the open-source Grok Build repository has stayed at **1.0.45** since 2026-09-29, while the official binary distributed through `x.ai/cli`, which updates itself, has reached 1.0.88. Those newer versions **have no published source**, so there is no way to audit what an agent that reads and writes your code and runs shell commands actually does.

What gbx does about it:

- **Built from source.** Each pinned commit is compiled once in public GitHub Actions (macOS / Linux) and published as the release `grok-build-<commit>`. Same commit, same script ([`scripts/build-grok.sh`](scripts/build-grok.sh)), reproducible by anyone.
- **Pinned and verified.** Every gbx release records the Grok Build commit it uses and the archive's SHA-256. On first launch gbx downloads it to `~/.gbx/grok/rev-<commit>/` and refuses it if the checksum does not match.
- **No update when nothing changed.** Upgrading gbx downloads and replaces nothing as long as the pinned Grok Build commit is the same; only a new commit triggers a download (about 70 MB).
- **Never swapped silently.** gbx turns off Grok Build's self-updater when it launches the pinned copy, so a closed-source upstream build cannot replace it.
- **Checkable at any time.** `gbx version` prints the gbx version, the pinned Grok Build version and source commit, and whether it is downloaded yet; `gbx doctor` shows which Grok Build is actually in use.

```text
$ gbx version
gbx version vX.Y.Z
Grok Build 1.0.45 (open source, built from xai-org/grok-build@2bdd1d6a6369)
  source: https://github.com/xai-org/grok-build/tree/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8
  installed: /Users/you/.gbx/grok/rev-2bdd1d6a6369/grok
```

### Install

```bash
npm install -g @grokbuildx/gbx                 # recommended, needs Node.js >= 22
go install github.com/grokbuildx/gbx@latest  # or Go
```

Or download the `.gz` for your platform from [Releases](https://github.com/grokbuildx/gbx/releases) (macOS / Linux / Windows, x64 and arm64), decompress it onto your `PATH`; `SHA256SUMS` covers the `.gz` files. Do not install via npm with `--omit=optional`, or the platform binary will be missing.

```bash
gbx version
gbx doctor     # read-only checks; --probe tests each model (billed)
```

On macOS and Linux, the npm packages and release binaries pin an open-source Grok Build; nothing else to install. It is downloaded and verified on first launch, and again only when the pinned commit changes. Windows builds and `go install` builds pin none and use, in order, `GBX_GROK_BIN`, `grok` on `PATH`, then `~/.grok/bin/grok`; setting `GBX_GROK_BIN` also overrides the pinned copy. Codex integration uses your existing Codex client.

### Quick start

```bash
# A. Subscription login (example with a GPT account alias)
gbx oauth gpt work

# B. API key / third-party gateway: single-page TUI — Endpoint + Key → Auto Configure → Save & Activate
gbx set deepseek

gbx            # launch Grok Build with the active provider
gbx --provider deepseek -p "hello"  # use deepseek once without changing the selection
```

A fresh install uses Grok Build's own login. Select a provider with `gbx oauth`, Save & Activate in `gbx set`, or `gbx use <name>`. Use `gbx use default` to return to Grok Build's own login.

Extra arguments reach Grok Build and can start a billed session. Put `--provider` before Grok's arguments, or use `--provider=deepseek`. Use `gbx grok doctor` to reach Grok's command of the same name.

### Commands

| Command | Purpose |
|---|---|
| `gbx set [name]` | Add / edit a provider (TUI); `-m <model>` sets the default model directly |
| `gbx ls [-a]` / `gbx use <name>` / `gbx preview [name]` | List / switch / preview Grok overlay; `gbx use default` uses Grok Build's own login |
| `gbx --provider <name> [Grok args]` | Use a provider for one launch without changing the selection |
| `gbx oauth <name> [alias]` / `gbx oauth prune` | Subscription login / remove unused credentials |
| `gbx provider cp\|mv\|rm\|models\|preview` | Provider management and model pool probing |
| `gbx codex run` / `start` / `status` / `stop` | Connect provider models and open ChatGPT / Codex (auto-stop on quit) |
| `gbx serve run` / `start` / `status` / `stop` | Use Grok Build in a local browser (auto-exit on close, or `start` in background) |
| `gbx mcp-server [-w <dir>]` | Expose local coding tools over MCP `stdio` to external AI hosts (no model config required) |
| `gbx cloud login\|push\|pull\|status\|remote\|tag\|logout` | Encrypted sync (iCloud / Google Drive) |
| `gbx bypass [on\|off]` | Add `--always-approve` on launch; trusted environments only |
| `gbx log [on\|off] [--level debug]` | Per-session logs (off by default, in `~/.gbx/logs/`) |
| `gbx lang [zh\|zh-TW\|en]` | UI language (`GBX_LANG` > config > system) |
| `gbx doctor` / `update` / `version` / `completion` | Diagnose / upgrade / version / completions |

Every subcommand supports `--help`.

### Browser sessions

```bash
gbx serve                        # same as gbx serve run: open browser, exit when tabs close
gbx serve run -p deepseek -w .   # select provider & workspace, auto-exit on browser close
gbx serve start                  # start detached in background (survives closing browser)
gbx serve status / gbx serve stop
```

Start a session, stream replies, stop a turn, and allow or deny the tool approvals that Grok requests. The page also includes a project file and Git panel, a web terminal, and settings for providers, OAuth login and cloud sync. Default mode (`gbx serve` / `gbx serve run`) opens your browser and automatically exits after all open tabs close (with a short reload grace window); `gbx serve start` runs detached in the background (pass `-o` to also open a browser) and stays alive until `gbx serve stop`. `-p` selects the provider for new sessions; omit it to use the active one. Repeat `-w`/`--workspace` to add project directories (default: the current directory). `--workspace-root` treats each direct subdirectory as a project. `-m`/`--model` and `--effort` set the default model and reasoning effort for new sessions. The server listens on `127.0.0.1` and requires the session link printed in the terminal. Use `--no-open` to print the link without opening it, `--port` to select a port, `--socket` to listen on a local Unix socket instead, or `--no-shell` to disable the web terminal.

Session history is stored in `~/.gbx/serve/sessions/` and survives restarts. Grok Build must support ACP v1 session loading.

### Codex model integration

```bash
gbx codex run              # connect active provider & open ChatGPT (auto-stop on quit)
gbx codex run deepseek -r  # select provider and gently restart if already open
gbx codex run -i           # independent mode (--independent / --solo), no OpenAI login
gbx codex run -a codex     # explicitly open Codex.app (default prefers ChatGPT.app)
gbx codex start            # background mode for Codex CLI / IDE / Linux
gbx codex stop             # manual disconnect & config restore (rarely needed in desktop mode)
```

- `gbx codex` / `gbx codex run` starts the proxy, immediately launches **ChatGPT.app** (falling back to **Codex.app**; choose explicitly with `-a chatgpt|codex`), and **automatically stops the proxy and restores `~/.codex/config.toml` when you quit the app** (`Cmd+Q` on macOS). If the app is already running, it prompts interactively to restart gently (or pass `-r` / `--restart` to restart without prompting). Use `gbx codex start` (or `run --no-open`) for background/CLI/IDE use without opening a desktop app.
- Bound provider edits automatically refresh the catalog and routes. New requests use new runtimes while existing responses finish. Codex decides when its picker refreshes; local JSON does not promise in-process reload, so restart the app if its list is stale.
- Attach multiple providers: `gbx codex run deepseek -w gpt -w grok` (`-w` / `--with` / `--with-provider`). Each route retains its own account. Later starts inherit additional bindings; `-w ''` clears them. Use `gbx codex run -R` (`--refresh`) to rediscover models explicitly.
- Local catalogs are the default. Independent mode can opt into `gbx codex run -i -c remote` (`--catalog remote`); `-c local` restores the fallback. Shared mode keeps merged JSON because built-in OpenAI rejects localhost remote catalog URLs. Remote fetching and background app-server updates are verified; immediate Desktop picker refresh is not promised. Gateway/catalog updates and client-picker reload are separate capabilities in every mode.
- To use another provider after OpenAI quota is exhausted, run `gbx codex run -i` (`--independent` / `--solo`). The custom GBX provider has its own session credential and a GBX-only picker. `gbx codex run -s` (`--shared`) restores the merged native/GBX picker. Starts and menu switches inherit the running mode; after stopping, a start without a mode flag defaults to shared.
- Codex chats resume with their saved model provider. Independent chats use `gbx_proxy`; switching to shared or stopping removes that definition, so those chats can report an unconfigured provider or `Model unavailable`. Restore `gbx codex run -i` and restart Codex to resume the original chat, or fork its history to use the other provider. Do not rewrite chat records. See [OpenAI Docs](https://learn.chatgpt.com/docs/codex-sdk#resume-a-thread-without-loading-its-history).
- `gbx use` only affects `gbx` launches, not a running proxy.
- Open Codex / ChatGPT once to create its profile; startup updates `~/.codex/config.toml` (honors `CODEX_HOME`). Labels show only model names, with a short `GBX` marker for a collision with a native name. WorkBuddy's internal revisions sharing a name appear once.
- GBX models in Codex follow the provider's context/compaction setting: Default uses 200K, or 1M for models marked `[1m]` in the model pool; Balanced uses the selected 500K / 800K budget. All compact at 85%. GBX generates replayable summaries for translated protocols; native Responses models retain upstream compaction.
- Translated models support Codex client tool search: GBX adapts search to an ordinary function call, Codex searches local tools, and GBX loads the returned definitions instead of sending all deferred tools up front. Codex executes actual tools and enforces permissions. The model must be able to use the search function and the discovered definitions correctly.
- Models that advertise image input can read screenshots. Translated models follow the installed native catalog's client tool mode; Codex still executes computer use and checks permissions. Within shared mode, normal conversation history can switch between translated models and native GPT. Native GPT compaction cannot be decrypted. Converted models can validate the matching local chat/checkpoint and recover a bounded visible-history excerpt with a notice that older details may be missing. Missing or mismatched records fail explicitly.
- Translated models use per-model effort tiers from the models.dev metadata cache. OAuth login and confirmed `gbx set` saves refresh it; `gbx set --refresh-models` updates metadata alone. Network failures use `~/.gbx/cache/modelsdev.json`; missing/unmatched metadata retains compatibility settings. The running proxy refreshes metadata; restart the app to reload a local JSON catalog. Native Responses models retain their own metadata.
- Codex retains its native OpenAI provider and login. Native requests keep their original ChatGPT / OpenAI route; only the separate `gbx/<provider>/<model>` allow-list goes to GBX, with native credentials removed. WebSockets receive 426 to fall back to HTTP Responses.
- Native metadata comes from an existing catalog or `models_cache.json`, with the installed Codex's `debug models --bundled` as fallback; native names are never hardcoded. Without a cache, install a recent Codex CLI or make the desktop app's bundled Codex discoverable. A conflicting custom provider or default-profile catalog override is reported instead of changing its routing.
- macOS shows a menu bar icon with the checked current provider, configuration switching, `Restart ChatGPT` / `Restart Codex`, and `Disconnect Codex`. Switching providers hot-reloads the gateway and prompts whether to gently restart the desktop app right away (with a restart guard so the proxy stays alive) or later from the menu bar. A failed proxy start restores the previous proxy and reports the error. The icon exits automatically with the proxy (including when the tracked app quits or on crash). Set `GBX_PROXY_NO_MENUBAR=1` (or `GBX_PROXY_NO_APP=1`) in a headless environment to disable desktop UI integration.
- The proxy manages only Codex configuration; ordinary Grok Build launches remain independent. Stop, auto-stop, and crash recovery restore only unchanged Codex fields owned by the proxy, retaining later edits. Upgrading an older shared proxy also restores its legacy Claude settings once.
- After a crash, the next `gbx` command restores settings from `~/.gbx/proxy/state.json`; startup diagnostics are in `~/.gbx/proxy/proxy.log`.

### Protocols

GBX serves Responses and Chat Completions on `127.0.0.1` with a random per-session key. Responses is the common input for provider adapters.

| Source | Upstream protocol |
|---|---|
| OpenAI Chat gateway / Kimi / WorkBuddy | Chat Completions |
| OpenAI Responses gateway / GPT / Grok | Responses with backend-specific identity headers |
| models.dev mixed gateway | Per model: Responses, Chat Completions, or Anthropic Messages (`@ai-sdk/anthropic` models, sent to the model's own models.dev base URL) |
| Cursor | Connect bidirectional agent stream |
| Kiro / Qoder | Amazon Q / COSY |
| AutoClaw | Managed Chat Completions proxy |

429 / 5xx normally get at most two quick retries (500 ms, 1 s). A longer `Retry-After` is relayed immediately instead of retrying early. Each backend handles 401 with its OAuth refresh; Qoder preserves the original authentication error if refresh fails. Qoder / WorkBuddy refuse cross-origin redirects for credential API, refresh and model requests. Accounts stay bound to the selected credential file, and disabled or changed credential types are not refreshed.

### Configuration

- `~/.gbx/config.yaml` stores provider settings, the active selection, language, bypass mode, and log level. **It contains plaintext API keys.**
- `~/.gbx/auth/*.json` stores OAuth credentials with 0600 permissions.
- Use `openai` for Chat Completions or `openai_responses` for a Responses API-key provider. An OAuth backend determines its own protocol.
- Each provider can set a default model with `gbx set -m <model>` or the TUI's Default Model field. Grok Build launches, Codex and `gbx serve` use it first; if unset, the first model in the pool is used.
- `gbx preview` (or `gbx provider preview`) displays the default and helper models and reasoning effort sent to Grok Build.
- Codex context presets are Default, Balanced 500K, and Balanced 800K.


### Cloud sync

`gbx cloud login` (Google Drive by default; `gbx cloud login icloud` on macOS), then `push` / `pull`. Without a browser (SSH, Cloud Shell, Linux without a display) Google Drive login prints the authorization URL instead: approve it in a browser on any device, which then lands on a `127.0.0.1` address that fails to load; paste that whole address back into the terminal. `--no-browser` / `--no-browser=false` forces either way. It syncs `config.yaml` and `auth/*.json`, gzip-compressed and encrypted with AES-256-GCM; Google Drive uses only the `drive.appdata` scope. The key is stored with the cloud profile, so a new device only needs `gbx cloud login` with the same account before `pull`, with no passphrase or pairing; in turn, anyone who can sign in to that cloud account (or holds its gbx authorization) can decrypt the synced provider credentials. On conflicts push / pull stop instead of overwriting.

### Development

```bash
gofmt -l .            # must be empty
go vet ./... && go test -race ./...
go build -o /tmp/gbx-verify .
node --test scripts/npm-packages.test.js
```

- Do not use `gbx` itself as a test harness: unknown args start a billed Grok session. Use `go test` and `--help` with a throwaway `HOME` / `USERPROFILE`.
- Architecture and conventions: [AGENTS.md](AGENTS.md) and [docs/development-architecture.md](docs/development-architecture.md); protocol notes: `internal/oauthproxy/doc.go`; npm release: [docs/npm-release.md](docs/npm-release.md); cloud sync design: [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md).
- Pushing a `v*` tag makes CI build six platform binaries and publish to GitHub Releases and npm.

### License

gbx is MIT-licensed. Grok Build is Apache-2.0 ([`third_party/grok-build/LICENSE`](third_party/grok-build/LICENSE)); every `grok-build-<commit>` release includes `GROK_BUILD_LICENSE` and `GROK_BUILD_THIRD_PARTY_NOTICES`. Other third-party references: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). gbx is an independent open-source project and is not affiliated with xAI.
