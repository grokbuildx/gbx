package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/grokbuildx/gbx/internal/config"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/providersession"
)

func TestMultiEditRedactsSecretsCapsStringsAndDoesNotMutateRawInput(t *testing.T) {
	nested := map[string]any{"api_key": "sk-super-secret-value-123456"}
	rawInput := map[string]any{"nested": nested}
	oldText := "API_KEY=sk-secret-before-987654\n" + strings.Repeat("a", ToolInputStringLimit+500)
	newText := "Authorization: Bearer super_secret_bearer_token_999\n" + strings.Repeat("b", ToolInputStringLimit+500)
	diffs := []acp.ToolCallContent{
		{Diff: &acp.ToolCallContentDiff{Path: "a.env", OldText: &oldText, NewText: newText}},
		{Diff: &acp.ToolCallContentDiff{Path: "b.env", OldText: &oldText, NewText: newText}},
	}
	name, _, _, input := acpToolPresentationWithMetaCanonical(acp.ToolKindEdit, "MultiEdit", rawInput, nil, diffs, nil, "")
	if name != "MultiEdit" {
		t.Fatalf("name = %q, want MultiEdit", name)
	}
	encoded := string(input)
	if strings.Contains(encoded, "sk-super-secret-value-123456") ||
		strings.Contains(encoded, "sk-secret-before-987654") ||
		strings.Contains(encoded, "super_secret_bearer_token_999") {
		t.Fatalf("secret leaked into MultiEdit tool input: %s", encoded[:min(len(encoded), 400)])
	}
	if nested["api_key"] != "sk-super-secret-value-123456" {
		t.Fatalf("coerceInputMap mutated caller rawInput in place: got %v", nested["api_key"])
	}
	var decoded map[string]any
	if err := json.Unmarshal(input, &decoded); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	diffList, ok := decoded["diffs"].([]any)
	if !ok || len(diffList) != 2 {
		t.Fatalf("unexpected diffs payload: %#v", decoded["diffs"])
	}
	firstDiff := diffList[0].(map[string]any)
	if len([]rune(firstDiff["before"].(string))) > ToolInputStringLimit+20 {
		t.Fatalf("diff before string was not capped: len=%d", len([]rune(firstDiff["before"].(string))))
	}
}

func TestWorkspaceResolveRejectsGitDirectoryAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	ws := Workspace{ID: "w1", Name: "w1", Path: dir, real: real}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []string{".git", ".git/config", ".GIT/config", "sub/.git/hooks/pre-commit"} {
		if _, err := ws.resolve(blocked); err == nil {
			t.Fatalf("expected %q to be rejected", blocked)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "innocent.txt")
		if err := os.Symlink(filepath.Join(dir, ".git", "config"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.resolve("innocent.txt"); err == nil {
			t.Fatal("expected symlink pointing inside .git to be rejected")
		}
	}
}

func TestWriteFileAcceptsPayloadAboveOneMegabyte(t *testing.T) {
	wsDir := t.TempDir()
	server, err := New(Config{Workspace: wsDir, StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })

	large := strings.Repeat("x", (1<<20)+128*1024)
	payload, _ := json.Marshal(map[string]string{
		"path":         "big.txt",
		"content":      large,
		"expectedHash": "",
	})
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/api/files/write", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("writeFile status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(filepath.Join(wsDir, "big.txt"))
	if err != nil || len(saved) != len(large) {
		t.Fatalf("saved len = %d (err=%v), want %d", len(saved), err, len(large))
	}
}

func TestListenUnixStagesSafelyInSharedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("socket permissions are POSIX")
	}
	dir := shortTempDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "shared.sock")
	ln, err := ListenUnix(sockPath)
	if err != nil {
		t.Fatalf("ListenUnix in shared dir failed: %v", err)
	}
	info, err := os.Stat(sockPath)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		_ = ln.Close()
		t.Fatalf("socket mode = %v, want 0600", perm)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("dial staged unix socket failed: %v", err)
	}
	_ = conn.Close()
	<-done
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected staged socket to be removed on Close, err=%v", err)
	}
}

func TestConfigHotAppliesBypassModeAndProbesUnsavedProviderWithoutPersistingLabels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GBX_HOME", filepath.Join(home, ".gbx"))
	t.Cleanup(func() { oauthproxy.ConfigureLogLevel(oauthproxy.LogLevelOff) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"demo-model","name":"Demo Model"}]}`))
	}))
	defer upstream.Close()

	if err := config.Update(func(cfg *provider.Config) error {
		cfg.Providers["saved"] = provider.Provider{
			Name:     "saved",
			Type:     "openai",
			Endpoint: upstream.URL + "/v1",
			APIKey:   "sk-saved",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Config{Workspace: t.TempDir(), StoreRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })

	// 1. Hot-apply bypassMode and logLevel via PUT /api/config.
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/api/config", strings.NewReader(`{"bypassMode":true,"logLevel":"warn"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := server.defaultPermissionMode(); got != "bypassPermissions" {
		t.Fatalf("defaultPermissionMode = %q, want bypassPermissions", got)
	}
	if got := oauthproxy.CurrentLogLevel(); got != oauthproxy.LogLevelWarn {
		t.Fatalf("CurrentLogLevel = %q, want warn", got)
	}

	// 2. Probing an unsaved provider with an endpoint succeeds instead of 404.
	probeBody, _ := json.Marshal(map[string]string{
		"endpoint": upstream.URL + "/v1",
		"apiKey":   "sk-unsaved",
		"type":     "openai",
	})
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/config/providers/new-draft/test", bytes.NewReader(probeBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unsaved probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res ProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK || len(res.Models) == 0 {
		t.Fatalf("unexpected probe result: %+v (err=%v)", res, err)
	}

	// 3. Probing an existing provider with a custom endpoint override must NOT mutate stored ModelLabels.
	altUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"alt-model","name":"Alt Model"}]}`))
	}))
	defer altUpstream.Close()
	customBody, _ := json.Marshal(map[string]string{
		"endpoint": altUpstream.URL + "/v1",
	})
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/config/providers/saved/test", bytes.NewReader(customBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := loaded.Providers["saved"].ModelLabels["alt-model"]; leaked {
		t.Fatalf("custom endpoint probe mutated persisted ModelLabels: %+v", loaded.Providers["saved"].ModelLabels)
	}
}

func TestCancelDuringPrepareRecordsCancelledNotError(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	session, err := store.Create(Meta{Workspace: t.TempDir(), Sandbox: SandboxWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	runner := NewRunner(1)
	runner.Prepare = func(ctx context.Context, _ LaunchSpec) (*exec.Cmd, func(), error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	tl := NewTimeline()
	errCh := make(chan error, 1)
	go func() {
		_, startErr := tl.Start(runner, session, LaunchSpec{Prompt: "hello", Workspace: session.Meta().Workspace, Sandbox: SandboxWorkspace})
		errCh <- startErr
	}()
	<-started
	if !tl.RequestCancel(session.ID()) {
		t.Fatal("expected RequestCancel to match running turn")
	}
	if startErr := <-errCh; startErr != nil {
		t.Fatalf("expected nil error on cancelled Prepare, got %v", startErr)
	}
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var sawResult, sawError bool
	for _, ev := range events {
		if ev.Type == EventError {
			sawError = true
		}
		if ev.Type == EventResult {
			var res ResultPayload
			_ = json.Unmarshal(ev.Payload, &res)
			if res.Cancelled && !res.IsError {
				sawResult = true
			}
		}
	}
	if sawError || !sawResult {
		t.Fatalf("expected cancelled result without error event; sawError=%v sawResult=%v events=%+v", sawError, sawResult, events)
	}
}

func TestRuntimePoolExclusivityAndGenerationInvalidation(t *testing.T) {
	pool := newRuntimePool()
	defer pool.Close()

	started := make(chan struct{})
	releasePrepare := make(chan struct{})
	calls := 0
	pool.prepare = func(ctx context.Context, cfg provider.Provider) (*providersession.Session, error) {
		calls++
		if calls == 1 {
			close(started)
			<-releasePrepare
		}
		return providersession.Prepare(ctx, cfg)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	cfg := provider.Provider{Name: "p1", Type: "openai", Endpoint: upstream.URL + "/v1", APIKey: "sk-1", Model: "m1"}

	// Invalidate while first Acquire is in Prepare; it must not poison the pool.
	type acqRes struct {
		rel func()
		err error
	}
	ch := make(chan acqRes, 1)
	go func() {
		_, rel, _, err := pool.AcquireWithExclusivity(context.Background(), cfg)
		ch <- acqRes{rel: rel, err: err}
	}()
	<-started
	pool.Invalidate("p1")
	close(releasePrepare)
	r1 := <-ch
	if r1.err != nil {
		t.Fatalf("first acquire failed: %v", r1.err)
	}
	defer r1.rel()

	// Next two overlapping acquires should reuse the fresh runtime and mark both non-exclusive.
	_, relA, exclA, err := pool.AcquireWithExclusivity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !exclA() {
		t.Fatal("single active turn should be exclusive")
	}
	_, relB, exclB, err := pool.AcquireWithExclusivity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if exclA() || exclB() {
		t.Fatalf("overlapping turns must not be exclusive: exclA=%v exclB=%v", exclA(), exclB())
	}
	relA()
	relB()

	// Sequential turn after both released must be exclusive again.
	_, relC, exclC, err := pool.AcquireWithExclusivity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !exclC() {
		t.Fatal("sequential turn after overlap ended should be exclusive")
	}
	relC()
}
