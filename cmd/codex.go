package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/grokbuildx/gbx/internal/codex"
	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/proxymanager"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Test hooks for isolated CLI and menu tests without touching real OS apps.
var (
	codexDesktopController  = proxymanager.DefaultDesktopController
	codexDesktopDisabled    = proxymanager.DesktopAppDisabled
	codexMenuPromptAction   = proxymanager.PromptMenuAction
	codexIsInteractiveInput = func(r io.Reader) bool {
		file, ok := r.(*os.File)
		return ok && term.IsTerminal(file.Fd())
	}
	startCodexProxy = func(ctx context.Context, m *proxymanager.Manager, executable string, p provider.Provider) (*proxymanager.State, error) {
		return m.Start(ctx, executable, p)
	}
)

// recoverStaleProxy restores a dead Codex proxy's configuration, including
// Claude settings left behind by older shared-proxy versions.
// It is quiet unless it restored something, and never fatal.
func recoverStaleProxy() {
	m, err := proxymanager.Default()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	restored, err := m.RecoverStale(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, locale.T("gbx: 无法恢复已退出代理的客户端配置：%v；请运行 gbx codex stop\n", "gbx: could not restore client settings for an exited proxy: %v; run gbx codex stop\n"), err)
		return
	}
	if restored {
		fmt.Fprintln(os.Stderr, locale.T("gbx: 代理已意外退出，原有客户端配置已恢复", "gbx: the proxy had exited; previous client settings restored"))
	}
}

func proxyProvider(name string) (provider.Provider, error) {
	cfg, err := config.Load()
	if err != nil {
		return provider.Provider{}, err
	}
	if name == "" {
		name = cfg.ActiveProvider
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return provider.Provider{}, fmt.Errorf("provider %q not found; use gbx ls or gbx set", name)
	}
	p.Name = name
	return p, nil
}

// runningProxyProvider reports the primary provider a live standalone proxy
// serves. A stale journal of a dead proxy does not count: startup recovery
// restores it.
var runningProxyProvider = func(ctx context.Context) (string, bool) {
	providers := runningProxyProviders(ctx)
	if len(providers) == 0 {
		return "", false
	}
	return providers[0], true
}

// runningProxyProviders reports all providers (primary plus any --with
// bindings) served by a live standalone Codex proxy.
var runningProxyProviders = func(ctx context.Context) []string {
	m, err := proxymanager.Default()
	if err != nil {
		return nil
	}
	s, err := m.Read()
	if err != nil || s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if !m.Running(ctx, s) {
		return nil
	}
	out := make([]string, 0, 1+len(s.AdditionalProviders))
	if name := strings.TrimSpace(s.Provider); name != "" {
		out = append(out, name)
	}
	for _, extra := range s.AdditionalProviders {
		if name := strings.TrimSpace(extra); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func startCodexDaemon(cmd *cobra.Command, args []string) (*proxymanager.Manager, *proxymanager.State, bool, error) {
	name, _ := cmd.Flags().GetString("provider")
	if len(args) > 0 {
		if name != "" {
			return nil, nil, false, fmt.Errorf("specify the provider either by name or with --provider, not both")
		}
		name = args[0]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, nil, false, err
		}
		name = cfg.ActiveProvider
	}
	if provider.IsDefaultProvider(name) {
		return nil, nil, true, runProxyDefault(cmd)
	}
	p, err := proxyProvider(name)
	if err != nil {
		return nil, nil, false, err
	}
	if err := provider.ValidateRuntime(p); err != nil {
		return nil, nil, false, err
	}
	m, err := proxymanager.Default()
	if err != nil {
		return nil, nil, false, err
	}
	independent, _ := cmd.Flags().GetBool("independent")
	catalogMode, _ := cmd.Flags().GetString("catalog")
	m.CatalogMode, err = codex.ParseCatalogMode(catalogMode)
	if err != nil {
		return nil, nil, false, err
	}
	m.Refresh, _ = cmd.Flags().GetBool("refresh")
	if cmd.Flags().Changed("with") {
		m.AdditionalProviders, _ = cmd.Flags().GetStringSlice("with")
	}
	shared, _ := cmd.Flags().GetBool("shared")
	if independent && shared {
		return nil, nil, false, fmt.Errorf("choose -i/--independent or -s/--shared")
	}
	if independent {
		m.CodexMode = "independent"
	} else if shared {
		m.CodexMode = "shared"
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, false, err
	}
	signalCtx, cancelSignals := signal.NotifyContext(cmd.Context(), proxySignals...)
	defer cancelSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 2*time.Minute)
	defer cancel()
	s, err := startCodexProxy(ctx, m, executable, p)
	if err != nil {
		return nil, nil, false, err
	}
	return m, s, false, nil
}

func runProxyStart(cmd *cobra.Command, args []string) error {
	_, s, handled, err := startCodexDaemon(cmd, args)
	if err != nil || handled {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), locale.T("Codex 模型接入已开启 · %s · %s\n停止：gbx codex stop\n", "Codex model integration running · %s · %s\nStop with: gbx codex stop\n"), s.Provider, s.BaseURL)
	if s.Codex != nil {
		fmt.Fprintln(cmd.OutOrStdout(), codexCatalogConnectionMessage(s.CatalogCapabilities()))
		if s.CodexMode == "independent" {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("独立提供商模式（-i）：不依赖 OpenAI 登录。恢复共享目录：gbx codex start -s", "Independent provider mode (-i): OpenAI login is not required. Restore the shared catalog: gbx codex start -s"))
		}
	}
	return nil
}

func desktopQuitHint(appName string) string {
	if runtime.GOOS == "darwin" {
		return locale.Tf("退出 %s（Cmd+Q）后将自动停止代理并恢复原有配置", "quitting %s (Cmd+Q) will automatically stop the proxy and restore original settings", appName)
	}
	return locale.Tf("退出 %s 后将自动停止代理并恢复原有配置", "quitting %s will automatically stop the proxy and restore original settings", appName)
}

func promptDesktopRestart(in io.Reader, out io.Writer, appName string) (bool, error) {
	fmt.Fprint(out, locale.Tf(
		"检测到 %s 正在运行，需要温和重启以加载最新模型列表。现在重启？[Y/n]: ",
		"%s is already running and needs a gentle restart to load the model list. Restart now? [Y/n]: ",
		appName,
	))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes", nil
}

func runProxyDesktop(cmd *cobra.Command, args []string) error {
	rawApp, _ := cmd.Flags().GetString("app")
	appChoice, err := proxymanager.ParseDesktopApp(rawApp)
	if err != nil {
		return err
	}
	restart, _ := cmd.Flags().GetBool("restart")
	noOpen, _ := cmd.Flags().GetBool("no-open")
	if noOpen {
		if appChoice != proxymanager.DesktopAppAuto {
			return fmt.Errorf("cannot combine --app with --no-open")
		}
		if restart {
			return fmt.Errorf("cannot combine --restart with --no-open")
		}
		return runProxyStart(cmd, args)
	}

	// Validate explicit --app target before starting a proxy daemon so missing
	// desktop apps fail fast cleanly.
	var resolvedApp *proxymanager.WatchedApp
	desktopEnabled := !codexDesktopDisabled()
	ctrl := codexDesktopController()
	if desktopEnabled {
		var found bool
		resolvedApp, found, err = ctrl.Resolve(appChoice)
		if err != nil {
			return err
		}
		if !found && appChoice != proxymanager.DesktopAppAuto {
			return fmt.Errorf(locale.T(
				"未找到 %s 桌面应用；请确认已安装，或改用 --app 指定另一款应用 / gbx codex start 仅启动后台代理",
				"%s desktop app not found; install it, choose the other app with --app, or use gbx codex start for background mode",
			), appChoice.DisplayName())
		}
	}

	m, s, handled, err := startCodexDaemon(cmd, args)
	if err != nil || handled {
		return err
	}

	if !desktopEnabled {
		fmt.Fprintf(cmd.OutOrStdout(), locale.T("Codex 模型接入已开启 · %s · %s\n停止：gbx codex stop\n", "Codex model integration running · %s · %s\nStop with: gbx codex stop\n"), s.Provider, s.BaseURL)
		if s.Codex != nil {
			fmt.Fprintln(cmd.OutOrStdout(), codexCatalogConnectionMessage(s.CatalogCapabilities()))
			if s.CodexMode == "independent" {
				fmt.Fprintln(cmd.OutOrStdout(), locale.T("独立提供商模式（-i）：不依赖 OpenAI 登录。恢复共享目录：gbx codex run -s", "Independent provider mode (-i): OpenAI login is not required. Restore the shared catalog: gbx codex run -s"))
			}
		}
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), locale.T("Codex 模型接入已开启 · %s · %s\n", "Codex model integration running · %s · %s\n"), s.Provider, s.BaseURL)

	if resolvedApp == nil {
		fmt.Fprintln(cmd.OutOrStdout(), locale.T(
			"未检测到 ChatGPT / Codex 桌面应用；代理已在后台运行（供 Codex CLI / IDE 使用）。\n停止：gbx codex stop",
			"No ChatGPT or Codex desktop app found; proxy is running in background (for Codex CLI / IDE).\nStop with: gbx codex stop",
		))
		if s.Codex != nil && s.CodexMode == "independent" {
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("独立提供商模式（-i）：不依赖 OpenAI 登录。恢复共享目录：gbx codex run -s", "Independent provider mode (-i): OpenAI login is not required. Restore the shared catalog: gbx codex run -s"))
		}
		return nil
	}

	opCtx, cancelOp := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancelOp()
	running, err := ctrl.IsRunning(opCtx, resolvedApp)
	if err != nil {
		return fmt.Errorf("check %s status: %w", resolvedApp.Name, err)
	}

	if !running {
		if err := proxymanager.OpenAndWatchDesktopApp(opCtx, m, ctrl, resolvedApp); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), locale.T(
			"已打开 %s；%s\n",
			"Opened %s; %s\n",
		), resolvedApp.Name, desktopQuitHint(resolvedApp.Name))
	} else {
		doRestart := restart
		prompted := false
		if !doRestart && codexIsInteractiveInput(cmd.InOrStdin()) {
			prompted = true
			confirmed, promptErr := promptDesktopRestart(cmd.InOrStdin(), cmd.ErrOrStderr(), resolvedApp.Name)
			if promptErr != nil {
				return promptErr
			}
			doRestart = confirmed
		}
		if doRestart {
			fmt.Fprintf(cmd.OutOrStdout(), locale.T("正在温和重启 %s…\n", "Restarting %s gracefully…\n"), resolvedApp.Name)
			err := proxymanager.GracefulRestartDesktopApp(opCtx, m, ctrl, resolvedApp)
			if errors.Is(err, proxymanager.ErrDesktopAppQuitTimedOut) {
				_ = m.ClearAppWatch(opCtx)
				fmt.Fprintf(cmd.OutOrStdout(), locale.T(
					"未能自动关闭 %s（可能存在未确认的弹窗）。请手动重启 %s 以加载模型列表；代理仍在运行（手动断开：gbx codex stop）。\n",
					"Could not close %s automatically (a confirmation prompt may be open). Restart %s manually to load the model list; proxy stays running (disconnect: gbx codex stop).\n",
				), resolvedApp.Name, resolvedApp.Name)
			} else if err != nil {
				return err
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), locale.T(
					"已温和重启 %s；%s\n",
					"Restarted %s gracefully; %s\n",
				), resolvedApp.Name, desktopQuitHint(resolvedApp.Name))
			}
		} else {
			// Detach auto-stop so a manual quit-and-reopen keeps the proxy alive.
			_ = m.ClearAppWatch(opCtx)
			if prompted {
				fmt.Fprintf(cmd.OutOrStdout(), locale.T(
					"已保持 %s 运行。手动重启 %s 即可加载模型列表；如需温和重启并自动托管退出：gbx codex run -r（手动断开：gbx codex stop）\n",
					"Left %s running. Restart %s manually to load the model list, or run gbx codex run -r to restart and auto-stop on quit (disconnect: gbx codex stop)\n",
				), resolvedApp.Name, resolvedApp.Name)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), locale.T(
					"检测到 %s 正在运行。请手动重启 %s，或运行 gbx codex run -r 温和重启并托管退出（手动断开：gbx codex stop）\n",
					"%s is already running. Restart %s manually, or run gbx codex run -r to restart gently and auto-stop on quit (disconnect: gbx codex stop)\n",
				), resolvedApp.Name, resolvedApp.Name)
			}
		}
	}

	if s.Codex != nil && s.CodexMode == "independent" {
		fmt.Fprintln(cmd.OutOrStdout(), locale.T("独立提供商模式（-i）：不依赖 OpenAI 登录。恢复共享目录：gbx codex run -s", "Independent provider mode (-i): OpenAI login is not required. Restore the shared catalog: gbx codex run -s"))
	}
	return nil
}

// runProxyDefault turns off the Codex proxy and restores the original route.
func runProxyDefault(cmd *cobra.Command) error {
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	s, err := m.Read()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(cmd.OutOrStdout(), locale.T(
			"default 无需 GBX 模型接入；Codex 使用原有配置",
			"default needs no GBX integration; Codex uses its original configuration",
		))
		return nil
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), locale.T(
		"已停止 %s 的 Codex 模型接入，Codex 恢复原有配置\n",
		"Disconnected %s from Codex; Codex is back on its original configuration\n",
	), s.Provider)
	return nil
}

func addDesktopLaunchFlags(c *cobra.Command) {
	c.Flags().StringP("app", "a", "", locale.T(
		"选择要打开并托管退出的桌面应用：chatgpt（优先）或 codex（二选一）",
		"Desktop app to open and track: chatgpt (preferred) or codex (mutually exclusive)",
	))
	c.Flags().BoolP("restart", "r", false, locale.T(
		"若目标桌面应用已在运行，直接温和重启以加载最新模型列表（不询问）",
		"Gracefully restart the desktop app without prompting if already running",
	))
	c.Flags().Bool("no-open", false, locale.T(
		"仅在后台启动代理，不打开也不绑定桌面应用退出",
		"Start the proxy in background without opening or tracking a desktop app",
	))
}

func newCodexCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "codex [provider]",
		Short: locale.T("将 GBX 模型接入 ChatGPT / Codex 桌面端或 CLI", "Connect GBX models to ChatGPT / Codex Desktop or CLI"),
		Args:  cobra.MaximumNArgs(1),
		RunE:  runProxyDesktop,
		Long: locale.T(`将 GBX Provider 模型接入 ChatGPT / Codex（默认优先拉起 ChatGPT.app，退出应用后自动停止代理并恢复原配置）。

常用命令：
  gbx codex                        一键接入当前 Provider 并打开 ChatGPT（等价于 gbx codex run）
  gbx codex run [provider]         接入指定 Provider 并打开 ChatGPT / Codex，退出应用自动还原
  gbx codex run -r                 若桌面应用已在运行，直接温和重启并托管退出
  gbx codex run -i                 独立模式（--independent / --solo）：不依赖 OpenAI 账号登录与额度
  gbx codex run -s                 共享模式（--shared）：同时保留原生 OpenAI 与 GBX 模型
  gbx codex run -a codex           显式选择 Codex.app（默认优先 ChatGPT.app，二者二选一）
  gbx codex run one -w two         同时挂载多个 Provider（-w / --with / --with-provider，可重复）
  gbx codex run -i -c remote       独立模式下启用本地远程模型目录（-c / --catalog）
  gbx codex run -R                 刷新上游账号模型目录（-R / --refresh），不中断进行中的请求

后台与管理命令：
  gbx codex start [provider]       仅在后台启动代理（不打开桌面应用，适合 Codex CLI / IDE / Linux）
  gbx codex status                 查看运行中的 Provider、端点、模式及托管的桌面应用
  gbx codex stop                   手动断开代理并恢复原有配置（不关 App 断开或异常恢复时使用）
  gbx codex run default            断开 GBX 并恢复 Codex 原有路由

说明：
  - 首次使用前请先打开一次 ChatGPT / Codex 以生成配置目录（支持 CODEX_HOME）。
  - 桌面应用已在运行时，gbx codex run 会在交互终端询问是否温和重启（或传 -r 直接温和重启）。
  - macOS 状态栏支持热切换 Provider，切换后会弹窗询问是否立即温和重启（带重启保护，不会误停代理）。
  - 独立模式（-i）会话绑定 gbx_proxy，切回共享模式（-s）或断开后需恢复 -i 才能续接原会话。`, `Connect GBX provider models to ChatGPT / Codex (prefers ChatGPT.app, opens it immediately, and auto-stops the proxy when the app quits).

Quick Start:
  gbx codex                        Connect active provider and open ChatGPT (same as gbx codex run)
  gbx codex run [provider]         Connect a provider, open ChatGPT / Codex, auto-stop on quit
  gbx codex run -r                 Gracefully restart the desktop app if already running
  gbx codex run -i                 Independent mode (--independent / --solo): no OpenAI login required
  gbx codex run -s                 Shared mode (--shared): native OpenAI + GBX models together
  gbx codex run -a codex           Explicitly pick Codex.app (default prefers ChatGPT.app; 2-pick-1)
  gbx codex run one -w two         Attach extra providers (-w / --with / --with-provider, repeatable)
  gbx codex run -i -c remote       Opt into loopback remote catalog (-c / --catalog)
  gbx codex run -R                 Rediscover account models (-R / --refresh) without interrupting output

Background & Recovery:
  gbx codex start [provider]       Start background proxy only (for Codex CLI / IDE / Linux / scripts)
  gbx codex status                 Show active provider, mode, endpoint, and tracked desktop app
  gbx codex stop                   Manually disconnect and restore original Codex config
  gbx codex run default            Disconnect GBX and restore Codex's original route

Notes:
  - Open ChatGPT / Codex once first to create its profile (CODEX_HOME is supported).
  - If the desktop app is already running, gbx codex run prompts for a gentle restart (or use -r).
  - On macOS, the menu bar switches providers live and offers one-click graceful restart (guarded so the proxy stays alive).
  - Independent mode (-i) binds chats to gbx_proxy; re-enable -i to resume those chats, or fork to switch modes.`),
	}
	normalizeCodexFlags := func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "solo", "indep":
			return "independent"
		case "with-provider":
			return "with"
		}
		return pflag.NormalizedName(name)
	}
	command.PersistentFlags().SetNormalizeFunc(normalizeCodexFlags)
	command.Flags().SetNormalizeFunc(normalizeCodexFlags)
	command.PersistentFlags().StringP("provider", "p", "", locale.T("指定主 Provider 名称（默认使用当前激活 Provider）", "Provider name (defaults to the active provider)"))
	command.PersistentFlags().BoolP("independent", "i", false, locale.T("独立模式：不依赖 OpenAI 账号认证（别名：--solo）", "Independent mode without OpenAI account auth (alias: --solo)"))
	command.PersistentFlags().BoolP("shared", "s", false, locale.T("共享模式：合并原生 OpenAI 与 GBX 模型目录", "Shared mode with native OpenAI + GBX merged catalog"))
	command.PersistentFlags().StringP("catalog", "c", "", locale.T("模型目录来源：local（默认）或 remote（需配合 -i）", "Catalog source: local (default) or remote (requires -i)"))
	command.PersistentFlags().StringSliceP("with", "w", nil, locale.T("额外挂载的 Provider，可重复（别名：--with-provider；传 '' 清空）", "Extra providers to attach, repeatable (alias: --with-provider; '' clears)"))
	command.PersistentFlags().BoolP("refresh", "R", false, locale.T("重新发现上游账号模型并热替换运行时，不中断进行中的请求", "Rediscover account models and replace runtimes without interrupting requests"))
	command.MarkFlagsMutuallyExclusive("independent", "shared")
	addDesktopLaunchFlags(command)

	runCmd := &cobra.Command{
		Use:   "run [provider]",
		Args:  cobra.MaximumNArgs(1),
		Short: locale.T("启动代理并打开 ChatGPT / Codex，退出应用自动停止并还原", "Start proxy, open ChatGPT / Codex, and auto-stop on quit"),
		Long: locale.T(`启动（或热切换）GBX Codex 代理，并立即打开桌面端应用：
  - 默认优先拉起 ChatGPT.app，未安装时回退 Codex.app（可用 -a chatgpt|codex 二选一）。
  - 若应用已在运行，会在交互终端询问是否温和重启；传 -r/--restart 可直接温和重启。
  - 退出该桌面应用（macOS Cmd+Q / Windows 关闭）后，代理自动停止并恢复 ~/.codex/config.toml，无需手动 stop。

示例：
  gbx codex run                    使用当前 Provider 并打开 ChatGPT
  gbx codex run deepseek -i -r     独立模式接入 deepseek 并温和重启 ChatGPT
  gbx codex run -a codex           指定打开 Codex.app
  gbx codex run one -w two -w grok 同时挂载多个 Provider
  gbx codex run --no-open          仅在后台启动代理（同 gbx codex start）`, `Start (or hot-switch) the GBX Codex proxy and immediately launch the desktop app:
  - Prefers ChatGPT.app, falling back to Codex.app (pick explicitly with -a chatgpt|codex).
  - If the app is already running, prompts for a gentle restart (or pass -r/--restart).
  - Quitting the app (Cmd+Q on macOS) automatically stops the proxy and restores ~/.codex/config.toml.

Examples:
  gbx codex run                    Connect active provider and open ChatGPT
  gbx codex run deepseek -i -r     Independent mode with deepseek and gentle restart
  gbx codex run -a codex           Explicitly open Codex.app
  gbx codex run one -w two -w grok Attach multiple providers to one gateway
  gbx codex run --no-open          Background proxy only (same as gbx codex start)`),
		RunE: runProxyDesktop,
	}
	addDesktopLaunchFlags(runCmd)

	start := &cobra.Command{
		Use:   "start [provider]",
		Args:  cobra.MaximumNArgs(1),
		Short: locale.T("仅在后台启动或切换代理（适合 Codex CLI / IDE / 无桌面环境）", "Start or switch the background proxy without opening a desktop app"),
		Long: locale.T(`在后台启动或热切换 GBX Codex 代理，不打开也不绑定桌面应用退出。
适用于 Codex CLI、VS Code / IDE 插件、Linux/SSH 服务器或脚本场景。

示例：
  gbx codex start                  用当前 Provider 启动后台代理
  gbx codex start deepseek -i      独立模式后台启动
  gbx codex start -R               刷新运行中的模型目录
  gbx codex start default          断开 GBX 并恢复原有配置`, `Start or hot-switch the background GBX Codex proxy without opening or tracking a desktop app.
Best suited for Codex CLI, IDE extensions, Linux/SSH environments, and scripts.

Examples:
  gbx codex start                  Start background proxy with active provider
  gbx codex start deepseek -i      Start background proxy in independent mode
  gbx codex start -R               Rediscover models on the running proxy
  gbx codex start default          Disconnect GBX and restore original settings`),
		RunE: runProxyStart,
	}
	stop := &cobra.Command{
		Use:   "stop",
		Args:  cobra.NoArgs,
		Short: locale.T("手动断开代理并恢复原有 Codex 配置", "Disconnect GBX models and restore original Codex settings"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := proxymanager.Default()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := m.Stop(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入已停止，原有配置已恢复", "Codex model integration stopped; previous settings restored"))
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Args:  cobra.NoArgs,
		Short: locale.T("查看代理运行状态、端点及托管的桌面应用（不暴露凭据）", "Show Codex model integration status without revealing credentials"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := proxymanager.Default()
			if err != nil {
				return err
			}
			s, err := m.Read()
			if err != nil {
				return err
			}
			if s == nil {
				fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入未开启", "Codex model integration is stopped"))
				return nil
			}
			if !m.Running(cmd.Context(), s) {
				fmt.Fprintln(cmd.OutOrStdout(), locale.T("Codex 模型接入未响应；运行 gbx codex stop 恢复配置", "Codex model integration is not responding; run gbx codex stop to restore settings"))
				return nil
			}
			endpoint := s.BaseURL
			if s.Codex != nil {
				endpoint = s.Codex.BaseURL
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\nCodex mode: %s\nCodex endpoint: %s\nPID: %d\n", s.Provider, s.CodexMode, endpoint, s.PID)
			fmt.Fprintf(cmd.OutOrStdout(), "Catalog source: %s\n", s.CatalogMode.Effective())
			if s.WatchedApp.Valid() {
				fmt.Fprintf(cmd.OutOrStdout(), "Desktop app: %s (auto-stop on quit)\n", s.WatchedApp.Name)
			}
			fmt.Fprintln(cmd.OutOrStdout(), codexCatalogConnectionMessage(s.CatalogCapabilities()))
			if len(s.AdditionalProviders) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Additional providers: %s\n", strings.Join(s.AdditionalProviders, ", "))
			}
			if s.Codex != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Codex config: %s\n", s.Codex.Path)
			}
			return nil
		},
	}
	serve := &cobra.Command{Use: "serve", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		name, _ := cmd.Flags().GetString("provider")
		id, _ := cmd.Flags().GetString("instance")
		if id == "" {
			return fmt.Errorf("internal proxy serve requires an instance; use gbx codex run")
		}
		p, err := proxyProvider(name)
		if err != nil {
			return err
		}
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		m.CodexListenAddress, _ = cmd.Flags().GetString("codex-listen-address")
		m.CodexMode, _ = cmd.Flags().GetString("codex-mode")
		catalogMode, _ := cmd.Flags().GetString("catalog-mode")
		m.CatalogMode, err = codex.ParseCatalogMode(catalogMode)
		if err != nil {
			return err
		}
		m.AdditionalProviders, _ = cmd.Flags().GetStringSlice("with")
		ctx, cancel := signal.NotifyContext(cmd.Context(), proxySignals...)
		defer cancel()
		return m.Run(ctx, p, id)
	}}
	serve.Flags().String("instance", "", "Internal startup identity")
	serve.Flags().String("codex-mode", "shared", "Internal Codex provider mode")
	serve.Flags().String("catalog-mode", "local", "Internal Codex catalog mode")
	serve.Flags().String("codex-listen-address", "", "Internal replacement Codex listener address")
	// Proxy failures are runtime errors, not usage mistakes: print the error
	// once (from Execute) without cobra's usage block.
	for _, c := range []*cobra.Command{command, runCmd, start, stop, status, serve} {
		c.SilenceUsage, c.SilenceErrors = true, true
	}
	menuBar := &cobra.Command{Use: "menubar", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		id, _ := cmd.Flags().GetString("instance")
		if id == "" {
			return fmt.Errorf("internal menu bar requires a proxy instance")
		}
		m, err := proxymanager.Default()
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), proxySignals...)
		defer cancel()
		return m.RunMenuBar(ctx, id)
	}}
	menuBar.Flags().String("instance", "", "Internal proxy identity")
	command.AddCommand(runCmd, start, stop, status, serve, menuBar)
	addProxyMenuCommands(command)
	for _, sub := range command.Commands() {
		sub.Flags().SetNormalizeFunc(normalizeCodexFlags)
	}
	return command
}

func init() { rootCmd.AddCommand(newCodexCommand()) }
