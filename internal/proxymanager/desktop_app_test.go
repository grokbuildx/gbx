package proxymanager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/provider"
)

type fakeDesktopController struct {
	mu       sync.Mutex
	resolved map[DesktopApp]*WatchedApp
	running  bool
	events   []string
	quitHook func()
	openHook func()
}

func (f *fakeDesktopController) Resolve(choice DesktopApp) (*WatchedApp, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if choice != DesktopAppAuto {
		app := f.resolved[choice]
		return app.Clone(), app != nil, nil
	}
	for _, kind := range orderedDesktopKinds(DesktopAppAuto) {
		if app := f.resolved[kind]; app != nil {
			return app.Clone(), true, nil
		}
	}
	return nil, false, nil
}

func (f *fakeDesktopController) IsRunning(_ context.Context, _ *WatchedApp) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeDesktopController) Open(_ context.Context, app *WatchedApp) error {
	f.mu.Lock()
	f.events = append(f.events, "open:"+string(app.Kind))
	hook := f.openHook
	if hook == nil {
		f.running = true
	}
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeDesktopController) Quit(_ context.Context, app *WatchedApp) error {
	f.mu.Lock()
	f.events = append(f.events, "quit:"+string(app.Kind))
	hook := f.quitHook
	if hook == nil {
		f.running = false
	}
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeDesktopController) setRunning(v bool) {
	f.mu.Lock()
	f.running = v
	f.mu.Unlock()
}

func (f *fakeDesktopController) snapshotEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestParseDesktopApp(t *testing.T) {
	cases := map[string]DesktopApp{
		"":        DesktopAppAuto,
		"auto":    DesktopAppAuto,
		"AUTO":    DesktopAppAuto,
		"chatgpt": DesktopAppChatGPT,
		"ChatGPT": DesktopAppChatGPT,
		"codex":   DesktopAppCodex,
		"CODEX":   DesktopAppCodex,
	}
	for raw, want := range cases {
		got, err := ParseDesktopApp(raw)
		if err != nil || got != want {
			t.Fatalf("ParseDesktopApp(%q) = (%q, %v), want (%q, nil)", raw, got, err, want)
		}
	}
	if _, err := ParseDesktopApp("safari"); err == nil {
		t.Fatal("expected error for unsupported desktop app")
	}
}

func TestDesktopAppCandidatesPreferChatGPTAndStayMutuallyExclusive(t *testing.T) {
	darwinAuto := desktopAppCandidatesDarwin(DesktopAppAuto, "/Users/tester")
	if len(darwinAuto) != 4 {
		t.Fatalf("darwin auto candidates = %d, want 4", len(darwinAuto))
	}
	if darwinAuto[0].Kind != DesktopAppChatGPT || darwinAuto[1].Kind != DesktopAppChatGPT ||
		darwinAuto[2].Kind != DesktopAppCodex || darwinAuto[3].Kind != DesktopAppCodex {
		t.Fatalf("darwin auto did not prefer ChatGPT first: %+v", darwinAuto)
	}
	for _, c := range desktopAppCandidatesDarwin(DesktopAppChatGPT, "/Users/tester") {
		if c.Kind != DesktopAppChatGPT {
			t.Fatalf("chatgpt choice included non-chatgpt candidate: %+v", c)
		}
	}
	for _, c := range desktopAppCandidatesDarwin(DesktopAppCodex, "/Users/tester") {
		if c.Kind != DesktopAppCodex {
			t.Fatalf("codex choice included non-codex candidate: %+v", c)
		}
	}

	winAuto := desktopAppCandidatesWindows(DesktopAppAuto, `C:\Users\tester\AppData\Local`, func(string) ([]string, error) { return nil, nil })
	if len(winAuto) != 4 {
		t.Fatalf("windows auto candidates = %d, want 4", len(winAuto))
	}
	if winAuto[0].Kind != DesktopAppChatGPT || winAuto[1].Kind != DesktopAppChatGPT ||
		winAuto[2].Kind != DesktopAppCodex || winAuto[3].Kind != DesktopAppCodex {
		t.Fatalf("windows auto did not prefer ChatGPT first: %+v", winAuto)
	}
	for _, c := range desktopAppCandidatesWindows(DesktopAppCodex, `C:\Users\tester\AppData\Local`, func(string) ([]string, error) { return nil, nil }) {
		if c.Kind != DesktopAppCodex {
			t.Fatalf("windows codex choice included non-codex candidate: %+v", c)
		}
	}
}

func startTestDaemonWithDesktop(t *testing.T, ctrl DesktopController, pollInterval time.Duration) (*Manager, string, <-chan struct{}, *error) {
	t.Helper()
	m, _ := isolatedManager(t)
	m.Desktop = ctrl
	m.WatchPollInterval = pollInterval
	configPath := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	originalConfig := "# user config\nmodel = 'native-fixture'\n"
	if err := os.WriteFile(configPath, []byte(originalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	p := provider.Provider{Name: "chosen", Type: "openai", Endpoint: upstream.URL + "/v1", APIKey: "fake-key", Model: "model-a"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = m.Run(ctx, p, "desktop-watch-fixture")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not exit")
		}
	})
	_ = awaitRunning(t, m)
	return m, originalConfig, done, &runErr
}

func assertConfigRestored(t *testing.T, originalConfig string) {
	t.Helper()
	gotConfig, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil {
		t.Fatalf("read restored config.toml: %v", err)
	}
	if strings.TrimSpace(string(gotConfig)) != strings.TrimSpace(originalConfig) ||
		strings.Contains(string(gotConfig), "openai_base_url") ||
		strings.Contains(string(gotConfig), "model_catalog_json") {
		t.Fatalf("config.toml not restored after auto-stop: %q", string(gotConfig))
	}
}

func TestProxyAutoStopsWhenWatchedDesktopAppQuits(t *testing.T) {
	chatGPT := &WatchedApp{Kind: DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	ctrl := &fakeDesktopController{resolved: map[DesktopApp]*WatchedApp{DesktopAppChatGPT: chatGPT}}
	m, originalConfig, done, runErr := startTestDaemonWithDesktop(t, ctrl, 40*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := OpenAndWatchDesktopApp(ctx, m, ctrl, chatGPT); err != nil {
		t.Fatal(err)
	}
	state, err := m.Read()
	if err != nil || state == nil || !state.WatchedApp.Valid() || state.WatchedApp.Kind != DesktopAppChatGPT {
		t.Fatalf("watched app not persisted in state: %+v err=%v", state, err)
	}

	// Quitting ChatGPT should automatically shut down the proxy and restore config.toml.
	ctrl.setRunning(false)
	select {
	case <-done:
		if *runErr != nil {
			t.Fatalf("daemon returned error on auto-stop: %v", *runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not auto-stop after watched app exited")
	}

	assertConfigRestored(t, originalConfig)
	if _, err := os.Stat(m.statePath()); !os.IsNotExist(err) {
		t.Fatalf("state.json survived auto-stop: %v", err)
	}
}

func TestProxyPauseWatchProtectsGracefulRestartThenAutoStops(t *testing.T) {
	chatGPT := &WatchedApp{Kind: DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	var notifiedDuringQuit atomic.Bool
	ctrl := &fakeDesktopController{
		resolved: map[DesktopApp]*WatchedApp{DesktopAppChatGPT: chatGPT},
		running:  true,
	}
	// Use a long fallback poll so this test specifically verifies NotifyAppExited
	// is suppressed during PauseAppWatch and immediately triggers stop afterwards.
	m, originalConfig, done, runErr := startTestDaemonWithDesktop(t, ctrl, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WatchApp(ctx, chatGPT, true); err != nil {
		t.Fatal(err)
	}

	ctrl.quitHook = func() {
		ctrl.setRunning(false)
		// Simulate macOS NSWorkspace termination event arriving while restarting.
		if err := m.NotifyAppExited(ctx); err == nil {
			notifiedDuringQuit.Store(true)
		}
		time.Sleep(120 * time.Millisecond)
	}

	if err := GracefulRestartDesktopApp(ctx, m, ctrl, chatGPT); err != nil {
		t.Fatalf("GracefulRestartDesktopApp failed: %v", err)
	}
	if !notifiedDuringQuit.Load() {
		t.Fatal("expected NotifyAppExited during restart pause window")
	}
	if events := ctrl.snapshotEvents(); !slices.Equal(events, []string{"quit:chatgpt", "open:chatgpt"}) {
		t.Fatalf("unexpected restart sequence: %v", events)
	}

	state, err := m.Read()
	if err != nil || !m.Running(ctx, state) {
		t.Fatalf("proxy stopped unexpectedly during graceful restart: state=%+v err=%v", state, err)
	}

	// After restart completes, a real quit immediately shuts down via NotifyAppExited.
	ctrl.setRunning(false)
	if err := m.NotifyAppExited(ctx); err != nil {
		t.Fatalf("NotifyAppExited after restart: %v", err)
	}
	select {
	case <-done:
		if *runErr != nil {
			t.Fatalf("daemon returned error after real exit: %v", *runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after post-restart app exit")
	}
	assertConfigRestored(t, originalConfig)
}

func TestProxyClearAppWatchAllowsManualRestart(t *testing.T) {
	chatGPT := &WatchedApp{Kind: DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	ctrl := &fakeDesktopController{
		resolved: map[DesktopApp]*WatchedApp{DesktopAppChatGPT: chatGPT},
		running:  true,
	}
	m, _, _, _ := startTestDaemonWithDesktop(t, ctrl, 40*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WatchApp(ctx, chatGPT, true); err != nil {
		t.Fatal(err)
	}
	if err := m.ClearAppWatch(ctx); err != nil {
		t.Fatal(err)
	}
	ctrl.setRunning(false)
	if err := m.NotifyAppExited(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(160 * time.Millisecond)

	state, err := m.Read()
	if err != nil || !m.Running(ctx, state) {
		t.Fatalf("proxy stopped even after ClearAppWatch: state=%+v err=%v", state, err)
	}
	if state.WatchedApp != nil {
		t.Fatalf("WatchedApp should be nil after clear: %+v", state.WatchedApp)
	}
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGracefulRestartQuitTimeoutDoesNotOpenOrForceKill(t *testing.T) {
	chatGPT := &WatchedApp{Kind: DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	ctrl := &fakeDesktopController{
		resolved: map[DesktopApp]*WatchedApp{DesktopAppChatGPT: chatGPT},
		running:  true,
		quitHook: func() {
			// App refuses to quit (e.g., modal save sheet open).
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := GracefulRestartDesktopApp(ctx, nil, ctrl, chatGPT)
	if err == nil {
		t.Fatal("expected timeout error when app does not quit")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrDesktopAppQuitTimedOut) {
		t.Fatalf("unexpected error: %v", err)
	}
	if events := ctrl.snapshotEvents(); !slices.Equal(events, []string{"quit:chatgpt"}) {
		t.Fatalf("expected only quit attempt, got %v", events)
	}
	if strings.Contains(strings.Join(ctrl.snapshotEvents(), ","), "open:") {
		t.Fatal("should not reopen when quit did not complete")
	}
}
