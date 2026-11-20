package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/groklaunch"
	"github.com/grokbuildx/gbx/internal/provider"
)

func sandboxServer(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	cfg.Token = "secret"
	if cfg.StoreRoot == "" {
		cfg.StoreRoot = t.TempDir()
	}
	if cfg.Workspace == "" {
		cfg.Workspace = t.TempDir()
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, registerHTTPTestServer(t, s.Handler())
}

func TestSandboxDefaultsAndStartupOverride(t *testing.T) {
	isolateConfig(t)
	for _, tc := range []struct{ saved, flag, want string }{
		{"", "", "workspace"},
		{"off", "", "off"},
		{"off", "workspace", "workspace"},
		{"workspace", "off", "off"},
	} {
		t.Run(tc.saved+"/"+tc.flag, func(t *testing.T) {
			seedConfig(t, func(c *provider.Config) { c.ServeSandbox = tc.saved })
			s, httpServer := sandboxServer(t, Config{Sandbox: tc.flag})
			if s.SandboxMode() != tc.want {
				t.Fatalf("mode = %q, want %q", s.SandboxMode(), tc.want)
			}
			cfg, err := config.Load()
			if err != nil || cfg.ServeSandbox != tc.saved {
				t.Fatalf("launch changed saved default: %v, %q", err, cfg.ServeSandbox)
			}
			for _, path := range []string{"/api/config", "/api/capabilities"} {
				status, body := doJSON(t, "GET", httpServer.URL+path, "secret", "")
				key := "serveSandbox"
				if path == "/api/capabilities" {
					key = "defaultSandbox"
				}
				var fields map[string]any
				_ = json.Unmarshal(body, &fields)
				if status != http.StatusOK || fields[key] != tc.want {
					t.Fatalf("%s = %d %s", path, status, body)
				}
			}
		})
	}
	for _, mode := range []string{"strict", "OFF", "typo"} {
		if _, err := New(Config{Sandbox: mode}); err == nil {
			t.Fatalf("accepted %q", mode)
		}
	}
	seedConfig(t, func(c *provider.Config) { c.ServeSandbox = "typo" })
	if _, err := New(Config{}); err == nil {
		t.Fatal("accepted invalid saved mode")
	}
}

func TestSandboxSettingsRequireConfirmationAndPersist(t *testing.T) {
	isolateConfig(t)
	seedConfig(t, func(c *provider.Config) { c.BypassMode = true })
	s, h := sandboxServer(t, Config{})
	for _, body := range []string{`{"serveSandbox":"typo"}`, `{"serveSandbox":"off"}`, `{"serveSandbox":"off","confirmSandboxOff":false}`} {
		status, response := doJSON(t, "PUT", h.URL+"/api/config", "secret", body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", body, status, response)
		}
		if s.SandboxMode() != "workspace" {
			t.Fatal("failed update published mode")
		}
		cfg, _ := config.Load()
		if cfg.ServeSandbox != "" || !cfg.BypassMode {
			t.Fatal("failed update mutated config")
		}
	}
	// Persisting a command-line off override also needs explicit confirmation.
	s2, h2 := sandboxServer(t, Config{Sandbox: "off"})
	status, _ := doJSON(t, "PUT", h2.URL+"/api/config", "secret", `{"serveSandbox":"off"}`)
	if status != http.StatusBadRequest || s2.SandboxMode() != "off" {
		t.Fatal("off override persisted without confirmation")
	}
	status, body := doJSON(t, "PUT", h.URL+"/api/config", "secret", `{"serveSandbox":"off","confirmSandboxOff":true,"activeProvider":"missing"}`)
	if status != http.StatusBadRequest || s.SandboxMode() != "workspace" {
		t.Fatalf("invalid combined update = %d %s", status, body)
	}
	status, body = doJSON(t, "PUT", h.URL+"/api/config", "secret", `{"serveSandbox":"off","confirmSandboxOff":true}`)
	if status != http.StatusOK || s.SandboxMode() != "off" {
		t.Fatalf("confirmed off = %d %s", status, body)
	}
	cfg, _ := config.Load()
	if cfg.ServeSandbox != "off" || !cfg.BypassMode {
		t.Fatal("sandbox save changed approval or did not persist")
	}
	info, err := os.Stat(config.ConfigPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions: %v %v", info, err)
	}
	fresh, _ := sandboxServer(t, Config{})
	if fresh.SandboxMode() != "off" {
		t.Fatal("restart lost saved mode")
	}
	status, body = doJSON(t, "PATCH", h.URL+"/api/config", "secret", `{"serveSandbox":"workspace"}`)
	if status != http.StatusOK || s.SandboxMode() != "workspace" {
		t.Fatalf("enable = %d %s", status, body)
	}
	cfg, _ = config.Load()
	if cfg.ServeSandbox != "workspace" || !cfg.BypassMode {
		t.Fatal("enable did not preserve approval")
	}
	if status, _ = doJSON(t, "PUT", h.URL+"/api/config", "", `{"serveSandbox":"off","confirmSandboxOff":true}`); status != http.StatusUnauthorized {
		t.Fatal("unauthenticated sandbox update allowed")
	}
}

func TestSandboxSessionsKeepModeAcrossSettingsAndRestart(t *testing.T) {
	isolateConfig(t)
	seen := make(chan LaunchSpec, 8)
	prepare := helperPrepare(t, "fake reply", false)
	cfg := Config{Workspace: t.TempDir(), StoreRoot: t.TempDir(), Launch: func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		seen <- spec
		return prepare(ctx, spec)
	}}
	s, h := sandboxServer(t, cfg)
	create := func() *Session {
		t.Helper()
		status, body := doJSON(t, "POST", h.URL+"/api/sessions", "secret", `{}`)
		var meta Meta
		if status != http.StatusCreated || json.Unmarshal(body, &meta) != nil {
			t.Fatalf("create = %d %s", status, body)
		}
		ss, err := s.store.Open(meta.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ss
	}
	first := create()
	if first.Meta().Sandbox != "workspace" {
		t.Fatal("default not captured")
	}
	run := func(server *Server, ss *Session, wantMode, wantResume string) {
		t.Helper()
		handle, err := server.startTurn(ss, "hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		waitDone(t, handle.Done)
		spec := <-seen
		if spec.Sandbox != wantMode || spec.Resume != wantResume {
			t.Fatalf("spec = %+v", spec)
		}
		results := resultPayloads(t, ss)
		if len(results) == 0 || results[len(results)-1].IsError {
			t.Fatalf("results = %+v", results)
		}
	}
	run(s, first, "workspace", "")
	resume := first.Meta().AgentSessionID
	if resume == "" {
		t.Fatal("missing ACP session ID")
	}
	status, body := doJSON(t, "PUT", h.URL+"/api/config", "secret", `{"serveSandbox":"off","confirmSandboxOff":true}`)
	if status != http.StatusOK {
		t.Fatalf("off = %d %s", status, body)
	}
	second := create()
	if second.Meta().Sandbox != "off" {
		t.Fatal("new session did not capture off")
	}
	run(s, first, "workspace", resume)
	run(s, second, "off", "")
	if status, _ := doJSON(t, "PATCH", h.URL+"/api/sessions/"+first.ID(), "secret", `{"sandbox":"off"}`); status != http.StatusConflict {
		t.Fatal("existing sandbox changed")
	}
	// Reopen the same store under a different startup default; sessions stay pinned.
	restarted, _ := sandboxServer(t, Config{Workspace: cfg.Workspace, StoreRoot: cfg.StoreRoot, Sandbox: "workspace", Launch: cfg.Launch})
	reopened, err := restarted.store.Open(second.ID())
	if err != nil {
		t.Fatal(err)
	}
	run(restarted, reopened, "off", second.Meta().AgentSessionID)
	legacy, err := s.store.Create(Meta{Workspace: cfg.Workspace, Provider: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if s.view(legacy.Meta()).Sandbox != "workspace" {
		t.Fatal("legacy view inherited off")
	}
	run(s, legacy, "workspace", "")
}

func TestSandboxLauncherPoliciesAndProviderIsolation(t *testing.T) {
	home := isolateConfig(t)
	bin := filepath.Join(home, "fake-grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(groklaunch.BinaryEnv, bin)
	t.Setenv("GROK_SANDBOX", "off")
	seedConfig(t, func(c *provider.Config) {
		c.Providers["fake"] = provider.Provider{Name: "fake", Type: "openai", Endpoint: "http://127.0.0.1:9/v1", APIKey: "fake", Model: "fake-model", Env: map[string]string{"GROK_SANDBOX": "off"}}
	})
	worktree := t.TempDir()
	_ = os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: /tmp/repo/.git/worktrees/test\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(worktree, ".grok"), 0o700)
	_ = os.WriteFile(filepath.Join(worktree, ".grok", "sandbox.toml"), []byte("[profiles.gbx_worktree]\nextends = \"workspace\"\n"), 0o600)
	for _, name := range []string{"default", "fake"} {
		for _, mode := range []string{"", "workspace", "off"} {
			for _, dir := range []string{t.TempDir(), worktree} {
				for _, permission := range []string{"default", "bypassPermissions"} {
					cmd, cleanup, err := DefaultLauncher(context.Background(), LaunchSpec{Provider: name, Sandbox: mode, Workspace: dir, PermissionMode: permission})
					if err != nil {
						t.Fatal(err)
					}
					want := "workspace"
					if mode == "off" {
						want = "off"
					} else if dir == worktree {
						want = "gbx_worktree"
					}
					var values []string
					for _, e := range cmd.Env {
						if strings.HasPrefix(e, "GROK_SANDBOX=") {
							values = append(values, e)
						}
					}
					cleanup()
					if !slices.Equal(values, []string{"GROK_SANDBOX=" + want}) {
						t.Fatalf("%s/%s/%s = %v", name, mode, dir, values)
					}
					if slices.Contains(cmd.Args, "--always-approve") != (permission == "bypassPermissions") {
						t.Fatalf("sandbox altered approvals: %v", cmd.Args)
					}
					if !slices.Contains(cmd.Env, "GROK_DISABLE_AUTOUPDATER=1") {
						t.Fatal("lost updater policy")
					}
				}
			}
		}
	}
	if _, _, err := DefaultLauncher(context.Background(), LaunchSpec{Sandbox: "typo"}); err == nil {
		t.Fatal("launcher accepted invalid mode")
	}
}

func TestSandboxRejectsProfileChangesBeforeACP(t *testing.T) {
	isolateConfig(t)
	store := newTestStore(t)
	for _, mode := range []string{"workspace", "off"} {
		session, err := store.Create(Meta{Workspace: t.TempDir(), Sandbox: mode, AgentSessionID: "existing"})
		if err != nil {
			t.Fatal(err)
		}
		other := "workspace"
		if mode == "workspace" {
			other = "off"
		}
		called := false
		runner := newTestRunner(t, func(context.Context, LaunchSpec) (*exec.Cmd, func(), error) {
			called = true
			return nil, nil, nil
		})
		if _, err := NewTimeline().Start(runner, session, LaunchSpec{Sandbox: other, Prompt: "p", Resume: "existing"}); err == nil || called {
			t.Fatalf("%s -> %s reached ACP: %v", mode, other, err)
		}
	}
	workspace := t.TempDir()
	_ = os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: /tmp/repo/.git/worktrees/test\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(workspace, ".grok"), 0o700)
	profilePath := filepath.Join(workspace, ".grok", "sandbox.toml")
	_ = os.WriteFile(profilePath, []byte("[profiles.gbx_worktree]\nextends = \"workspace\"\n"), 0o600)
	session, err := store.Create(Meta{Workspace: workspace, Sandbox: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{Workspace: workspace}
	if err := pinSessionSandbox(session, &spec); err != nil {
		t.Fatal(err)
	}
	if session.Meta().SandboxProfile != "gbx_worktree" || spec.sandboxProfile != "gbx_worktree" {
		t.Fatal("worktree profile was not pinned")
	}
	_ = os.WriteFile(profilePath, []byte("# removed profile\n"), 0o600)
	if err := pinSessionSandbox(session, &spec); err == nil {
		t.Fatal("missing profile accepted on resume")
	}
	if _, _, err := DefaultLauncher(context.Background(), spec); err == nil {
		t.Fatal("launcher accepted changed profile")
	}
	legacy, err := store.Create(Meta{Workspace: workspace, Worktree: &SessionWorktree{Name: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pinSessionSandbox(legacy, &LaunchSpec{Workspace: workspace}); err == nil {
		t.Fatal("legacy worktree missing its profile accepted")
	}
}

func TestSandboxPinFailureDoesNotPublish(t *testing.T) {
	store := newTestStore(t)
	session, err := store.Create(Meta{Workspace: t.TempDir(), Sandbox: "off"})
	if err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(session.dir, "meta.json")
	if err := os.Rename(metaPath, metaPath+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metaPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := pinSessionSandbox(session, &LaunchSpec{Workspace: session.Meta().Workspace}); err == nil {
			t.Fatal("pin succeeded without persistence")
		}
		if session.Meta().SandboxProfile != "" {
			t.Fatal("failed pin published in-memory profile")
		}
	}
}
