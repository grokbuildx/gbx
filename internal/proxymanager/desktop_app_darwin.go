//go:build darwin

package proxymanager

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type osDesktopController struct{}

func (osDesktopController) Resolve(choice DesktopApp) (*WatchedApp, bool, error) {
	home, _ := os.UserHomeDir()
	for _, candidate := range desktopAppCandidatesDarwin(choice, home) {
		info, err := os.Stat(candidate.Path)
		if err != nil || !info.IsDir() {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(candidate.Path); err == nil && resolved != "" {
			candidate.Path = resolved
		}
		if bid := readDarwinBundleID(candidate.Path); bid != "" {
			candidate.BundleID = bid
		}
		return candidate.Clone(), true, nil
	}
	return nil, false, nil
}

func readDarwinBundleID(appPath string) string {
	plist := filepath.Join(appPath, "Contents", "Info.plist")
	out, err := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :CFBundleIdentifier", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const darwinIsRunningJXA = `ObjC.import('AppKit');
function run(argv) {
  const bundleId = argv[0] || '';
  const appPath = argv[1] || '';
  const appName = argv[2] || '';
  const apps = $.NSWorkspace.sharedWorkspace.runningApplications;
  for (let i = 0; i < apps.count; i++) {
    const a = apps.objectAtIndex(i);
    if (a.terminated) continue;
    const bid = ObjC.unwrap(a.bundleIdentifier) || '';
    const burl = (a.bundleURL ? ObjC.unwrap(a.bundleURL.path) : '') || '';
    const name = ObjC.unwrap(a.localizedName) || '';
    if ((bundleId && bid === bundleId) || (appPath && burl === appPath) || (!bundleId && !appPath && appName && name === appName)) {
      return '1';
    }
  }
  return '0';
}
`

const darwinTerminateJXA = `ObjC.import('AppKit');
function run(argv) {
  const bundleId = argv[0] || '';
  const appPath = argv[1] || '';
  const appName = argv[2] || '';
  const apps = $.NSWorkspace.sharedWorkspace.runningApplications;
  for (let i = 0; i < apps.count; i++) {
    const a = apps.objectAtIndex(i);
    if (a.terminated) continue;
    const bid = ObjC.unwrap(a.bundleIdentifier) || '';
    const burl = (a.bundleURL ? ObjC.unwrap(a.bundleURL.path) : '') || '';
    const name = ObjC.unwrap(a.localizedName) || '';
    if ((bundleId && bid === bundleId) || (appPath && burl === appPath) || (!bundleId && !appPath && appName && name === appName)) {
      a.terminate;
    }
  }
  return 'ok';
}
`

func (osDesktopController) IsRunning(ctx context.Context, app *WatchedApp) (bool, error) {
	if !app.Valid() {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-", app.BundleID, app.Path, app.Name)
	cmd.Stdin = strings.NewReader(darwinIsRunningJXA)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false, err
	}
	return strings.TrimSpace(out.String()) == "1", nil
}

func (osDesktopController) Open(ctx context.Context, app *WatchedApp) error {
	if !app.Valid() {
		return fmt.Errorf("invalid desktop app")
	}
	target := app.Path
	if target == "" {
		target = app.Name
	}
	return exec.CommandContext(ctx, "/usr/bin/open", "-a", target).Run()
}

func (osDesktopController) Quit(ctx context.Context, app *WatchedApp) error {
	if !app.Valid() {
		return fmt.Errorf("invalid desktop app")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-", app.BundleID, app.Path, app.Name)
	cmd.Stdin = strings.NewReader(darwinTerminateJXA)
	return cmd.Run()
}
