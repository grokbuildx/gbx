package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/fsutil"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/proxymanager"
	"github.com/spf13/cobra"
)

// Menu data contains names and non-secret local app metadata only. The UI must
// never receive provider keys, endpoints, account credentials, or the proxy's
// control token.
func proxyMenuConfig(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	current := cfg.ActiveProvider
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	var watched *proxymanager.WatchedApp
	if state, err := m.Read(); err != nil {
		return err
	} else if state != nil {
		current = state.Provider
		if state.WatchedApp.Valid() {
			watched = state.WatchedApp.Clone()
		}
	}
	var desktopApp *proxymanager.WatchedApp
	if watched != nil {
		desktopApp = watched.Clone()
	} else if !codexDesktopDisabled() {
		if resolved, found, err := codexDesktopController().Resolve(proxymanager.DesktopAppAuto); err == nil && found {
			desktopApp = resolved
		}
	}
	names := make([]string, 0, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if provider.IsDefaultProvider(name) || provider.ValidateRuntime(p) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		Current    string                   `json:"current"`
		Providers  []string                 `json:"providers"`
		WatchedApp *proxymanager.WatchedApp `json:"watched_app,omitempty"`
		DesktopApp *proxymanager.WatchedApp `json:"desktop_app,omitempty"`
	}{current, names, watched, desktopApp})
}

func resolveMenuDesktopApp(state *proxymanager.State, ctrl proxymanager.DesktopController) *proxymanager.WatchedApp {
	if state != nil && state.WatchedApp.Valid() {
		return state.WatchedApp.Clone()
	}
	if ctrl == nil {
		return nil
	}
	resolved, found, err := ctrl.Resolve(proxymanager.DesktopAppAuto)
	if err != nil || !found {
		return nil
	}
	return resolved
}

// A failed menu switch restores the previous running provider. Updating the
// selected configuration happens only after the replacement reports readiness.
func proxyMenuSwitch(cmd *cobra.Command, _ []string) (switchErr error) {
	defer func() {
		if switchErr != nil {
			proxymanager.ShowMenuError(switchErr.Error())
		}
	}()
	name, _ := cmd.Flags().GetString("provider")
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("select a provider")
	}
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(cmd.Context(), filepath.Join(m.Dir, "menu.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	target, ok := cfg.Providers[name]
	if !ok || provider.IsDefaultProvider(name) || provider.ValidateRuntime(target) != nil {
		return fmt.Errorf("select a provider supported by the standalone proxy")
	}
	previous, err := m.Read()
	if err != nil {
		return err
	}
	var rollback *provider.Provider
	if previous != nil && m.Running(cmd.Context(), previous) {
		if p, exists := cfg.Providers[previous.Provider]; exists {
			p.Name = previous.Provider
			rollback = &p
		}
	}
	if err := runProxyStart(cmd, nil); err != nil {
		if rollback != nil {
			executable, execErr := os.Executable()
			if execErr != nil {
				return errors.Join(err, execErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if previous.Codex != nil {
				endpoint, parseErr := url.Parse(previous.Codex.BaseURL)
				if parseErr != nil {
					return errors.Join(err, parseErr)
				}
				m.CodexListenAddress = endpoint.Host
			}
			m.CodexMode = previous.CodexMode
			m.CatalogMode = previous.CatalogMode
			m.AdditionalProviders = append([]string(nil), previous.AdditionalProviders...)
			_, restoreErr := m.Start(ctx, executable, *rollback)
			return errors.Join(err, restoreErr)
		}
		return err
	}
	if err := config.Update(func(current *provider.Config) error {
		state, err := m.Read()
		if err != nil {
			return err
		}
		if state == nil || state.Provider != name {
			return fmt.Errorf("proxy changed before its menu selection could be saved")
		}
		if _, exists := current.Providers[name]; !exists {
			return fmt.Errorf("selected provider was removed while switching")
		}
		current.ActiveProvider = name
		return nil
	}); err != nil {
		return err
	}
	if codexDesktopDisabled() {
		return nil
	}
	state, _ := m.Read()
	ctrl := codexDesktopController()
	app := resolveMenuDesktopApp(state, ctrl)
	if app == nil {
		return nil
	}
	opCtx, cancelOp := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancelOp()
	running, err := ctrl.IsRunning(opCtx, app)
	if err != nil {
		return nil
	}
	if running {
		confirmed, promptErr := codexMenuPromptAction(
			"GBX Codex",
			locale.Tf(
				"已切换到配置「%s」。需要温和重启 %s 以刷新模型列表。\n\n若选择「稍后」，之后可随时在状态栏点击「Restart %s」。",
				"Switched to %q. Restart %s gracefully to refresh the model list.\n\nIf you choose Later, you can click \"Restart %s\" from the menu bar anytime.",
				name, app.Name, app.Name,
			),
			locale.Tf("立即重启 %s", "Restart %s", app.Name),
			locale.T("稍后", "Later"),
		)
		if promptErr != nil || !confirmed {
			return nil
		}
		if err := proxymanager.GracefulRestartDesktopApp(opCtx, m, ctrl, app); err != nil {
			if errors.Is(err, proxymanager.ErrDesktopAppQuitTimedOut) {
				_ = m.ClearAppWatch(opCtx)
				return fmt.Errorf(locale.T(
					"未能自动关闭 %s（可能存在未确认的弹窗）。请手动重启 %s 以刷新模型列表。",
					"Could not close %s automatically (a confirmation prompt may be open). Restart %s manually to refresh the model list.",
				), app.Name, app.Name)
			}
			return err
		}
		return nil
	}
	confirmed, promptErr := codexMenuPromptAction(
		"GBX Codex",
		locale.Tf(
			"已切换到配置「%s」。是否立即打开 %s？退出应用后将自动停止代理并恢复原有配置。",
			"Switched to %q. Open %s now? Quitting the app will automatically stop the proxy and restore settings.",
			name, app.Name,
		),
		locale.Tf("打开 %s", "Open %s", app.Name),
		locale.T("稍后", "Later"),
	)
	if promptErr != nil || !confirmed {
		return nil
	}
	return proxymanager.OpenAndWatchDesktopApp(opCtx, m, ctrl, app)
}

func proxyMenuRestart(cmd *cobra.Command, _ []string) (restartErr error) {
	defer func() {
		if restartErr != nil {
			proxymanager.ShowMenuError(restartErr.Error())
		}
	}()
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(cmd.Context(), filepath.Join(m.Dir, "menu.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := m.Read()
	if err != nil || state == nil {
		return err
	}
	ctrl := codexDesktopController()
	app := resolveMenuDesktopApp(state, ctrl)
	if app == nil {
		return errors.New(locale.T("未检测到 ChatGPT / Codex 桌面应用", "no ChatGPT or Codex desktop app found"))
	}
	opCtx, cancelOp := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancelOp()
	running, err := ctrl.IsRunning(opCtx, app)
	if err != nil {
		return err
	}
	if running {
		if err := proxymanager.GracefulRestartDesktopApp(opCtx, m, ctrl, app); err != nil {
			if errors.Is(err, proxymanager.ErrDesktopAppQuitTimedOut) {
				_ = m.ClearAppWatch(opCtx)
				return fmt.Errorf(locale.T(
					"未能自动关闭 %s（可能存在未确认的弹窗）。请手动重启 %s。",
					"Could not close %s automatically (a confirmation prompt may be open). Restart %s manually.",
				), app.Name, app.Name)
			}
			return err
		}
		return nil
	}
	return proxymanager.OpenAndWatchDesktopApp(opCtx, m, ctrl, app)
}

func proxyMenuAppExited(cmd *cobra.Command, _ []string) error {
	m, err := proxymanager.Default()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	return m.NotifyAppExited(ctx)
}

func addProxyMenuCommands(command *cobra.Command) {
	command.AddCommand(
		&cobra.Command{Use: "menu-configs", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuConfig},
		&cobra.Command{Use: "menu-switch", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuSwitch},
		&cobra.Command{Use: "menu-restart", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuRestart},
		&cobra.Command{Use: "menu-app-exited", Hidden: true, Args: cobra.NoArgs, RunE: proxyMenuAppExited},
	)
}
