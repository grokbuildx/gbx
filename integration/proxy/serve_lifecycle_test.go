package proxy_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/webui"
)

func TestServeStartStatusStopLifecycle(t *testing.T) {
	binary := os.Getenv("GBX_TEST_BINARY")
	if binary == "" {
		t.Skip("set GBX_TEST_BINARY to a built native gbx binary")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GBX_LANG", "en")
	t.Setenv("GBX_SERVE_NO_OPEN", "1")

	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gbx %v failed: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// Ensure any leaked daemon is stopped on test failure.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, binary, "serve", "stop").Run()
	})

	// 1. Detached start returns immediately with a launch URL and persists state (without API token).
	startOut := run("serve", "start", "--workspace", workspace, "--no-shell")
	if !strings.Contains(startOut, "http://127.0.0.1:") || !strings.Contains(startOut, "#") {
		t.Fatalf("unexpected serve start output: %q", startOut)
	}
	mgr := &webui.LifecycleManager{Dir: filepath.Join(home, ".gbx", "serve")}
	state1, err := mgr.Read()
	if err != nil || state1 == nil {
		t.Fatalf("read state after start: (%+v, %v)", state1, err)
	}
	if state1.Mode != webui.ServeModeStart || state1.AutoExit {
		t.Fatalf("unexpected state1: %+v", state1)
	}
	if strings.Contains(state1.URL, "#") {
		t.Fatalf("state.json must not store launch token fragment: %q", state1.URL)
	}

	// Extract generated token from stdout to verify /api/presence connects and
	// disconnecting does NOT stop a `serve start` daemon.
	idx := strings.Index(startOut, "http://127.0.0.1:")
	rest := startOut[idx:]
	launchURL := strings.Fields(rest)[0]
	baseURL, token, ok := strings.Cut(launchURL, "#")
	if !ok || token == "" {
		t.Fatalf("could not parse launch URL from %q", startOut)
	}

	tabCtx, tabCancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(tabCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/presence?token="+token, nil)
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
	_, _ = resp.Body.Read(buf)

	statusWithTab := run("serve", "status")
	if !strings.Contains(statusWithTab, "Tabs:     1") {
		tabCancel()
		_ = resp.Body.Close()
		t.Fatalf("expected 1 connected tab in status:\n%s", statusWithTab)
	}

	// Close the browser tab: persistent `serve start` daemon must remain alive.
	tabCancel()
	_ = resp.Body.Close()
	time.Sleep(300 * time.Millisecond)

	statusAfterClose := run("serve", "status")
	if !strings.Contains(statusAfterClose, "background (start)") || !strings.Contains(statusAfterClose, "Tabs:     0") {
		t.Fatalf("serve start daemon should stay running with 0 tabs:\n%s", statusAfterClose)
	}

	// 2. Starting again replaces the previous daemon cleanly.
	_ = run("serve", "start", "--workspace", workspace, "--no-shell")
	state2, err := mgr.Read()
	if err != nil || state2 == nil || state2.ID == state1.ID {
		t.Fatalf("expected new instance ID on restart: old=%+v new=%+v err=%v", state1, state2, err)
	}

	// 3. `gbx serve stop` shuts down the daemon and removes state.json.
	stopOut := run("serve", "stop")
	if !strings.Contains(stopOut, "Stopped gbx serve") {
		t.Fatalf("unexpected stop output: %q", stopOut)
	}
	if _, err := os.Stat(mgr.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("state.json still present after stop: %v", err)
	}
	statusStopped := run("serve", "status")
	if !strings.Contains(statusStopped, "not running") {
		t.Fatalf("unexpected status after stop: %q", statusStopped)
	}
}
