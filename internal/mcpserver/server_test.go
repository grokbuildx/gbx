package mcpserver_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/mcpserver"
)

type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type mockMCPHost struct {
	t      *testing.T
	toSrv  *io.PipeWriter
	fromBr *bufio.Reader

	mu        sync.Mutex
	rootsURIs []string
	respCh    map[int]chan rpcFrame
	nextID    int
	done      chan error
}

func startTestServer(t *testing.T, cfg mcpserver.Config, initialRoots []string) *mockMCPHost {
	t.Helper()
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = filepath.Join(t.TempDir(), "mcp.json")
	}
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()

	srv, err := mcpserver.New(cfg)
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	h := &mockMCPHost{
		t:         t,
		toSrv:     srvInW,
		fromBr:    bufio.NewReader(srvOutR),
		rootsURIs: append([]string(nil), initialRoots...),
		respCh:    make(map[int]chan rpcFrame),
		done:      make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = srvInW.Close()
		cancel()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Errorf("server did not shut down cleanly")
		}
		_ = srvOutR.Close()
	})

	go func() {
		err := srv.Serve(ctx, srvInR, srvOutW)
		_ = srvOutW.Close()
		h.done <- err
	}()

	go h.readLoop()
	return h
}

func (h *mockMCPHost) readLoop() {
	for {
		line, err := h.fromBr.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var frame rpcFrame
			if uErr := json.Unmarshal(line, &frame); uErr == nil {
				if frame.Method == "roots/list" {
					h.mu.Lock()
					uris := append([]string(nil), h.rootsURIs...)
					h.mu.Unlock()
					type rootItem struct {
						URI  string `json:"uri"`
						Name string `json:"name"`
					}
					roots := make([]rootItem, 0, len(uris))
					for _, u := range uris {
						roots = append(roots, rootItem{URI: u, Name: filepath.Base(u)})
					}
					resRaw, _ := json.Marshal(map[string]any{"roots": roots})
					reply, _ := json.Marshal(rpcFrame{
						JSONRPC: "2.0",
						ID:      frame.ID,
						Result:  resRaw,
					})
					_, _ = h.toSrv.Write(append(reply, '\n'))
					continue
				}
				var id int
				if json.Unmarshal(frame.ID, &id) == nil {
					h.mu.Lock()
					ch, ok := h.respCh[id]
					if ok {
						delete(h.respCh, id)
					}
					h.mu.Unlock()
					if ok {
						ch <- frame
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *mockMCPHost) setRoots(uris []string) {
	h.mu.Lock()
	h.rootsURIs = append([]string(nil), uris...)
	h.mu.Unlock()
	h.notify("notifications/roots/list_changed", nil)
}

func (h *mockMCPHost) notify(method string, params any) {
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	b, _ := json.Marshal(rpcFrame{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	})
	_, _ = h.toSrv.Write(append(b, '\n'))
}

func (h *mockMCPHost) call(method string, params any) rpcFrame {
	h.t.Helper()
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	ch := make(chan rpcFrame, 1)
	h.respCh[id] = ch
	h.mu.Unlock()

	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	idRaw, _ := json.Marshal(id)
	b, _ := json.Marshal(rpcFrame{
		JSONRPC: "2.0",
		ID:      idRaw,
		Method:  method,
		Params:  raw,
	})
	if _, err := h.toSrv.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write request %s: %v", method, err)
	}
	select {
	case resp := <-ch:
		return resp
	case <-time.After(10 * time.Second):
		h.t.Fatalf("timeout waiting for %s response", method)
		return rpcFrame{}
	}
}

func (h *mockMCPHost) callTool(name string, args map[string]any) (string, bool) {
	h.t.Helper()
	resp := h.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if resp.Error != nil {
		h.t.Fatalf("protocol error on tool %s: %+v", name, resp.Error)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		h.t.Fatalf("unmarshal tool result %s: %v", name, err)
	}
	if len(out.Content) == 0 {
		return "", out.IsError
	}
	return out.Content[0].Text, out.IsError
}

func pathToFileURI(p string) string {
	slashed := filepath.ToSlash(p)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	return u.String()
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/calc\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
}

func TestDynamicRootsAndSchemaCompatibility(t *testing.T) {
	repoA := t.TempDir()
	initGitRepo(t, repoA)
	repoB := t.TempDir()
	initGitRepo(t, repoB)

	host := startTestServer(t, mcpserver.Config{Version: "1.2.3"}, []string{pathToFileURI(repoA)})

	// 1. Initialize advertising roots capability (like macOS Gemini App).
	initResp := host.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{
			"roots": map[string]any{"listChanged": true},
		},
	})
	if initResp.Error != nil {
		t.Fatalf("initialize failed: %+v", initResp.Error)
	}
	host.notify("notifications/initialized", nil)

	// 2. Validate tools/list schemas are strictly Gemini-compatible.
	toolsResp := host.call("tools/list", map[string]any{})
	var listed struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Type       string         `json:"type"`
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(toolsResp.Result, &listed); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("expected non-empty tools list")
	}
	for _, tool := range listed.Tools {
		if tool.InputSchema.Type != "object" {
			t.Fatalf("tool %s schema type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
		if tool.InputSchema.Properties == nil {
			t.Fatalf("tool %s properties map is nil (rejected by Gemini schema validators)", tool.Name)
		}
	}
	if !strings.Contains(string(toolsResp.Result), `"readOnlyHint":true`) || !strings.Contains(string(toolsResp.Result), `"destructiveHint":false`) {
		t.Fatalf("expected explicit MCP ToolAnnotations on read-only tools, got: %s", toolsResp.Result)
	}

	// 3. Single connected folder auto-resolves without passing 'workspace' (including batch paths).
	readTxt, isErr := host.callTool("read_file", map[string]any{"paths": []string{"calc.go", "go.mod"}})
	if isErr || !strings.Contains(readTxt, "func Add") || !strings.Contains(readTxt, "example.com/calc") {
		t.Fatalf("batch read_file from dynamic root failed: isErr=%v out=%s", isErr, readTxt)
	}

	// 4. Live roots/list_changed switches connected folder to repoB.
	if err := os.WriteFile(filepath.Join(repoB, "only_in_b.txt"), []byte("hello from B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host.setRoots([]string{pathToFileURI(repoB)})
	deadline := time.Now().Add(3 * time.Second)
	for {
		txt, errFlag := host.callTool("read_file", map[string]any{"path": "only_in_b.txt"})
		if !errFlag && strings.Contains(txt, "hello from B") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roots/list_changed did not switch to repoB; last=%s", txt)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestZeroImplicitCwdAndSecurityBoundaries(t *testing.T) {
	// 1. With no --workspace and no client roots, server must NOT expose os.Getwd().
	emptyHost := startTestServer(t, mcpserver.Config{}, nil)
	emptyHost.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	emptyHost.notify("notifications/initialized", nil)
	txt, isErr := emptyHost.callTool("list_dir", map[string]any{})
	if !isErr || !strings.Contains(txt, "no workspace") {
		t.Fatalf("expected refusal when no workspace or connected folder is set, got isErr=%v txt=%s", isErr, txt)
	}

	// 2. Symlink escape, .git blocking, --lock-workspaces, and --no-exec.
	repo := t.TempDir()
	initGitRepo(t, repo)
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(secretFile, filepath.Join(repo, "escape.txt")); err != nil {
			t.Fatal(err)
		}
	}

	lockedHost := startTestServer(t, mcpserver.Config{
		Workspaces:     []string{repo},
		LockWorkspaces: true,
		DisableExec:    true,
	}, []string{pathToFileURI(outside)})
	lockedHost.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{"roots": map[string]any{}},
	})
	lockedHost.notify("notifications/initialized", nil)

	// Verify run_command is omitted and rejected under --no-exec.
	toolsResp := lockedHost.call("tools/list", nil)
	if strings.Contains(string(toolsResp.Result), `"run_command"`) {
		t.Fatalf("run_command should not be advertised under --no-exec: %s", toolsResp.Result)
	}
	callExec := lockedHost.call("tools/call", map[string]any{"name": "run_command", "arguments": map[string]any{"command": "echo hi"}})
	if callExec.Error == nil {
		t.Fatalf("expected protocol error calling disabled run_command, got %s", callExec.Result)
	}

	// Verify --lock-workspaces ignored outside client root.
	wsList, isErr := lockedHost.callTool("workspace_list", nil)
	if isErr || strings.Contains(wsList, outside) {
		t.Fatalf("--lock-workspaces leaked client root: %s", wsList)
	}

	// Verify .git (case-insensitive) and ../ traversal are blocked.
	for _, badPath := range []string{".git/config", ".GIT/HEAD", "../secret.txt"} {
		msg, errFlag := lockedHost.callTool("read_file", map[string]any{"path": badPath})
		if !errFlag {
			t.Fatalf("expected %q to be blocked, got: %s", badPath, msg)
		}
	}
	if runtime.GOOS != "windows" {
		if msg, errFlag := lockedHost.callTool("read_file", map[string]any{"path": "escape.txt"}); !errFlag {
			t.Fatalf("expected symlink escape to be blocked, got: %s", msg)
		}
	}
}

func TestEndToEndCodingFlowInIsolatedWorktree(t *testing.T) {
	repo := t.TempDir()
	wtStore := t.TempDir()
	initGitRepo(t, repo)

	// Local untracked instruction overlay in main repo should copy into worktree without dirtying it.
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.local.md"), []byte("# Local Instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	host := startTestServer(t, mcpserver.Config{
		Workspaces:  []string{repo},
		WorktreeDir: wtStore,
	}, nil)
	host.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	host.notify("notifications/initialized", nil)

	// 1. Create isolated worktree.
	wtOut, isErr := host.callTool("worktree_create", map[string]any{"name": "fix-add"})
	if isErr {
		t.Fatalf("worktree_create failed: %s", wtOut)
	}

	// 2. Search code inside worktree.
	searchOut, isErr := host.callTool("search_code", map[string]any{
		"worktree": "fix-add",
		"query":    "return a - b",
	})
	if isErr || !strings.Contains(searchOut, "calc.go") {
		t.Fatalf("search_code failed: isErr=%v out=%s", isErr, searchOut)
	}

	// 3. Read calc.go and capture SHA-256 hash.
	readOut, isErr := host.callTool("read_file", map[string]any{
		"worktree": "fix-add",
		"path":     "calc.go",
	})
	if isErr {
		t.Fatalf("read_file failed: %s", readOut)
	}
	var readJSON struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(readOut), &readJSON); err != nil || readJSON.Hash == "" {
		t.Fatalf("missing hash in read_file output: %s", readOut)
	}

	// 4. Stale expected_hash must fail safely.
	if staleMsg, errFlag := host.callTool("edit_file", map[string]any{
		"worktree":      "fix-add",
		"path":          "calc.go",
		"old_string":    "return a - b",
		"new_string":    "return a + b",
		"expected_hash": "0000000000000000000000000000000000000000000000000000000000000000",
	}); !errFlag || !strings.Contains(staleMsg, "changed on disk") {
		t.Fatalf("expected stale hash rejection, got isErr=%v msg=%s", errFlag, staleMsg)
	}

	// 5. Valid edit_file + write_file unit test inside worktree.
	if editMsg, errFlag := host.callTool("edit_file", map[string]any{
		"worktree":      "fix-add",
		"path":          "calc.go",
		"old_string":    "return a - b",
		"new_string":    "return a + b",
		"expected_hash": readJSON.Hash,
	}); errFlag {
		t.Fatalf("edit_file failed: %s", editMsg)
	}
	if writeMsg, errFlag := host.callTool("write_file", map[string]any{
		"worktree": "fix-add",
		"path":     "calc_test.go",
		"content":  "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"wrong sum\")\n\t}\n}\n",
	}); errFlag {
		t.Fatalf("write_file failed: %s", writeMsg)
	}

	// Main workspace calc.go must still be untouched while working in worktree!
	mainCalc, _ := os.ReadFile(filepath.Join(repo, "calc.go"))
	if !strings.Contains(string(mainCalc), "return a - b") {
		t.Fatalf("main workspace was mutated before merge: %s", mainCalc)
	}

	// 6. Run go test inside worktree via run_command.
	execOut, isErr := host.callTool("run_command", map[string]any{
		"worktree": "fix-add",
		"command":  "go test ./...",
	})
	if isErr {
		t.Fatalf("run_command failed: %s", execOut)
	}
	var execJSON struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(execOut), &execJSON); err != nil || execJSON.ExitCode != 0 {
		t.Fatalf("go test inside worktree failed: %s", execOut)
	}

	// 7. Commit inside worktree (leaving untracked AGENTS.local.md unstaged), merge into main, and remove worktree.
	if commitOut, errFlag := host.callTool("git_commit", map[string]any{
		"worktree": "fix-add",
		"message":  "Fix Add implementation and add unit test",
		"paths":    []string{"calc.go", "calc_test.go"},
	}); errFlag {
		t.Fatalf("git_commit failed: %s", commitOut)
	}

	if mergeOut, errFlag := host.callTool("worktree_merge", map[string]any{"name": "fix-add"}); errFlag {
		t.Fatalf("worktree_merge failed: %s", mergeOut)
	}

	if rmOut, errFlag := host.callTool("worktree_remove", map[string]any{"name": "fix-add"}); errFlag {
		t.Fatalf("worktree_remove failed: %s", rmOut)
	}

	mergedCalc, _ := os.ReadFile(filepath.Join(repo, "calc.go"))
	if !strings.Contains(string(mergedCalc), "return a + b") {
		t.Fatalf("merged main workspace missing fix: %s", mergedCalc)
	}
}

func TestContentLengthFramingFallback(t *testing.T) {
	srv, err := mcpserver.New(mcpserver.Config{
		Version:    "test",
		ConfigPath: filepath.Join(t.TempDir(), "mcp.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	if !strings.HasPrefix(out.String(), "Content-Length:") || !strings.Contains(out.String(), `"result":{}`) {
		t.Fatalf("unexpected Content-Length response: %q", out.String())
	}
}

func TestWorkspaceAddPersistsAndExpandsTwoLevelGitRepos(t *testing.T) {
	codeRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "mcp.json")

	// Level 1 Git repo: ~/Space/GitHub/gbx
	repoTop := filepath.Join(codeRoot, "gbx")
	if err := os.MkdirAll(repoTop, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoTop)

	// Level 2 nested Git repos under grouping folder: ~/Space/GitHub/Swifty/{SwiftyKafka,SwiftyRedis}
	swiftyGroup := filepath.Join(codeRoot, "Swifty")
	if err := os.MkdirAll(swiftyGroup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(swiftyGroup, "OVERVIEW.md"), []byte("# Swifty Group\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"SwiftyKafka", "SwiftyRedis"} {
		subDir := filepath.Join(swiftyGroup, sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		initGitRepo(t, subDir)
	}

	// 1. Start zero-arg server (simulating Gemini App without roots/list).
	host1 := startTestServer(t, mcpserver.Config{ConfigPath: cfgPath}, nil)
	host1.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	host1.notify("notifications/initialized", nil)

	// Calling workspace_add(as_root=true) discovers gbx, Swifty, SwiftyKafka, SwiftyRedis and persists mcp.json.
	addOut, isErr := host1.callTool("workspace_add", map[string]any{
		"path":    codeRoot,
		"as_root": true,
	})
	if isErr {
		t.Fatalf("workspace_add failed: %s", addOut)
	}
	for _, want := range []string{"gbx", "SwiftyKafka", "SwiftyRedis", "Swifty"} {
		if !strings.Contains(addOut, want) {
			t.Fatalf("workspace_add output missing %q: %s", want, addOut)
		}
	}

	// Both short name ("SwiftyKafka") and group-qualified ("Swifty/SwiftyKafka") resolve directly as Git repos.
	for _, selector := range []string{"SwiftyKafka", "Swifty/SwiftyKafka"} {
		stOut, errFlag := host1.callTool("git_status", map[string]any{"workspace": selector})
		if errFlag || !strings.Contains(stOut, `"repo": true`) {
			t.Fatalf("git_status(%q) failed: isErr=%v out=%s", selector, errFlag, stOut)
		}
	}

	// 2. Restart a brand-new zero-arg server pointing at the same ConfigPath: workspaces load automatically!
	host2 := startTestServer(t, mcpserver.Config{ConfigPath: cfgPath}, nil)
	host2.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	host2.notify("notifications/initialized", nil)

	listOut, isErr := host2.callTool("workspace_list", nil)
	if isErr || !strings.Contains(listOut, "SwiftyKafka") || !strings.Contains(listOut, "gbx") {
		t.Fatalf("persisted mcp.json did not auto-load on restart: isErr=%v out=%s", isErr, listOut)
	}
}

func TestRunBatchExecutesPipelineAndHaltsOnTestFailure(t *testing.T) {
	repo := t.TempDir()
	wtStore := t.TempDir()
	initGitRepo(t, repo)

	host := startTestServer(t, mcpserver.Config{
		Workspaces:  []string{repo},
		WorktreeDir: wtStore,
	}, nil)
	host.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	host.notify("notifications/initialized", nil)

	// 1. Failing test in step 2 must halt before git_commit in step 3!
	failBatchOut, isErr := host.callTool("run_batch", map[string]any{
		"steps": []map[string]any{
			{
				"tool": "worktree_create",
				"name": "batch-fix",
			},
			{
				"tool":     "write_file",
				"worktree": "batch-fix",
				"path":     "calc_test.go",
				"content":  "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"broken\")\n\t}\n}\n",
			},
			{
				"tool":     "run_command",
				"worktree": "batch-fix",
				"command":  "go test ./...",
			},
			{
				"tool":     "git_commit",
				"worktree": "batch-fix",
				"message":  "must not commit broken code",
				"paths":    []string{"calc_test.go"},
			},
		},
	})
	if isErr {
		t.Fatalf("run_batch returned unexpected error: %s", failBatchOut)
	}
	if !strings.Contains(failBatchOut, `"stopped_early": true`) || !strings.Contains(failBatchOut, `"completed_steps": 3`) {
		t.Fatalf("expected run_batch to halt after failing go test at step 3, got:\n%s", failBatchOut)
	}

	// 2. Second single batch fixes calc.go, passes go test, commits, and merges!
	passBatchOut, isErr := host.callTool("run_batch", map[string]any{
		"steps": []map[string]any{
			{
				"tool":       "edit_file",
				"worktree":   "batch-fix",
				"path":       "calc.go",
				"old_string": "return a - b",
				"new_string": "return a + b",
			},
			{
				"tool":     "run_command",
				"worktree": "batch-fix",
				"command":  "go test ./...",
			},
			{
				"tool":     "git_commit",
				"worktree": "batch-fix",
				"message":  "Fix Add and add test",
				"paths":    []string{"calc.go", "calc_test.go"},
			},
			{
				"tool": "worktree_merge",
				"name": "batch-fix",
			},
		},
	})
	if isErr || !strings.Contains(passBatchOut, `"stopped_early": false`) || !strings.Contains(passBatchOut, `"completed_steps": 4`) {
		t.Fatalf("passing run_batch failed: isErr=%v out=\n%s", isErr, passBatchOut)
	}

	// 3. BatchOnly mode exposes strictly [run_batch] in tools/list while keeping internal tools callable inside run_batch.
	batchOnlyHost := startTestServer(t, mcpserver.Config{
		Workspaces: []string{repo},
		BatchOnly:  true,
	}, nil)
	batchOnlyHost.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	toolsResp := batchOnlyHost.call("tools/list", nil)
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(toolsResp.Result, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "run_batch" {
		t.Fatalf("BatchOnly tools = %+v, want [run_batch]", listed.Tools)
	}
}
