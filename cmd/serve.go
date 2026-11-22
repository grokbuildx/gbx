package cmd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grokbuildx/gbx/internal/browser"
	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/selfupdate"
	"github.com/grokbuildx/gbx/internal/webui"
	"github.com/spf13/cobra"
)

// openServeBrowser is overridden in tests so isolated runs never open a real browser.
var openServeBrowser = browser.Open

// serveExecutable resolves the binary spawned by `gbx serve start`; overridden in tests.
var serveExecutable = selfBinary

type serveOptions struct {
	port           int
	workspaces     []string
	workspaceRoots []string
	token          string
	maxConcurrency int
	providerName   string
	model          string
	effort         string
	openBrowser    bool
	noOpen         bool
	shell          bool
	noShell        bool
	lockWorkspaces bool
	socket         string
	basePath       string
	sandbox        string
	autoExit       bool
	exitGrace      time.Duration
}

const (
	serveStopReasonSignal int32 = iota
	serveStopReasonBrowserClosed
	serveStopReasonRemoteStop
)

func addServeFlags(cmd *cobra.Command, opts *serveOptions, defaultOpen, allowAutoExit bool) {
	flags := cmd.Flags()
	flags.IntVar(&opts.port, "port", 0, locale.T("本地回环监听端口（0 表示自动分配）", "Loopback TCP port to listen on (0 selects a free port)"))
	if defaultOpen {
		flags.BoolVarP(&opts.openBrowser, "open", "o", true, locale.T("服务就绪后打开系统浏览器（--open=false 关闭）", "Open the system browser once ready (--open=false to disable)"))
	} else {
		flags.BoolVarP(&opts.openBrowser, "open", "o", false, locale.T("后台启动就绪后顺便打开系统浏览器", "Also open the system browser once the background server is ready"))
	}
	flags.BoolVar(&opts.noOpen, "no-open", false, locale.T("不自动打开浏览器（覆盖 --open）", "Do not open a browser; overrides --open"))
	flags.StringArrayVarP(&opts.workspaces, "workspace", "w", nil, locale.T("允许运行会话的项目目录（可重复；默认当前目录）", "Project folder conversations may run in (repeatable; default: current directory)"))
	flags.StringArrayVar(&opts.workspaceRoots, "workspace-root", nil, locale.T("其直接子目录都作为项目的根目录（可重复）", "Directory whose subdirectories are all projects (repeatable)"))
	flags.StringVar(&opts.token, "token", "", locale.T("/api 鉴权 token（默认读取 GBX_SERVE_TOKEN，未设置时自动生成）", "Token required by /api (defaults to GBX_SERVE_TOKEN, else generated per run)"))
	flags.IntVar(&opts.maxConcurrency, "max-concurrency", 3, locale.T("最大并发 Grok Build 进程数", "Maximum simultaneous Grok Build processes"))
	flags.StringVarP(&opts.providerName, "provider", "p", "", locale.T("新会话默认使用的 Provider（默认当前激活 Provider）", "Provider new conversations start with (default: active provider)"))
	flags.StringVarP(&opts.model, "model", "m", "", locale.T("新会话默认模型 ID", "Model ID new conversations start with"))
	flags.StringVar(&opts.effort, "effort", "", locale.T("新会话默认推理强度（low, medium, high, xhigh, max）", "Effort new conversations start with (low, medium, high, xhigh, max)"))
	flags.BoolVar(&opts.shell, "shell", true, locale.T("在页面提供项目目录终端", "Offer a terminal (a shell in the project folder) in the page"))
	flags.BoolVar(&opts.noShell, "no-shell", false, locale.T("禁用页面终端（覆盖 --shell）", "Do not offer a terminal in the page; overrides --shell"))
	flags.BoolVar(&opts.lockWorkspaces, "lock-workspaces", false, locale.T("锁定项目列表，禁止从页面添加目录", "Serve only --workspace / --workspace-root folders; disallow adding others from the page"))
	flags.StringVar(&opts.socket, "socket", "", locale.T("改为监听本机 Unix socket（不带路径时使用 ~/.gbx/serve/gbx.sock）", "Listen on a local Unix socket instead of TCP (no value: ~/.gbx/serve/gbx.sock)"))
	flags.StringVar(&opts.sandbox, "sandbox", "", locale.T("新对话的操作系统沙盒：workspace 或 off（默认使用网页保存的设置，否则 workspace；已有对话保持原模式）", "OS sandbox for new conversations: workspace or off (default: saved web setting, otherwise workspace; existing conversations keep their mode)"))
	flags.StringVar(&opts.basePath, "base-path", "", locale.T("挂载页面与 /api 的 URL 前缀，如 /chat", "Mount the page and /api under a URL prefix such as /chat"))
	flags.Lookup("socket").NoOptDefVal = defaultSocket
	if allowAutoExit {
		flags.BoolVar(&opts.autoExit, "auto-exit", true, locale.T("所有浏览器页面关闭后自动退出（仅打开浏览器模式生效；--auto-exit=false 关闭）", "Automatically exit when all browser tabs close (active in browser mode; --auto-exit=false to disable)"))
		flags.DurationVar(&opts.exitGrace, "exit-grace", webui.DefaultAutoExitGrace, locale.T("关闭最后一个页面后等待重连的宽限时间（避免刷新页面误退）", "Grace period after the last tab closes so page reloads do not stop the server"))
		_ = flags.MarkHidden("exit-grace")
	}
}

func newServeCommand() *cobra.Command {
	rootOpts := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve [run|start|status|stop] [flags]",
		Short: locale.T("在本地浏览器中使用 Grok Build 会话（默认打开浏览器，关闭页面自动退出）", "Use Grok Build conversations in a local browser (opens browser and exits when closed)"),
		Long: locale.T(`在本地浏览器中运行 gbx 会话：每个对话在项目目录中通过 ACP v1 启动 Grok Build（读写项目文件、运行工具、切换 Provider）。

默认行为（gbx serve / gbx serve run）：
  启动本地服务并立即打开系统浏览器；当所有浏览器标签页都关闭后（短暂宽限避免刷新误退），服务会自动清理并退出。
  使用 --no-open 或 --socket 时保持前台运行，不跟随浏览器退出。

常驻后台模式（gbx serve start / status / stop）：
  gbx serve start 在后台常驻运行，关闭浏览器不会停止服务，直到手动执行 gbx serve stop。

每一个回合起一个新的 Grok Build ACP 进程，并用 LoadSession 续接同一会话，因此 Provider、模型、推理强度（effort）和权限模式
在回合边界生效；停止当前回合会立即取消 ACP 请求并回收进程。会话历史存放在 ~/.gbx/serve/sessions/<id>/，重启后仍在。

沙盒默认开启。--sandbox workspace|off 覆盖本次启动的新对话默认值；网页设置可修改并保存该默认值。
服务严格绑定在回环地址 127.0.0.1（或 0600 权限的本机 Unix socket --socket）。TCP 模式默认自动生成一次性会话 token 并通过
启动链接的 #fragment 传给浏览器（也可通过 --token 或 GBX_SERVE_TOKEN 指定固定 token），同时校验回环 Host 与同机 Origin。`, `Run gbx conversations in a local browser: each conversation drives Grok Build over ACP v1 in its project directory
(reading and editing files, running tools, and switching providers).

Default browser mode (gbx serve / gbx serve run):
  Starts the loopback server and immediately opens your browser. Once all browser tabs are closed (after a short
  reload grace window), the server shuts down cleanly and exits automatically. Passing --no-open or --socket keeps
  it running in the foreground without auto-exit.

Persistent background mode (gbx serve start / status / stop):
  gbx serve start runs detached in the background and stays alive even after closing the browser, until you run
  gbx serve stop.

Every turn starts a fresh Grok Build ACP worker and resumes the conversation with LoadSession, so provider, model,
reasoning effort, and permission mode take effect at turn boundaries. Session history lives under
~/.gbx/serve/sessions/<id>/ and survives restarts.

The OS sandbox defaults to workspace. TCP always binds 127.0.0.1 with loopback Host/Origin checks and a per-run
random token passed via the launch URL #fragment (unless --token or GBX_SERVE_TOKEN is set). --socket serves the
same API over a mode-0600 local Unix socket without opening a port.`),
		Example: locale.T(`  gbx serve                      # 启动并打开浏览器，关闭浏览器页面后自动退出
  gbx serve run -w ~/proj        # 指定项目目录运行，关闭浏览器自动退出
  gbx serve start                # 后台常驻启动（关闭浏览器不退出）
  gbx serve start -o --port 8080 # 后台常驻启动并顺便打开浏览器
  gbx serve status               # 查看运行状态与已连接标签页数量
  gbx serve stop                 # 停止正在运行的 gbx serve`, `  gbx serve                      # Open in browser and exit automatically when tabs close
  gbx serve run -w ~/proj        # Serve a specific project and exit when the browser closes
  gbx serve start                # Run detached in the background (survives browser close)
  gbx serve start -o --port 8080 # Start in background on port 8080 and open browser
  gbx serve status               # Show server status and connected browser tab count
  gbx serve stop                 # Stop the running gbx serve instance`),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeServer(cmd, rootOpts, args, webui.ServeModeRun, false)
		},
	}
	addServeFlags(cmd, rootOpts, true, true)

	runOpts := &serveOptions{}
	runCmd := &cobra.Command{
		Use:   "run [flags]",
		Short: locale.T("启动并打开浏览器，关闭所有页面后自动退出（默认模式）", "Start and open the browser; exit automatically when all tabs close (default)"),
		Long: locale.T(`启动本地 WebUI 服务并立即打开浏览器。
页面保持一条轻量的 /api/presence 连接；刷新页面（宽限时间内重连）不会触发退出，当所有标签页都关闭后自动优雅停机并退出。
传入 --no-open 或 --socket 时不会自动退出。`, `Start the local WebUI server and immediately open the browser.
Each open tab holds a lightweight /api/presence stream; page reloads survive the grace window, and closing the last
tab shuts the server down cleanly. Passing --no-open or --socket disables auto-exit.`),
		Example:      locale.T("  gbx serve run\n  gbx serve run -w ./repo -p deepseek", "  gbx serve run\n  gbx serve run -w ./repo -p deepseek"),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeServer(cmd, runOpts, args, webui.ServeModeRun, false)
		},
	}
	addServeFlags(runCmd, runOpts, true, true)

	startOpts := &serveOptions{}
	startCmd := &cobra.Command{
		Use:   "start [flags]",
		Short: locale.T("后台常驻启动 WebUI 服务（关闭浏览器不退出，需 gbx serve stop 停止）", "Start WebUI as a persistent background daemon (stop with gbx serve stop)"),
		Long: locale.T(`在后台启动独立的 gbx serve 守护进程并返回终端提示符。
关闭浏览器标签页不会停止服务；若已有受管实例在运行，会先平滑停止旧实例再启动新实例。
默认不自动打开浏览器（加 -o / --open 可同时打开），使用 gbx serve status 查看状态，gbx serve stop 停止。`, `Start a detached background gbx serve daemon and return immediately.
Closing the browser does not stop the server. If a managed instance is already running, it is stopped cleanly before
starting the new one. Pass -o / --open to also launch the browser. Use gbx serve status and gbx serve stop to manage it.`),
		Example:      locale.T("  gbx serve start\n  gbx serve start -o --port 8080\n  gbx serve start --socket", "  gbx serve start\n  gbx serve start -o --port 8080\n  gbx serve start --socket"),
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeStart(cmd, startOpts, args)
		},
	}
	addServeFlags(startCmd, startOpts, false, false)

	statusCmd := &cobra.Command{
		Use:          "status",
		Short:        locale.T("查看 gbx serve 运行状态", "Show gbx serve status"),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServeStatus(cmd)
		},
	}

	stopCmd := &cobra.Command{
		Use:          "stop",
		Short:        locale.T("停止正在运行的 gbx serve", "Stop the running gbx serve instance"),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServeStop(cmd)
		},
	}

	daemonOpts := &serveOptions{}
	daemonCmd := &cobra.Command{
		Use:          "_daemon",
		Hidden:       true,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeServer(cmd, daemonOpts, args, webui.ServeModeStart, true)
		},
	}
	addServeFlags(daemonCmd, daemonOpts, false, false)

	cmd.AddCommand(runCmd, startCmd, statusCmd, stopCmd, daemonCmd)
	return cmd
}

func validateServeOptions(cmd *cobra.Command, opts *serveOptions, args []string) (string, error) {
	socket, err := socketPath(cmd, opts.socket, args)
	if err != nil {
		return "", err
	}
	if cmd.Flags().Changed("sandbox") {
		if strings.TrimSpace(opts.sandbox) == "" {
			return "", fmt.Errorf("--sandbox: expected workspace or off")
		}
		if opts.sandbox, err = webui.NormalizeSandbox(opts.sandbox); err != nil {
			return "", err
		}
	}
	if opts.port < 0 || opts.port > 65535 {
		return "", fmt.Errorf("--port must be between 0 and 65535, got %d", opts.port)
	}
	if opts.exitGrace < 0 {
		return "", fmt.Errorf("--exit-grace must be >= 0, got %s", opts.exitGrace)
	}
	if len(opts.workspaces) == 0 && len(opts.workspaceRoots) == 0 {
		opts.workspaces = []string{"."}
	}
	if opts.basePath, err = webui.NormalizeBasePath(strings.TrimSpace(opts.basePath)); err != nil {
		return "", fmt.Errorf("--base-path: %w", err)
	}
	return socket, nil
}

func runServeServer(cmd *cobra.Command, opts *serveOptions, args []string, mode string, daemonMode bool) error {
	socket, err := validateServeOptions(cmd, opts, args)
	if err != nil {
		return err
	}

	if loaded, loadErr := config.Load(); loadErr == nil {
		oauthproxy.ConfigureLogLevel(configuredLogLevel(loaded.LogLevel))
		if _, ownsLog, logErr := oauthproxy.EnsureSessionLog("grok_serve"); logErr == nil && ownsLog {
			defer oauthproxy.CloseLog()
		}
	}

	token := strings.TrimSpace(opts.token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GBX_SERVE_TOKEN"))
	}
	// Remove server secrets from process env immediately so no child process
	// (ACP worker, git, shell, MCP) can ever inherit them.
	_ = os.Unsetenv("GBX_SERVE_TOKEN")
	instanceID := strings.TrimSpace(os.Getenv(webui.EnvServeInstanceID))
	_ = os.Unsetenv(webui.EnvServeInstanceID)

	generatedToken := false
	if socket == "" && token == "" {
		token = rand.Text()
		generatedToken = true
	}

	noOpenEnv := strings.TrimSpace(os.Getenv("GBX_SERVE_NO_OPEN")) == "1"
	willOpenBrowser := !daemonMode && socket == "" && opts.openBrowser && !opts.noOpen && !noOpenEnv
	enableAutoExit := mode == webui.ServeModeRun && willOpenBrowser && opts.autoExit
	exitGrace := opts.exitGrace
	if enableAutoExit && exitGrace <= 0 {
		exitGrace = webui.DefaultAutoExitGrace
	}

	sigCtx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	runCtx, cancelRun := context.WithCancel(sigCtx)
	defer cancelRun()

	var stopReason atomic.Int32
	triggerStop := func(reason int32) {
		stopReason.CompareAndSwap(serveStopReasonSignal, reason)
		cancelRun()
	}

	var (
		mgr               *webui.LifecycleManager
		releaseDaemonLock func()
	)
	if m, mgrErr := webui.DefaultLifecycleManager(); mgrErr == nil {
		if daemonMode {
			lock, lockErr := m.AcquireDaemonLock(runCtx)
			if lockErr != nil {
				return fmt.Errorf("acquire serve daemon lock: %w", lockErr)
			}
			mgr = m
			releaseDaemonLock = lock
		} else if socket == "" {
			if lock, ok, lockErr := m.TryLockDaemon(); lockErr == nil && ok {
				mgr = m
				releaseDaemonLock = lock
			}
		}
	}
	if releaseDaemonLock != nil {
		defer releaseDaemonLock()
	}

	cfg := webui.Config{
		Token:           token,
		RequireToken:    socket == "",
		Workspaces:      opts.workspaces,
		WorkspaceRoots:  opts.workspaceRoots,
		MaxConcurrency:  opts.maxConcurrency,
		DefaultProvider: opts.providerName,
		DefaultModel:    opts.model,
		DefaultEffort:   opts.effort,
		Version:         Version,
		SelfBinary:      selfBinary(),
		LatestVersion:   selfupdate.FetchLatestReleaseVersion,
		Shell:           opts.shell && !opts.noShell,
		LockWorkspaces:  opts.lockWorkspaces,
		BasePath:        opts.basePath,
		Sandbox:         opts.sandbox,
	}
	if enableAutoExit {
		cfg.AutoExitGrace = exitGrace
		cfg.OnAutoExit = func() { triggerStop(serveStopReasonBrowserClosed) }
	}
	server, err := webui.New(cfg)
	if err != nil {
		return err
	}
	var closeServerOnce sync.Once
	closeServer := func() {
		closeServerOnce.Do(func() {
			closeCtx, cancelClose := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancelClose()
			_ = server.Close(closeCtx)
		})
	}
	defer closeServer()

	errOut := cmd.ErrOrStderr()
	if server.SandboxMode() == webui.SandboxOff {
		fmt.Fprintln(errOut, locale.T(
			"警告：已为新对话选择关闭 Grok Build 操作系统沙盒，agent 可写入工作区外的文件并启动程序；工具审批单独控制。已有对话保留原模式，父进程限制和上游管理策略仍然有效。",
			"Warning: the Grok Build OS sandbox is requested off for new conversations. Agent processes can write outside the workspace and launch programs; tool approval is separate. Existing conversations keep their mode; inherited restrictions and managed policies still apply.",
		))
	}

	var (
		listener  net.Listener
		port      = opts.port
		baseURL   string
		launchURL string
	)
	if socket != "" {
		listener, err = webui.ListenUnix(socket)
		if err != nil {
			return err
		}
	} else {
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		listener, err = net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
			port = tcp.Port
		}
		baseURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + opts.basePath + "/"
		launchURL = baseURL
		if generatedToken {
			launchURL = baseURL + "#" + token
		}
	}

	if mgr != nil {
		state := &webui.DaemonState{
			ID:         instanceID,
			PID:        os.Getpid(),
			Mode:       mode,
			URL:        baseURL,
			Workspaces: server.WorkspaceCount(),
			AutoExit:   enableAutoExit,
			StartedAt:  time.Now().UTC(),
		}
		if socket != "" {
			state.Socket = listener.Addr().String()
		}
		closeControl, ctrlErr := webui.StartControlServer(state, server.PresenceCount, func() {
			triggerStop(serveStopReasonRemoteStop)
		})
		if ctrlErr != nil {
			_ = listener.Close()
			return ctrlErr
		}
		defer closeControl()
		if writeErr := mgr.WriteState(state); writeErr != nil {
			_ = listener.Close()
			return writeErr
		}
		defer mgr.RemoveStateIfOwner(state.ID)
	}

	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return base },
	}

	serveDone := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-runCtx.Done():
			stopSignals()
			if !daemonMode {
				switch stopReason.Load() {
				case serveStopReasonBrowserClosed:
					fmt.Fprint(errOut, locale.T("\n浏览器页面已关闭，正在停止 gbx serve…\n", "\nBrowser closed; stopping gbx serve…\n"))
				case serveStopReasonRemoteStop:
					fmt.Fprint(errOut, locale.T("\n收到停止指令，正在停止 gbx serve…\n", "\nStop requested; stopping gbx serve…\n"))
				default:
					fmt.Fprint(errOut, locale.T("\n正在停止 gbx serve…（再按一次 Ctrl+C 立即退出）\n", "\nStopping gbx serve… (Ctrl+C again to quit now)\n"))
				}
			}
			cancelBase()
			closeServer()
			httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelHTTP()
			_ = httpServer.Shutdown(httpCtx)
		case <-serveDone:
		}
	}()

	if !daemonMode {
		if socket != "" {
			ready, _ := json.Marshal(map[string]any{"socket": listener.Addr().String(), "pid": os.Getpid(), "version": Version})
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ready)
			fmt.Fprintf(errOut, locale.T(
				"gbx serve 已启动 · unix:%s · %d 个项目\n停止：Ctrl+C\n",
				"gbx serve listening · unix:%s · projects: %d\nStop with Ctrl+C\n",
			), listener.Addr().String(), server.WorkspaceCount())
		} else {
			if enableAutoExit {
				fmt.Fprintf(errOut, locale.T(
					"gbx serve 已启动 · %s · %d 个项目\n关闭浏览器页面后将自动退出（也可按 Ctrl+C 或执行 gbx serve stop）\n",
					"gbx serve listening · %s · projects: %d\nExits automatically when the browser closes (or press Ctrl+C / run gbx serve stop)\n",
				), launchURL, server.WorkspaceCount())
			} else {
				fmt.Fprintf(errOut, locale.T(
					"gbx serve 已启动 · %s · %d 个项目\n停止：Ctrl+C\n",
					"gbx serve listening · %s · projects: %d\nStop with Ctrl+C\n",
				), launchURL, server.WorkspaceCount())
			}
			if willOpenBrowser {
				if err := openServeBrowser(launchURL); err != nil {
					fmt.Fprintf(errOut, locale.T(
						"gbx: 无法打开浏览器（%v）；请手动打开 %s\n",
						"gbx: could not open a browser (%v); open %s yourself\n",
					), err, launchURL)
				}
			}
		}
	}

	err = httpServer.Serve(listener)
	close(serveDone)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		closeServer()
		<-shutdownDone
		return err
	}
	<-shutdownDone
	return nil
}

func runServeStart(cmd *cobra.Command, opts *serveOptions, args []string) error {
	socket, err := validateServeOptions(cmd, opts, args)
	if err != nil {
		return err
	}
	mgr, err := webui.DefaultLifecycleManager()
	if err != nil {
		return err
	}

	token := strings.TrimSpace(opts.token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GBX_SERVE_TOKEN"))
	}
	_ = os.Unsetenv("GBX_SERVE_TOKEN")
	generatedToken := false
	if socket == "" && token == "" {
		token = rand.Text()
		generatedToken = true
	}

	daemonArgs := []string{"serve", "_daemon"}
	if opts.port > 0 {
		daemonArgs = append(daemonArgs, "--port", strconv.Itoa(opts.port))
	}
	for _, ws := range opts.workspaces {
		abs, absErr := filepath.Abs(ws)
		if absErr != nil {
			abs = ws
		}
		daemonArgs = append(daemonArgs, "--workspace", abs)
	}
	for _, root := range opts.workspaceRoots {
		abs, absErr := filepath.Abs(root)
		if absErr != nil {
			abs = root
		}
		daemonArgs = append(daemonArgs, "--workspace-root", abs)
	}
	if opts.maxConcurrency > 0 {
		daemonArgs = append(daemonArgs, "--max-concurrency", strconv.Itoa(opts.maxConcurrency))
	}
	if strings.TrimSpace(opts.providerName) != "" {
		daemonArgs = append(daemonArgs, "--provider", opts.providerName)
	}
	if strings.TrimSpace(opts.model) != "" {
		daemonArgs = append(daemonArgs, "--model", opts.model)
	}
	if strings.TrimSpace(opts.effort) != "" {
		daemonArgs = append(daemonArgs, "--effort", opts.effort)
	}
	if !opts.shell || opts.noShell {
		daemonArgs = append(daemonArgs, "--no-shell")
	}
	if opts.lockWorkspaces {
		daemonArgs = append(daemonArgs, "--lock-workspaces")
	}
	if socket != "" {
		daemonArgs = append(daemonArgs, "--socket="+socket)
	}
	if cmd.Flags().Changed("sandbox") {
		daemonArgs = append(daemonArgs, "--sandbox", opts.sandbox)
	}
	if opts.basePath != "" {
		daemonArgs = append(daemonArgs, "--base-path", opts.basePath)
	}

	var extraEnv []string
	if token != "" {
		extraEnv = append(extraEnv, "GBX_SERVE_TOKEN="+token)
	}
	state, err := mgr.StartDetached(cmd.Context(), serveExecutable(), daemonArgs, extraEnv)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if state.Socket != "" {
		fmt.Fprintf(out, locale.T(
			"gbx serve 已在后台启动 · unix:%s · PID %d · %d 个项目\n停止：gbx serve stop · 日志：%s\n",
			"gbx serve started in background · unix:%s · PID %d · projects: %d\nStop: gbx serve stop · log: %s\n",
		), state.Socket, state.PID, state.Workspaces, mgr.LogPath())
		return nil
	}

	launchURL := state.URL
	if generatedToken && token != "" {
		launchURL = state.URL + "#" + token
	}
	fmt.Fprintf(out, locale.T(
		"gbx serve 已在后台启动 · %s · PID %d · %d 个项目\n关闭浏览器不会停止服务；停止：gbx serve stop · 日志：%s\n",
		"gbx serve started in background · %s · PID %d · projects: %d\nClosing the browser keeps it running; stop: gbx serve stop · log: %s\n",
	), launchURL, state.PID, state.Workspaces, mgr.LogPath())

	noOpenEnv := strings.TrimSpace(os.Getenv("GBX_SERVE_NO_OPEN")) == "1"
	if opts.openBrowser && !opts.noOpen && !noOpenEnv {
		if err := openServeBrowser(launchURL); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), locale.T(
				"gbx: 无法打开浏览器（%v）；请手动打开 %s\n",
				"gbx: could not open a browser (%v); open %s yourself\n",
			), err, launchURL)
		}
	}
	return nil
}

func runServeStatus(cmd *cobra.Command) error {
	mgr, err := webui.DefaultLifecycleManager()
	if err != nil {
		return err
	}
	status, err := mgr.Status(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if status == nil {
		fmt.Fprintln(out, locale.T("gbx serve 未运行", "gbx serve is not running"))
		return nil
	}
	endpoint := status.URL
	if status.Socket != "" {
		endpoint = "unix:" + status.Socket
	}
	modeLabel := locale.T("后台常驻 (start)", "background (start)")
	if status.Mode == webui.ServeModeRun {
		if status.AutoExit {
			modeLabel = locale.T("浏览器联动 (run，关闭页面自动退出)", "browser-coupled (run, auto-exit on close)")
		} else {
			modeLabel = locale.T("前台运行 (run)", "foreground (run)")
		}
	}
	fmt.Fprintf(out, locale.T(
		"gbx serve 运行中 · PID %d\n  模式：%s\n  地址：%s\n  项目：%d 个\n  已连接页面：%d 个\n",
		"gbx serve running · PID %d\n  Mode:     %s\n  Endpoint: %s\n  Projects: %d\n  Tabs:     %d\n",
	), status.PID, modeLabel, endpoint, status.Workspaces, status.Clients)
	return nil
}

func runServeStop(cmd *cobra.Command) error {
	mgr, err := webui.DefaultLifecycleManager()
	if err != nil {
		return err
	}
	stopped, prev, err := mgr.Stop(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if stopped && prev != nil {
		fmt.Fprintf(out, locale.T("已停止 gbx serve（PID %d）\n", "Stopped gbx serve (PID %d)\n"), prev.PID)
		return nil
	}
	if prev != nil {
		fmt.Fprintln(out, locale.T("已清理失效的 gbx serve 状态文件", "Cleaned up stale gbx serve state"))
		return nil
	}
	fmt.Fprintln(out, locale.T("gbx serve 未运行", "gbx serve is not running"))
	return nil
}

const defaultSocket = "default"

func socketPath(cmd *cobra.Command, socket string, args []string) (string, error) {
	if socket == defaultSocket && len(args) == 1 {
		socket, args = args[0], nil
	}
	if len(args) > 0 {
		return "", fmt.Errorf("unexpected argument %q", args[0])
	}
	if socket == "" {
		return "", nil
	}
	if cmd.Flags().Changed("port") {
		return "", errors.New("--socket cannot be combined with --port")
	}
	if socket == defaultSocket {
		return webui.DefaultSocketPath()
	}
	return socket, nil
}

func init() {
	rootCmd.AddCommand(newServeCommand())
}

func selfBinary() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
