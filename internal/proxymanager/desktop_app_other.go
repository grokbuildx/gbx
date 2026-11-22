//go:build !darwin && !windows

package proxymanager

import (
	"context"
	"fmt"
)

type osDesktopController struct{}

func (osDesktopController) Resolve(DesktopApp) (*WatchedApp, bool, error) { return nil, false, nil }
func (osDesktopController) IsRunning(context.Context, *WatchedApp) (bool, error) {
	return false, nil
}
func (osDesktopController) Open(context.Context, *WatchedApp) error {
	return fmt.Errorf("desktop app launch is supported on macOS and Windows only")
}
func (osDesktopController) Quit(context.Context, *WatchedApp) error {
	return fmt.Errorf("desktop app restart is supported on macOS and Windows only")
}
