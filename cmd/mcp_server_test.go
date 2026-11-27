package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPServerSubprocessHelper(t *testing.T) {
	if os.Getenv("GBX_TEST_MCP_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"gbx", "mcp-server"}, strings.Split(os.Getenv("GBX_TEST_MCP_ARGS"), "\n")...)
	Execute()
	os.Exit(0)
}

func TestMCPServerRunsWithoutProviderConfigAndKeepsStdoutPure(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("USERPROFILE", emptyHome)

	repo := t.TempDir()
	reqLines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workspace_list","arguments":{}}}`,
	}, "\n") + "\n"

	// 1. Direct Cobra command test.
	var stdout, stderr syncBuffer
	c := newMCPServerCommand()
	c.SetIn(strings.NewReader(reqLines))
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"--workspace", repo, "--lock-workspaces"})
	if err := c.Execute(); err != nil {
		t.Fatalf("newMCPServerCommand Execute() failed: %v (stderr: %s)", err, stderr.String())
	}
	assertPureJSONRPCLines(t, stdout.String(), repo)

	// 2. Full subprocess Execute() test verifying root.go startup isolation and pure stdout.
	sub := exec.Command(os.Args[0], "-test.run=^TestMCPServerSubprocessHelper$")
	sub.Env = append(os.Environ(),
		"GBX_TEST_MCP_HELPER=1",
		"GBX_TEST_MCP_ARGS="+strings.Join([]string{"--workspace", repo, "--lock-workspaces"}, "\n"),
		"HOME="+emptyHome,
		"USERPROFILE="+emptyHome,
	)
	sub.Stdin = strings.NewReader(reqLines)
	var subOut, subErr bytes.Buffer
	sub.Stdout = &subOut
	sub.Stderr = &subErr
	if err := sub.Run(); err != nil {
		t.Fatalf("subprocess Execute() failed: %v (stderr: %s)", err, subErr.String())
	}
	assertPureJSONRPCLines(t, subOut.String(), repo)

	// ~/.gbx must remain untouched when starting mcp-server without worktrees or --save.
	if _, err := os.Stat(filepath.Join(emptyHome, ".gbx")); !os.IsNotExist(err) {
		t.Fatalf("expected ~/.gbx not to be created by mcp-server startup, stat err = %v", err)
	}

	// 3. Verify --save writes ~/.gbx/mcp.json and subsequent zero-arg mcp-server loads it automatically.
	saveCmd := newMCPServerCommand()
	var saveOut syncBuffer
	saveCmd.SetOut(&saveOut)
	saveCmd.SetErr(&saveOut)
	saveCmd.SetArgs([]string{"--workspace", repo, "--save"})
	if err := saveCmd.Execute(); err != nil {
		t.Fatalf("--save failed: %v (%s)", err, saveOut.String())
	}

	zeroArgCmd := newMCPServerCommand()
	var zeroOut, zeroErr syncBuffer
	zeroArgCmd.SetIn(strings.NewReader(reqLines))
	zeroArgCmd.SetOut(&zeroOut)
	zeroArgCmd.SetErr(&zeroErr)
	zeroArgCmd.SetArgs(nil)
	if err := zeroArgCmd.Execute(); err != nil {
		t.Fatalf("zero-arg mcp-server failed: %v (%s)", err, zeroErr.String())
	}
	assertPureJSONRPCLines(t, zeroOut.String(), repo)
}

func TestMCPServerDoesNotBreakGrokMCPForwarding(t *testing.T) {
	_, _, dispatchMCPServer, err := resolveRootGrokArgs([]string{"mcp-server", "--help"})
	if err != nil || dispatchMCPServer {
		t.Fatalf("expected mcp-server to stay in gbx rootCmd, got dispatch=%v err=%v", dispatchMCPServer, err)
	}

	_, rest, dispatchGrokMCP, err := resolveRootGrokArgs([]string{"mcp", "list"})
	if err != nil || !dispatchGrokMCP || len(rest) != 2 || rest[0] != "mcp" {
		t.Fatalf("expected 'gbx mcp list' to forward to Grok Build, got dispatch=%v rest=%v err=%v", dispatchGrokMCP, rest, err)
	}
}

func assertPureJSONRPCLines(t *testing.T, rawOut, expectedRepo string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(rawOut), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 JSON-RPC responses on stdout, got %d:\n%s", len(lines), rawOut)
	}
	for i, line := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout line %d is not valid JSON-RPC: %v\nline: %q", i+1, err, line)
		}
	}
	if !strings.Contains(lines[1], expectedRepo) {
		t.Fatalf("expected workspace_list response to include %q, got: %s", expectedRepo, lines[1])
	}
}
