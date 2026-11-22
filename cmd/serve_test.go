package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/provider"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestServeCommandFlagsAndValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// --host was retired: gbx serve always binds 127.0.0.1 (or a local Unix socket).
	cmd := newServeCommand()
	if cmd.Flags().Lookup("host") != nil {
		t.Fatal("gbx serve still registers --host")
	}

	for _, args := range [][]string{
		{"--port", "70000", "--no-open"},
		{"--socket", "/tmp/x.sock", "--port", "8099", "--no-open"},
		{"--base-path", "../bad", "--no-open"},
		{"--sandbox", "invalid", "--no-open"},
		{"--sandbox=", "--no-open"},
		{"extra-arg", "--no-open"},
	} {
		c := newServeCommand()
		c.SetArgs(args)
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		if err := c.Execute(); err == nil {
			t.Fatalf("expected error for args %v", args)
		}
	}
}

func TestServeCommandSocketLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX socket mode test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	shortDir, err := os.MkdirTemp("", "gbx-serve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	sock := filepath.Join(shortDir, "gbx.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout syncBuffer
	cmd := newServeCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&stdout)
	if err := config.Update(func(c *provider.Config) error {
		c.ServeSandbox = "workspace"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--socket", sock, "--workspace", t.TempDir(), "--no-shell", "--sandbox", "off"})

	done := make(chan error, 1)
	go func() {
		done <- cmd.Execute()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "\n") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("gbx serve --socket did not print ready JSON")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var ready struct {
		Socket string `json:"socket"`
		PID    int    `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &ready); err != nil {
		t.Fatalf("ready JSON: %v (%q)", err, stdout.String())
	}
	if ready.Socket != sock || ready.PID == 0 {
		t.Fatalf("ready = %+v", ready)
	}

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(body), `"defaultSandbox":"off"`) {
		t.Fatalf("sandbox flag did not reach server: %s %v", body, err)
	}
	if !strings.Contains(stderr.String(), "sandbox") && !strings.Contains(stderr.String(), "沙盒") {
		t.Fatalf("off flag omitted warning: %q", stderr.String())
	}
	loaded, err := config.Load()
	if err != nil || loaded.ServeSandbox != "workspace" {
		t.Fatalf("flag persisted over saved setting: %+v %v", loaded, err)
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("serve exit = %v", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gbx serve did not exit on context cancel")
	}
}

func TestServeRunBrowserAutoExitAndReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GBX_SERVE_NO_OPEN", "")

	opened := make(chan string, 1)
	prevOpen := openServeBrowser
	openServeBrowser = func(u string) error {
		opened <- u
		return nil
	}
	t.Cleanup(func() { openServeBrowser = prevOpen })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	cmd := newServeCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "--workspace", t.TempDir(), "--no-shell", "--exit-grace", "150ms"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	var launchURL string
	select {
	case launchURL = <-opened:
	case err := <-done:
		t.Fatalf("serve run exited early: %v\nstderr=%s", err, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("serve run did not call openServeBrowser")
	}

	baseURL, token, ok := strings.Cut(launchURL, "#")
	if !ok || token == "" {
		t.Fatalf("launchURL missing generated token fragment: %q", launchURL)
	}
	presenceURL := strings.TrimRight(baseURL, "/") + "/api/presence?token=" + token

	openTab := func() (*http.Response, context.CancelFunc) {
		t.Helper()
		tabCtx, tabCancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(tabCtx, http.MethodGet, presenceURL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			tabCancel()
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			tabCancel()
			t.Fatalf("presence status = %d", resp.StatusCode)
		}
		buf := make([]byte, 32)
		if _, err := resp.Body.Read(buf); err != nil {
			_ = resp.Body.Close()
			tabCancel()
			t.Fatalf("read connected event: %v", err)
		}
		return resp, tabCancel
	}

	// Tab 1 connects -> gbx serve status sees 1 connected tab and run mode.
	tab1, cancel1 := openTab()
	var statusOut bytes.Buffer
	statusCmd := newServeCommand()
	statusCmd.SetOut(&statusOut)
	statusCmd.SetErr(&bytes.Buffer{})
	statusCmd.SetArgs([]string{"status"})
	if err := statusCmd.Execute(); err != nil {
		t.Fatalf("serve status: %v", err)
	}
	if !strings.Contains(statusOut.String(), "run") || !strings.Contains(statusOut.String(), "1") {
		t.Fatalf("unexpected status output: %q", statusOut.String())
	}

	// Simulate page reload: close tab 1 and reconnect within 40ms (< 150ms grace).
	cancel1()
	_ = tab1.Body.Close()
	time.Sleep(40 * time.Millisecond)
	tab2, cancel2 := openTab()

	select {
	case err := <-done:
		t.Fatalf("serve run exited during reload grace window: %v", err)
	case <-time.After(220 * time.Millisecond):
	}

	// Now close last tab -> serve run must exit automatically and cleanly.
	cancel2()
	_ = tab2.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve run exit error = %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("serve run did not auto-exit after last browser tab closed")
	}

	// State file must be removed on exit.
	if _, err := os.Stat(filepath.Join(home, ".gbx", "serve", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("expected state.json removed after auto-exit, stat err=%v", err)
	}
}

func TestServeRunStoppedByServeStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	cmd := newServeCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "--no-open", "--workspace", t.TempDir(), "--no-shell"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait until status reports running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var statusOut bytes.Buffer
		sc := newServeCommand()
		sc.SetOut(&statusOut)
		sc.SetErr(&bytes.Buffer{})
		sc.SetArgs([]string{"status"})
		_ = sc.Execute()
		if strings.Contains(statusOut.String(), "127.0.0.1") {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve run did not publish status; stderr=%s status=%q", stderr.String(), statusOut.String())
		}
		time.Sleep(30 * time.Millisecond)
	}

	var stopOut bytes.Buffer
	stopCmd := newServeCommand()
	stopCmd.SetOut(&stopOut)
	stopCmd.SetErr(&bytes.Buffer{})
	stopCmd.SetArgs([]string{"stop"})
	if err := stopCmd.Execute(); err != nil {
		cancel()
		t.Fatalf("serve stop: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve run exit after stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("serve run did not exit after gbx serve stop")
	}

	// Second stop reports not running cleanly.
	var stopOut2 bytes.Buffer
	stopCmd2 := newServeCommand()
	stopCmd2.SetOut(&stopOut2)
	stopCmd2.SetErr(&bytes.Buffer{})
	stopCmd2.SetArgs([]string{"stop"})
	if err := stopCmd2.Execute(); err != nil {
		t.Fatalf("second serve stop: %v", err)
	}
	if !strings.Contains(stopOut2.String(), "not running") && !strings.Contains(stopOut2.String(), "未运行") {
		t.Fatalf("unexpected second stop output: %q", stopOut2.String())
	}
}

func TestServeHelpBilingual(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			t.Setenv("GBX_LANG", lang)
			for _, sub := range [][]string{{"--help"}, {"run", "--help"}, {"start", "--help"}, {"status", "--help"}, {"stop", "--help"}} {
				var out bytes.Buffer
				cmd := newServeCommand()
				cmd.SetOut(&out)
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(sub)
				if err := cmd.Execute(); err != nil {
					t.Fatalf("help %v failed: %v", sub, err)
				}
				text := out.String()
				if strings.TrimSpace(text) == "" {
					t.Fatalf("empty help for %v", sub)
				}
				if len(sub) == 1 {
					for _, want := range []string{"run", "start", "status", "stop"} {
						if !strings.Contains(text, want) {
							t.Fatalf("root serve help (%s) missing %q:\n%s", lang, want, text)
						}
					}
				}
			}
		})
	}
}
