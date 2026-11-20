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
