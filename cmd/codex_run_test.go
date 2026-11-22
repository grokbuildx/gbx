package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/codex"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/proxymanager"
)

type mockCodexDesktop struct {
	mu       sync.Mutex
	apps     map[proxymanager.DesktopApp]*proxymanager.WatchedApp
	running  bool
	events   []string
	quitStay bool
}

func (m *mockCodexDesktop) Resolve(choice proxymanager.DesktopApp) (*proxymanager.WatchedApp, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if choice != proxymanager.DesktopAppAuto {
		app := m.apps[choice]
		return app.Clone(), app != nil, nil
	}
	if app := m.apps[proxymanager.DesktopAppChatGPT]; app != nil {
		return app.Clone(), true, nil
	}
	if app := m.apps[proxymanager.DesktopAppCodex]; app != nil {
		return app.Clone(), true, nil
	}
	return nil, false, nil
}

func (m *mockCodexDesktop) IsRunning(context.Context, *proxymanager.WatchedApp) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, nil
}

func (m *mockCodexDesktop) Open(_ context.Context, app *proxymanager.WatchedApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "open:"+string(app.Kind))
	m.running = true
	return nil
}

func (m *mockCodexDesktop) Quit(_ context.Context, app *proxymanager.WatchedApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, "quit:"+string(app.Kind))
	if !m.quitStay {
		m.running = false
	}
	return nil
}

func (m *mockCodexDesktop) snapshotEvents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

func startFakeControlServer(t *testing.T, statePath string, stateID string, watched **proxymanager.WatchedApp) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-control-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			_, _ = io.WriteString(w, stateID)
		case r.Method == http.MethodPost && r.URL.Path == "/watch-app":
			var req struct {
				App *proxymanager.WatchedApp `json:"app"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			if req.App == nil {
				calls = append(calls, "watch:clear")
				if watched != nil {
					*watched = nil
				}
			} else {
				calls = append(calls, "watch:"+string(req.App.Kind))
				if watched != nil {
					*watched = req.App.Clone()
				}
			}
			mu.Unlock()
			if statePath != "" {
				if raw, err := os.ReadFile(statePath); err == nil {
					var st proxymanager.State
					if json.Unmarshal(raw, &st) == nil {
						st.WatchedApp = req.App.Clone()
						if next, err := json.Marshal(st); err == nil {
							_ = os.WriteFile(statePath, next, 0o600)
						}
					}
				}
			}
			_, _ = io.WriteString(w, stateID)
		case r.Method == http.MethodPost && r.URL.Path == "/pause-watch":
			mu.Lock()
			calls = append(calls, "pause-watch")
			mu.Unlock()
			_, _ = io.WriteString(w, stateID)
		case r.Method == http.MethodPost && r.URL.Path == "/app-exited":
			mu.Lock()
			calls = append(calls, "app-exited")
			mu.Unlock()
			_, _ = io.WriteString(w, stateID)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func setupCodexRunTestEnv(t *testing.T, desktop *mockCodexDesktop) (*proxymanager.Manager, *[]string, *[]string) {
	t.Helper()
	beforeLang := locale.Current()
	locale.SetLanguage("en")
	t.Cleanup(func() { locale.SetLanguage(beforeLang) })

	saveGrokFixture(t, &provider.Config{
		ActiveProvider: "work",
		Providers: map[string]provider.Provider{
			"work":  {Name: "work", Type: "openai", Endpoint: "https://example.invalid/v1", APIKey: "k", Model: "m1"},
			"extra": {Name: "extra", Type: "openai", Endpoint: "https://example.invalid/v1", APIKey: "k2", Model: "m2"},
		},
	})
	codexHome := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	m, err := proxymanager.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(m.Dir, "state.json")
	var currentWatched *proxymanager.WatchedApp
	controlSrv, controlCalls := startFakeControlServer(t, statePath, "fake-instance-id", &currentWatched)

	var startRecord []string
	origDesktop := codexDesktopController
	origDisabled := codexDesktopDisabled
	origPrompt := codexMenuPromptAction
	origInteractive := codexIsInteractiveInput
	origStart := startCodexProxy
	t.Cleanup(func() {
		codexDesktopController = origDesktop
		codexDesktopDisabled = origDisabled
		codexMenuPromptAction = origPrompt
		codexIsInteractiveInput = origInteractive
		startCodexProxy = origStart
	})

	codexDesktopController = func() proxymanager.DesktopController { return desktop }
	codexDesktopDisabled = func() bool { return false }
	codexIsInteractiveInput = func(io.Reader) bool { return false }
	startCodexProxy = func(_ context.Context, mgr *proxymanager.Manager, _ string, p provider.Provider) (*proxymanager.State, error) {
		mode := mgr.CodexMode
		if mode == "" {
			mode = "shared"
		}
		cat := mgr.CatalogMode
		if cat == "" {
			cat = codex.CatalogLocal
		}
		startRecord = append(startRecord, p.Name+"|"+mode+"|"+string(cat)+"|"+strings.Join(mgr.AdditionalProviders, ","))
		st := &proxymanager.State{
			ID:                  "fake-instance-id",
			ProxyVersion:        proxymanager.CurrentProxyVersion,
			Provider:            p.Name,
			PID:                 os.Getpid(),
			BaseURL:             "http://127.0.0.1:43210/v1",
			ControlURL:          controlSrv.URL,
			ControlToken:        "test-control-token",
			CodexMode:           mode,
			CatalogMode:         cat,
			AdditionalProviders: append([]string(nil), mgr.AdditionalProviders...),
			CodexHome:           codexHome,
			Codex:               &codex.Overlay{Path: filepath.Join(codexHome, "config.toml"), BaseURL: "http://127.0.0.1:43210/v1"},
			WatchedApp:          currentWatched.Clone(),
		}
		raw, _ := json.Marshal(st)
		if err := os.WriteFile(statePath, raw, 0o600); err != nil {
			return nil, err
		}
		return st, nil
	}
	return m, controlCalls, &startRecord
}

func TestCodexShortFlagsAndAliases(t *testing.T) {
	chatGPT := &proxymanager.WatchedApp{Kind: proxymanager.DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	codexApp := &proxymanager.WatchedApp{Kind: proxymanager.DesktopAppCodex, Name: "Codex", Path: "/Applications/Codex.app", BundleID: "com.openai.codex"}

	t.Run("short flags parse independent, remote catalog, with, refresh, and explicit codex app", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
			proxymanager.DesktopAppChatGPT: chatGPT,
			proxymanager.DesktopAppCodex:   codexApp,
		}}
		_, controlCalls, starts := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run", "work", "-i", "-c", "remote", "-w", "extra", "-R", "-a", "codex"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute failed: %v", err)
		}
		if !slices.Equal(*starts, []string{"work|independent|remote|extra"}) {
			t.Fatalf("unexpected start args: %v", *starts)
		}
		if !slices.Equal(desktop.snapshotEvents(), []string{"open:codex"}) {
			t.Fatalf("expected Codex.app to open, got %v", desktop.snapshotEvents())
		}
		if !slices.Equal(*controlCalls, []string{"watch:codex"}) {
			t.Fatalf("expected watch:codex, got %v", *controlCalls)
		}
	})

	t.Run("aliases --solo and --with-provider still work", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
			proxymanager.DesktopAppChatGPT: chatGPT,
		}}
		_, _, starts := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"start", "work", "--solo", "--with-provider", "extra"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute failed: %v", err)
		}
		if !slices.Equal(*starts, []string{"work|independent|local|extra"}) {
			t.Fatalf("unexpected start args: %v", *starts)
		}
	})

	t.Run("-i and -s remain mutually exclusive", func(t *testing.T) {
		cmd := newCodexCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run", "-i", "-s"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("expected mutually exclusive error for -i -s")
		}
	})

	t.Run("--no-open rejects --app and --restart", func(t *testing.T) {
		for _, args := range [][]string{{"run", "--no-open", "-r"}, {"run", "--no-open", "-a", "chatgpt"}} {
			cmd := newCodexCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("expected error for %v", args)
			}
		}
	})
}

func TestCodexRunPrefersChatGPTAndHandlesRestartModes(t *testing.T) {
	chatGPT := &proxymanager.WatchedApp{Kind: proxymanager.DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	codexApp := &proxymanager.WatchedApp{Kind: proxymanager.DesktopAppCodex, Name: "Codex", Path: "/Applications/Codex.app", BundleID: "com.openai.codex"}

	t.Run("prefers ChatGPT over Codex when both exist", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
			proxymanager.DesktopAppChatGPT: chatGPT,
			proxymanager.DesktopAppCodex:   codexApp,
		}}
		_, controlCalls, _ := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(desktop.snapshotEvents(), []string{"open:chatgpt"}) {
			t.Fatalf("expected ChatGPT first, got %v", desktop.snapshotEvents())
		}
		if !slices.Equal(*controlCalls, []string{"watch:chatgpt"}) {
			t.Fatalf("controlCalls = %v", *controlCalls)
		}
		if !strings.Contains(out.String(), "Opened ChatGPT") {
			t.Fatalf("unexpected output: %s", out.String())
		}
	})

	t.Run("falls back to Codex when ChatGPT is not installed", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
			proxymanager.DesktopAppCodex: codexApp,
		}}
		_, _, _ = setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(desktop.snapshotEvents(), []string{"open:codex"}) {
			t.Fatalf("expected Codex fallback, got %v", desktop.snapshotEvents())
		}
	})

	t.Run("running app with -r restarts gently without prompt", func(t *testing.T) {
		desktop := &mockCodexDesktop{
			apps:    map[proxymanager.DesktopApp]*proxymanager.WatchedApp{proxymanager.DesktopAppChatGPT: chatGPT},
			running: true,
		}
		_, controlCalls, _ := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run", "-r"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(desktop.snapshotEvents(), []string{"quit:chatgpt", "open:chatgpt"}) {
			t.Fatalf("events = %v", desktop.snapshotEvents())
		}
		if !slices.Equal(*controlCalls, []string{"pause-watch", "watch:chatgpt"}) {
			t.Fatalf("controlCalls = %v", *controlCalls)
		}
		if !strings.Contains(out.String(), "Restarted ChatGPT gracefully") {
			t.Fatalf("unexpected output: %s", out.String())
		}
	})

	t.Run("running app prompts interactively and restarts on Enter/yes", func(t *testing.T) {
		desktop := &mockCodexDesktop{
			apps:    map[proxymanager.DesktopApp]*proxymanager.WatchedApp{proxymanager.DesktopAppChatGPT: chatGPT},
			running: true,
		}
		_, controlCalls, _ := setupCodexRunTestEnv(t, desktop)
		codexIsInteractiveInput = func(io.Reader) bool { return true }

		cmd := newCodexCommand()
		var out, errOut bytes.Buffer
		cmd.SetIn(strings.NewReader("\n"))
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut.String(), "Restart now? [Y/n]:") {
			t.Fatalf("missing interactive prompt: %q", errOut.String())
		}
		if !slices.Equal(desktop.snapshotEvents(), []string{"quit:chatgpt", "open:chatgpt"}) {
			t.Fatalf("events = %v", desktop.snapshotEvents())
		}
		if !slices.Equal(*controlCalls, []string{"pause-watch", "watch:chatgpt"}) {
			t.Fatalf("controlCalls = %v", *controlCalls)
		}
	})

	t.Run("running app interactive decline clears watch and leaves app running", func(t *testing.T) {
		desktop := &mockCodexDesktop{
			apps:    map[proxymanager.DesktopApp]*proxymanager.WatchedApp{proxymanager.DesktopAppChatGPT: chatGPT},
			running: true,
		}
		_, controlCalls, _ := setupCodexRunTestEnv(t, desktop)
		codexIsInteractiveInput = func(io.Reader) bool { return true }

		cmd := newCodexCommand()
		var out bytes.Buffer
		cmd.SetIn(strings.NewReader("n\n"))
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if len(desktop.snapshotEvents()) != 0 {
			t.Fatalf("expected no app quit/open when declined, got %v", desktop.snapshotEvents())
		}
		if !slices.Equal(*controlCalls, []string{"watch:clear"}) {
			t.Fatalf("expected watch:clear on decline, got %v", *controlCalls)
		}
		if !strings.Contains(out.String(), "Left ChatGPT running") {
			t.Fatalf("unexpected output: %s", out.String())
		}
	})

	t.Run("explicit missing app fails fast before starting proxy", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
			proxymanager.DesktopAppChatGPT: chatGPT,
		}}
		_, _, starts := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run", "-a", "codex"})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "Codex desktop app not found") {
			t.Fatalf("expected missing Codex app error, got %v", err)
		}
		if len(*starts) != 0 {
			t.Fatalf("proxy should not start when explicit --app is missing: %v", *starts)
		}
	})

	t.Run("auto mode with no installed desktop app keeps proxy running for CLI", func(t *testing.T) {
		desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{}}
		_, _, starts := setupCodexRunTestEnv(t, desktop)

		cmd := newCodexCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if len(*starts) != 1 || !strings.Contains(out.String(), "No ChatGPT or Codex desktop app found") {
			t.Fatalf("starts=%v out=%s", *starts, out.String())
		}
	})
}

func TestCodexMenuSwitchPromptsAndGuardsRestart(t *testing.T) {
	chatGPT := &proxymanager.WatchedApp{Kind: proxymanager.DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app", BundleID: "com.openai.chat"}
	desktop := &mockCodexDesktop{
		apps:    map[proxymanager.DesktopApp]*proxymanager.WatchedApp{proxymanager.DesktopAppChatGPT: chatGPT},
		running: true,
	}
	_, controlCalls, _ := setupCodexRunTestEnv(t, desktop)

	var promptedTitle, promptedMsg string
	codexMenuPromptAction = func(title, message, _, _ string) (bool, error) {
		promptedTitle, promptedMsg = title, message
		return true, nil
	}

	cmd := newCodexCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"menu-switch", "--provider", "extra"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("menu-switch failed: %v", err)
	}
	if promptedTitle != "GBX Codex" || !strings.Contains(promptedMsg, "extra") || !strings.Contains(promptedMsg, "ChatGPT") {
		t.Fatalf("unexpected menu prompt: title=%q msg=%q", promptedTitle, promptedMsg)
	}
	if !slices.Equal(desktop.snapshotEvents(), []string{"quit:chatgpt", "open:chatgpt"}) {
		t.Fatalf("expected guarded restart on confirmation, got %v", desktop.snapshotEvents())
	}
	if !slices.Equal(*controlCalls, []string{"pause-watch", "watch:chatgpt"}) {
		t.Fatalf("expected pause-watch then watch:chatgpt, got %v", *controlCalls)
	}
}

func TestCodexHelpQualityAcrossSubcommandsAndLocales(t *testing.T) {
	before := locale.Current()
	t.Cleanup(func() { locale.SetLanguage(before) })

	for _, lang := range []string{"en", "zh"} {
		locale.SetLanguage(lang)
		for _, tc := range []struct {
			args     []string
			required []string
		}{
			{[]string{"--help"}, []string{"run", "start", "stop", "status", "-i", "--independent", "-s", "--shared", "-w", "--with", "-c", "--catalog", "-r", "--restart", "-a", "--app", "-R", "--refresh", "ChatGPT"}},
			{[]string{"run", "--help"}, []string{"-r", "--restart", "-a", "--app", "--no-open", "-i", "ChatGPT", "Codex"}},
			{[]string{"start", "--help"}, []string{"-i", "-s", "-R", "CLI"}},
		} {
			cmd := newCodexCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("lang=%s args=%v: %v", lang, tc.args, err)
			}
			for _, token := range tc.required {
				if !strings.Contains(out.String(), token) {
					t.Fatalf("lang=%s args=%v help missing %q:\n%s", lang, tc.args, token, out.String())
				}
			}
		}
	}
	// Ensure status displays tracked desktop app cleanly when running.
	desktop := &mockCodexDesktop{apps: map[proxymanager.DesktopApp]*proxymanager.WatchedApp{
		proxymanager.DesktopAppChatGPT: {Kind: proxymanager.DesktopAppChatGPT, Name: "ChatGPT", Path: "/Applications/ChatGPT.app"},
	}}
	_, _, _ = setupCodexRunTestEnv(t, desktop)
	runCmd := newCodexCommand()
	runCmd.SetOut(&bytes.Buffer{})
	runCmd.SetErr(&bytes.Buffer{})
	runCmd.SetArgs([]string{"run"})
	if err := runCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	statusCmd := newCodexCommand()
	var statusOut bytes.Buffer
	statusCmd.SetOut(&statusOut)
	statusCmd.SetErr(&bytes.Buffer{})
	statusCmd.SetArgs([]string{"status"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statusCmd.SetContext(ctx)
	if err := statusCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusOut.String(), "Desktop app: ChatGPT (auto-stop on quit)") {
		t.Fatalf("status missing desktop app watch line:\n%s", statusOut.String())
	}
}
