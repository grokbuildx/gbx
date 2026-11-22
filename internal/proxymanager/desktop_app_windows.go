//go:build windows

package proxymanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type osDesktopController struct{}

func (osDesktopController) Resolve(choice DesktopApp) (*WatchedApp, bool, error) {
	for _, candidate := range desktopAppCandidatesWindows(choice, os.Getenv("LOCALAPPDATA"), filepath.Glob) {
		info, err := os.Stat(candidate.Path)
		if err != nil || info.IsDir() {
			continue
		}
		return candidate.Clone(), true, nil
	}
	return nil, false, nil
}

func windowsAppImageName(app *WatchedApp) string {
	if app == nil {
		return ""
	}
	if app.Path != "" {
		return filepath.Base(app.Path)
	}
	switch app.Kind {
	case DesktopAppChatGPT:
		return "ChatGPT.exe"
	case DesktopAppCodex:
		return "Codex.exe"
	default:
		return ""
	}
}

func (osDesktopController) IsRunning(ctx context.Context, app *WatchedApp) (bool, error) {
	image := windowsAppImageName(app)
	if image == "" {
		return false, nil
	}
	out, err := exec.CommandContext(ctx, "tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false, err
	}
	needle := `"` + strings.ToLower(image) + `"`
	return strings.Contains(strings.ToLower(string(out)), needle), nil
}

func (osDesktopController) Open(_ context.Context, app *WatchedApp) error {
	if !app.Valid() || app.Path == "" {
		return fmt.Errorf("desktop app executable path unavailable")
	}
	cmd := exec.Command(app.Path)
	detach(cmd)
	return cmd.Start()
}

func (osDesktopController) Quit(ctx context.Context, app *WatchedApp) error {
	image := windowsAppImageName(app)
	if image == "" {
		return fmt.Errorf("invalid desktop app")
	}
	// Omit /F so Windows sends a graceful WM_CLOSE rather than terminating abruptly.
	err := exec.CommandContext(ctx, "taskkill", "/IM", image).Run()
	if err != nil {
		if running, checkErr := (osDesktopController{}).IsRunning(ctx, app); checkErr == nil && !running {
			return nil
		}
		return err
	}
	return nil
}
