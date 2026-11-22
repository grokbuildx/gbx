package proxymanager

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

//go:embed menubar.jxa
var menuBarScript string

// The helper owns AppKit's event loop. Watching authenticated readiness also
// removes the icon when the daemon is killed without running its defers.
func (m *Manager) RunMenuBar(ctx context.Context, id string) error {
	s, err := m.Read()
	if err != nil {
		return err
	}
	if s == nil || s.ID != id || !m.Running(ctx, s) {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", executable, s.Provider)
	command.Stdin = strings.NewReader(menuBarScript)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() { _ = command.Process.Kill(); <-done }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			done <- err
			return err
		case <-ticker.C:
			current, err := m.Read()
			if err != nil || current == nil || current.ID != id || !m.Running(ctx, current) {
				return nil
			}
		}
	}
}

func (m *Manager) startMenuBar(id string) func() {
	if os.Getenv("GBX_PROXY_NO_MENUBAR") == "1" {
		return func() {}
	}
	executable, err := os.Executable()
	if err != nil {
		return func() {}
	}
	command := exec.Command(executable, "codex", "menubar", "--instance", id)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "gbx: menu bar unavailable:", err)
		return func() {}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	}
}

func ShowMenuError(message string) {
	if DesktopAppDisabled() {
		return
	}
	script := `function run(argv) { const app = Application.currentApplication(); app.includeStandardAdditions = true; app.displayAlert('GBX Codex', {message: argv[0], as: 'critical'}); }`
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", message)
	command.Stdin = strings.NewReader(script)
	_ = command.Run()
}

// PromptMenuAction presents a native macOS two-button confirmation alert and
// reports whether the user chose primaryButton.
func PromptMenuAction(title, message, primaryButton, secondaryButton string) (bool, error) {
	if DesktopAppDisabled() {
		return false, nil
	}
	script := `function run(argv) {
  const app = Application.currentApplication();
  app.includeStandardAdditions = true;
  try {
    const res = app.displayAlert(argv[0], {
      message: argv[1],
      buttons: [argv[2], argv[3]],
      defaultButton: argv[2],
      cancelButton: argv[3]
    });
    return res.buttonReturned === argv[2] ? '1' : '0';
  } catch (_) {
    return '0';
  }
}`
	command := exec.Command("/usr/bin/osascript", "-l", "JavaScript", "-", title, message, primaryButton, secondaryButton)
	command.Stdin = strings.NewReader(script)
	out, err := command.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}
