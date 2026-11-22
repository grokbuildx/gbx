package proxymanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DesktopApp identifies a supported desktop client for Codex integration.
// ChatGPT and Codex are mutually exclusive targets; Auto prefers ChatGPT.
type DesktopApp string

const (
	DesktopAppAuto    DesktopApp = ""
	DesktopAppChatGPT DesktopApp = "chatgpt"
	DesktopAppCodex   DesktopApp = "codex"
)

// ErrDesktopAppQuitTimedOut indicates the running desktop app did not exit
// gracefully within the timeout; callers should keep the proxy running and let
// the user restart manually instead of force-killing the app.
var ErrDesktopAppQuitTimedOut = errors.New("desktop app did not exit gracefully before timeout")

// ParseDesktopApp validates a user-supplied desktop app target.
func ParseDesktopApp(raw string) (DesktopApp, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return DesktopAppAuto, nil
	case string(DesktopAppChatGPT):
		return DesktopAppChatGPT, nil
	case string(DesktopAppCodex):
		return DesktopAppCodex, nil
	default:
		return "", fmt.Errorf("invalid desktop app %q: choose chatgpt or codex", raw)
	}
}

// DisplayName returns the user-facing application name for a desktop app kind.
func (a DesktopApp) DisplayName() string {
	switch a {
	case DesktopAppChatGPT:
		return "ChatGPT"
	case DesktopAppCodex:
		return "Codex"
	default:
		return "ChatGPT / Codex"
	}
}

// WatchedApp describes a resolved desktop application bound to a running Codex
// proxy's lifecycle. It stores non-secret local metadata only.
type WatchedApp struct {
	Kind     DesktopApp `json:"kind"`
	Name     string     `json:"name"`
	Path     string     `json:"path,omitempty"`
	BundleID string     `json:"bundle_id,omitempty"`
}

func (a *WatchedApp) Valid() bool {
	if a == nil {
		return false
	}
	return (a.Kind == DesktopAppChatGPT || a.Kind == DesktopAppCodex) && strings.TrimSpace(a.Name) != ""
}

func (a *WatchedApp) Clone() *WatchedApp {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

// DesktopController encapsulates OS-specific desktop app discovery and
// lifecycle operations so callers and daemon loops remain testable off-platform.
type DesktopController interface {
	Resolve(choice DesktopApp) (*WatchedApp, bool, error)
	IsRunning(ctx context.Context, app *WatchedApp) (bool, error)
	Open(ctx context.Context, app *WatchedApp) error
	Quit(ctx context.Context, app *WatchedApp) error
}

// DefaultDesktopController returns the OS-native DesktopController.
func DefaultDesktopController() DesktopController {
	return osDesktopController{}
}

// DesktopAppDisabled reports whether desktop UI integration (app launch, menu
// prompts) is disabled for isolated tests or headless runs.
func DesktopAppDisabled() bool {
	return os.Getenv("GBX_PROXY_NO_APP") == "1" || os.Getenv("GBX_PROXY_NO_MENUBAR") == "1"
}

func orderedDesktopKinds(choice DesktopApp) []DesktopApp {
	switch choice {
	case DesktopAppChatGPT:
		return []DesktopApp{DesktopAppChatGPT}
	case DesktopAppCodex:
		return []DesktopApp{DesktopAppCodex}
	default:
		// ChatGPT and Codex are mutually exclusive; default prefers ChatGPT.
		return []DesktopApp{DesktopAppChatGPT, DesktopAppCodex}
	}
}

func desktopAppCandidatesDarwin(choice DesktopApp, home string) []WatchedApp {
	roots := []string{"/Applications"}
	if strings.TrimSpace(home) != "" {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	var out []WatchedApp
	for _, kind := range orderedDesktopKinds(choice) {
		var appBundle, name, bundleID string
		switch kind {
		case DesktopAppChatGPT:
			appBundle, name, bundleID = "ChatGPT.app", "ChatGPT", "com.openai.chat"
		case DesktopAppCodex:
			appBundle, name, bundleID = "Codex.app", "Codex", "com.openai.codex"
		default:
			continue
		}
		for _, root := range roots {
			out = append(out, WatchedApp{
				Kind:     kind,
				Name:     name,
				Path:     filepath.Join(root, appBundle),
				BundleID: bundleID,
			})
		}
	}
	return out
}

func desktopAppCandidatesWindows(choice DesktopApp, localAppData string, globFn func(string) ([]string, error)) []WatchedApp {
	if strings.TrimSpace(localAppData) == "" {
		return nil
	}
	if globFn == nil {
		globFn = filepath.Glob
	}
	var out []WatchedApp
	for _, kind := range orderedDesktopKinds(choice) {
		var name, exe, pkgGlob string
		switch kind {
		case DesktopAppChatGPT:
			name, exe, pkgGlob = "ChatGPT", "ChatGPT.exe", "OpenAI.ChatGPT*"
		case DesktopAppCodex:
			name, exe, pkgGlob = "Codex", "Codex.exe", "OpenAI.Codex*"
		default:
			continue
		}
		out = append(out, WatchedApp{
			Kind: kind,
			Name: name,
			Path: filepath.Join(localAppData, "Programs", name, exe),
		})
		matches, _ := globFn(filepath.Join(localAppData, "Microsoft", "WindowsApps", pkgGlob, exe))
		for _, match := range matches {
			out = append(out, WatchedApp{Kind: kind, Name: name, Path: match})
		}
		out = append(out, WatchedApp{
			Kind: kind,
			Name: name,
			Path: filepath.Join(localAppData, "Microsoft", "WindowsApps", exe),
		})
	}
	return out
}

// WaitAppRunning polls ctrl.IsRunning until the app reaches wantRunning or the
// timeout/context expires.
func WaitAppRunning(ctx context.Context, ctrl DesktopController, app *WatchedApp, wantRunning bool, timeout time.Duration) (bool, error) {
	if ctrl == nil || !app.Valid() {
		return false, nil
	}
	deadline := time.Now().Add(timeout)
	for {
		running, err := ctrl.IsRunning(ctx, app)
		if err == nil && running == wantRunning {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, err
		}
		wait := 150 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// OpenAndWatchDesktopApp opens the desktop app and attaches the running proxy's
// auto-stop watcher to it.
func OpenAndWatchDesktopApp(ctx context.Context, m *Manager, ctrl DesktopController, app *WatchedApp) error {
	if ctrl == nil || !app.Valid() {
		return fmt.Errorf("desktop app is not configured")
	}
	if err := ctrl.Open(ctx, app); err != nil {
		return fmt.Errorf("open %s: %w", app.Name, err)
	}
	seenRunning, _ := WaitAppRunning(ctx, ctrl, app, true, 8*time.Second)
	if m != nil {
		if err := m.WatchApp(ctx, app, seenRunning); err != nil {
			return err
		}
	}
	return nil
}

// GracefulRestartDesktopApp pauses auto-stop watching, asks the running desktop
// app to quit gracefully, reopens it, and reattaches auto-stop watching.
func GracefulRestartDesktopApp(ctx context.Context, m *Manager, ctrl DesktopController, app *WatchedApp) error {
	if ctrl == nil || !app.Valid() {
		return fmt.Errorf("desktop app is not configured")
	}
	if m != nil {
		if err := m.PauseAppWatch(ctx, 25*time.Second); err != nil {
			return fmt.Errorf("pause proxy app watch: %w", err)
		}
	}
	if err := ctrl.Quit(ctx, app); err != nil {
		return fmt.Errorf("quit %s: %w", app.Name, err)
	}
	exited, err := WaitAppRunning(ctx, ctrl, app, false, 8*time.Second)
	if err != nil && ctx.Err() != nil {
		return err
	}
	if !exited {
		return fmt.Errorf("%w: %s", ErrDesktopAppQuitTimedOut, app.Name)
	}
	if err := ctrl.Open(ctx, app); err != nil {
		return fmt.Errorf("reopen %s: %w", app.Name, err)
	}
	seenRunning, _ := WaitAppRunning(ctx, ctrl, app, true, 8*time.Second)
	if m != nil {
		if err := m.WatchApp(ctx, app, seenRunning); err != nil {
			return err
		}
	}
	return nil
}

type watchAppRequest struct {
	App          *WatchedApp `json:"app,omitempty"`
	SeenRunning  bool        `json:"seen_running,omitempty"`
	GraceSeconds int         `json:"grace_seconds,omitempty"`
}

type pauseWatchRequest struct {
	Seconds int `json:"seconds"`
}

type watchOpKind int

const (
	watchOpSet watchOpKind = iota
	watchOpPause
	watchOpExited
)

type watchOp struct {
	kind        watchOpKind
	app         *WatchedApp
	seenRunning bool
	grace       time.Duration
	pauseFor    time.Duration
	result      chan error
	shouldStop  chan bool
}

func postControlJSON(ctx context.Context, state *State, path string, payload any) error {
	if state == nil {
		return fmt.Errorf("Codex proxy is not running")
	}
	if err := control(ctx, state, http.MethodGet, "/health"); err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, state.ControlURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(data) != state.ID {
		return fmt.Errorf("proxy control %s failed (%d)", path, response.StatusCode)
	}
	return nil
}

// WatchApp binds the running Codex proxy's auto-stop lifecycle to app.
func (m *Manager) WatchApp(ctx context.Context, app *WatchedApp, seenRunning bool) error {
	if !app.Valid() {
		return fmt.Errorf("invalid desktop app watch target")
	}
	s, err := m.Read()
	if err != nil {
		return err
	}
	return postControlJSON(ctx, s, "/watch-app", watchAppRequest{
		App:          app.Clone(),
		SeenRunning:  seenRunning,
		GraceSeconds: 15,
	})
}

// ClearAppWatch detaches any desktop app auto-stop binding from the running
// Codex proxy so manually restarting the app does not stop the proxy.
func (m *Manager) ClearAppWatch(ctx context.Context) error {
	s, err := m.Read()
	if err != nil || s == nil {
		return err
	}
	if !m.Running(ctx, s) {
		return nil
	}
	return postControlJSON(ctx, s, "/watch-app", watchAppRequest{})
}

// PauseAppWatch suspends auto-stop checks for duration d so a graceful app
// restart does not stop the proxy while the app briefly exits.
func (m *Manager) PauseAppWatch(ctx context.Context, d time.Duration) error {
	s, err := m.Read()
	if err != nil || s == nil {
		return err
	}
	if !m.Running(ctx, s) {
		return nil
	}
	seconds := int(d / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 120 {
		seconds = 120
	}
	return postControlJSON(ctx, s, "/pause-watch", pauseWatchRequest{Seconds: seconds})
}

// NotifyAppExited tells the running Codex proxy that its watched desktop app
// terminated; the daemon double-checks pause windows and live status before
// stopping.
func (m *Manager) NotifyAppExited(ctx context.Context) error {
	s, err := m.Read()
	if err != nil || s == nil {
		return err
	}
	if !m.Running(ctx, s) {
		return nil
	}
	return postControlJSON(ctx, s, "/app-exited", nil)
}
