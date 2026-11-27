package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/mcpserver"
	"github.com/grokbuildx/gbx/internal/process"
	"github.com/spf13/cobra"
)

type mcpServerOptions struct {
	workspaces     []string
	workspaceRoots []string
	lockWorkspaces bool
	noExec         bool
	batchOnly      bool
	worktreeDir    string
	configPath     string
	save           bool
}

func newMCPServerCommand() *cobra.Command {
	opts := &mcpServerOptions{}
	cmd := &cobra.Command{
		Use:   "mcp-server [flags]",
		Short: locale.T("通过 MCP stdio 暴露本地编码工具（无需配置大模型）", "Expose local workspace coding tools over MCP stdio (no LLM configuration needed)"),
		Long: locale.T(`启动基于标准输入输出（stdio）的本地 MCP 编码服务器，让外部大模型客户端（如 macOS Gemini App、Claude Desktop、Gemini CLI 等）直接安全地浏览、搜索、修改项目代码、管理隔离 Git Worktree 并执行构建与测试，无需在 gbx 中配置任何大模型或账号。

工作区发现与持久化规则：
  • 自动合并命令行 --workspace / --workspace-root、环境变量 GBX_MCP_WORKSPACES / GBX_MCP_WORKSPACE_ROOTS、持久化配置 ~/.gbx/mcp.json 以及 MCP 客户端的 roots/list。
  • --workspace-root 会自动识别直接子仓库以及二级分组目录（如 ~/Space/GitHub/Swifty/*）下的 Git 子仓库。
  • 未开启 --lock-workspaces 时，宿主模型也可直接调用 workspace_add 工具注册已关联目录并自动保存到 ~/.gbx/mcp.json；或执行一次 gbx mcp-server --workspace-root <dir> --save 预先写入配置。`, `Start a local Model Context Protocol (MCP) coding server over stdio so external AI hosts (such as the macOS Gemini App, Claude Desktop, or Gemini CLI) can safely search, read, edit, manage isolated Git worktrees, and run builds/tests in your repositories without configuring any LLM provider in gbx.

Workspace discovery & persistence:
  • Merges CLI --workspace / --workspace-root flags, GBX_MCP_WORKSPACES / GBX_MCP_WORKSPACE_ROOTS environment variables, ~/.gbx/mcp.json, and MCP client roots/list folders.
  • --workspace-root automatically expands direct repositories as well as 2-level nested Git repositories under grouping directories (such as ~/Space/GitHub/Swifty/*).
  • When --lock-workspaces is not set, the host LLM can also call workspace_add to register a folder and persist it to ~/.gbx/mcp.json, or you can pre-save defaults once with gbx mcp-server --workspace-root <dir> --save.`),
		Example: locale.T(`  gbx mcp-server --workspace-root ~/Space/GitHub --save  # 保存默认代码总目录到 ~/.gbx/mcp.json（之后零参数启动即可）
  gbx mcp-server                                         # 零参数启动（自动加载 ~/.gbx/mcp.json 或由宿主调用 workspace_add）
  gbx mcp-server -w ~/proj --lock-workspaces             # 仅开放指定项目目录并锁定（忽略 ~/.gbx/mcp.json 与动态添加）
  gbx mcp-server -w ~/proj --no-exec                     # 仅开放文件检索、编辑与 Git 工具，禁用命令执行`, `  gbx mcp-server --workspace-root ~/Space/GitHub --save  # Save default workspace root to ~/.gbx/mcp.json
  gbx mcp-server                                         # Start with zero args (reads ~/.gbx/mcp.json or accepts workspace_add)
  gbx mcp-server -w ~/proj --lock-workspaces             # Serve and lock a single repository
  gbx mcp-server -w ~/proj --no-exec                     # Expose file, search, and Git tools without command execution`),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = os.Unsetenv("GBX_SERVE_TOKEN")
			_ = os.Unsetenv("GBX_SERVE_INSTANCE_ID")

			if opts.save {
				if len(opts.workspaces) == 0 && len(opts.workspaceRoots) == 0 && !opts.batchOnly {
					return fmt.Errorf("%s", locale.T(
						"--save 需要配合 --workspace、--workspace-root 或 --batch-only",
						"--save requires --workspace, --workspace-root, or --batch-only",
					))
				}
				if len(opts.workspaces) > 0 || len(opts.workspaceRoots) > 0 {
					if _, err := mcpserver.New(mcpserver.Config{
						Workspaces:     opts.workspaces,
						WorkspaceRoots: opts.workspaceRoots,
						LockWorkspaces: true,
					}); err != nil {
						return err
					}
				}
				savedPath, savedCfg, err := mcpserver.SavePersistedConfig(opts.configPath, mcpserver.PersistedConfig{
					Workspaces:     opts.workspaces,
					WorkspaceRoots: opts.workspaceRoots,
					BatchOnly:      opts.batchOnly,
				}, true)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", locale.T(
					fmt.Sprintf("已保存 MCP 默认配置至 %s（workspaces: %d, workspace_roots: %d, batch_only: %v）", savedPath, len(savedCfg.Workspaces), len(savedCfg.WorkspaceRoots), savedCfg.BatchOnly),
					fmt.Sprintf("Saved MCP defaults to %s (workspaces: %d, workspace_roots: %d, batch_only: %v)", savedPath, len(savedCfg.Workspaces), len(savedCfg.WorkspaceRoots), savedCfg.BatchOnly),
				))
				return nil
			}

			srv, err := mcpserver.New(mcpserver.Config{
				Workspaces:     opts.workspaces,
				WorkspaceRoots: opts.workspaceRoots,
				LockWorkspaces: opts.lockWorkspaces,
				DisableExec:    opts.noExec,
				BatchOnly:      opts.batchOnly,
				WorktreeDir:    opts.worktreeDir,
				ConfigPath:     opts.configPath,
				Version:        Version,
			})
			if err != nil {
				return err
			}
			baseCtx := cmd.Context()
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			ctx, stop := process.SignalContext(baseCtx)
			defer stop()
			return srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&opts.workspaces, "workspace", "w", nil, locale.T("允许操作的项目目录（可重复指定）", "Project directory to expose (repeatable)"))
	flags.StringArrayVar(&opts.workspaceRoots, "workspace-root", nil, locale.T("将该目录下的直接子仓库及二级分组 Git 仓库作为项目开放（可重复指定）", "Expose direct repositories and 2-level nested Git repos under this directory (repeatable)"))
	flags.BoolVar(&opts.lockWorkspaces, "lock-workspaces", false, locale.T("锁定工作区列表（仅使用本次命令行目录，忽略 ~/.gbx/mcp.json、roots/list 与 workspace_add）", "Serve only CLI --workspace / --workspace-root folders; ignore ~/.gbx/mcp.json, roots/list, and workspace_add"))
	flags.BoolVar(&opts.noExec, "no-exec", false, locale.T("禁用 run_command 与 worktree 创建/合并工具（仅开放文件读写、搜索与基础 Git）", "Disable run_command and worktree tools (expose file, search, and basic Git tools only)"))
	flags.BoolVar(&opts.batchOnly, "batch-only", false, locale.T("仅向宿主暴露 run_batch 单一工具，强制将多步操作合并为单次授权", "Advertise only run_batch in tools/list so hosts with per-call prompts approve once per multi-step batch"))
	flags.StringVar(&opts.worktreeDir, "worktree-dir", "", locale.T("隔离 Git Worktree 的存放根目录（默认 ~/.gbx/serve）", "Base directory for managed Git worktrees (default: ~/.gbx/serve)"))
	flags.StringVar(&opts.configPath, "config", "", locale.T("MCP 工作区配置文件路径（默认 ~/.gbx/mcp.json）", "Path to MCP workspace config file (default: ~/.gbx/mcp.json)"))
	flags.BoolVar(&opts.save, "save", false, locale.T("将本次指定的 --workspace / --workspace-root 保存到 ~/.gbx/mcp.json 后退出", "Save --workspace / --workspace-root to ~/.gbx/mcp.json and exit"))
	return cmd
}

func init() {
	rootCmd.AddCommand(newMCPServerCommand())
}
