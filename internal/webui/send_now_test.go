package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func waitAgentSession(t *testing.T, session *Session) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if id := session.Meta().AgentSessionID; id != "" {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent session was not initialized")
	return ""
}

func TestMessageSendNowStopsAndResumesWithAttachments(t *testing.T) {
	slow, fast := helperPrepare(t, "", true), helperPrepare(t, "sent now", false)
	var mu sync.Mutex
	var specs []LaunchSpec
	launcher := func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		mu.Lock()
		specs = append(specs, spec)
		mu.Unlock()
		if spec.Prompt == "one" {
			return slow(ctx, spec)
		}
		return fast(ctx, spec)
	}
	workspace := t.TempDir()
	server, err := New(Config{Workspace: workspace, StoreRoot: t.TempDir(), Token: "secret", Launch: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	httpServer := registerHTTPTestServer(t, server.Handler())
	session, err := server.store.Create(Meta{Title: "send now", Workspace: workspace, Model: "test-model", Effort: "high", PermissionMode: "plan", Sandbox: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httpServer.URL + "/api/sessions/" + session.ID()
	if status, body := doJSON(t, "POST", endpoint+"/messages", "secret", `{"text":"one"}`); status != http.StatusAccepted {
		t.Fatalf("first=%d %s", status, body)
	}
	agentID := waitAgentSession(t, session)
	if status, body := doJSON(t, "POST", endpoint+"/messages", "secret", `{"text":"invalid","steer":true,"attachments":["missing"]}`); status != http.StatusBadRequest {
		t.Fatalf("invalid attachment = %d: %s", status, body)
	}
	if !server.timeline.Running(session.ID()) {
		t.Fatal("invalid send now stopped the current turn")
	}

	status, files, raw := upload(t, endpoint+"/uploads", map[string][]byte{"notes.txt": []byte("test attachment"), "image.png": pngBytes(t)})
	if status != http.StatusCreated {
		t.Fatalf("upload=%d %s", status, raw)
	}
	ids := []string{files[0].ID, files[1].ID}
	body, _ := json.Marshal(map[string]any{"text": "two", "attachments": ids, "steer": true})
	if status, raw := doJSON(t, "POST", endpoint+"/messages", "secret", string(body)); status != http.StatusAccepted {
		t.Fatalf("send now=%d %s", status, raw)
	}
	waitIdle(t, httpServer.URL, session.ID())
	events, err := session.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	users, results := 0, 0
	for _, event := range events {
		switch event.Type {
		case EventUser:
			users++
			var user UserPayload
			_ = json.Unmarshal(event.Payload, &user)
			if users == 2 && (user.Text != "two" || len(user.Attachments) != 2 || results != 1) {
				t.Fatalf("second user=%+v, preceding results=%d", user, results)
			}
		case EventResult:
			results++
			var result ResultPayload
			_ = json.Unmarshal(event.Payload, &result)
			if result.IsError || result.Cancelled != (results == 1) {
				t.Fatalf("result %d=%+v", results, result)
			}
		}
	}
	if users != 2 || results != 2 {
		t.Fatalf("users/results=%d/%d", users, results)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(specs) != 2 {
		t.Fatalf("launches=%d", len(specs))
	}
	got := specs[1]
	if got.Resume != agentID || got.PermissionMode != "plan" || got.Sandbox != "workspace" || got.Model != "test-model" || got.Effort != "high" || len(got.Attachments) != 2 {
		t.Fatalf("replacement spec=%+v", got)
	}
	if session.Meta().AgentSessionID != agentID {
		t.Fatal("agent identity changed")
	}
}

func TestSendNowReservationAndCancellation(t *testing.T) {
	for _, cancelKind := range []string{"request", "stop", "shutdown"} {
		t.Run(cancelKind, func(t *testing.T) {
			session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			timeline := NewTimeline()
			old := &turn{done: make(chan struct{}), ended: make(chan struct{}), abort: make(chan struct{})}
			timeline.running[session.ID()] = old
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			outcome := make(chan error, 1)
			go func() { _, err := timeline.StartNow(ctx, nil, session, LaunchSpec{Prompt: "next"}); outcome <- err }()
			select {
			case <-old.abort:
			case <-time.After(time.Second):
				t.Fatal("current turn was not cancelled")
			}
			if !timeline.Running(session.ID()) {
				t.Fatal("replacement must remain busy")
			}
			if _, err := timeline.Start(nil, session, LaunchSpec{}); !errors.Is(err, ErrBusy) {
				t.Fatalf("ordinary start=%v", err)
			}
			if _, err := timeline.StartNow(context.Background(), nil, session, LaunchSpec{}); !errors.Is(err, ErrBusy) {
				t.Fatalf("second send now=%v", err)
			}
			switch cancelKind {
			case "request":
				cancel()
			case "stop":
				timeline.RequestCancel(session.ID())
			case "shutdown":
				timeline.CancelAll()
			}
			select {
			case err := <-outcome:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("replacement did not cancel")
			}
			timeline.mu.Lock()
			n := len(timeline.replacing)
			timeline.mu.Unlock()
			if n != 0 {
				t.Fatal("reservation leaked")
			}
			events, _ := session.Events(0)
			if len(events) != 0 {
				t.Fatalf("unsent message was stored: %+v", events)
			}
		})
	}
}

func TestSendNowAfterIdleAndSetupFailure(t *testing.T) {
	session, err := newTestStore(t).Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	timeline := NewTimeline()
	broken := newTestRunner(t, func(context.Context, LaunchSpec) (*exec.Cmd, func(), error) {
		return nil, nil, errors.New("fake launch failure")
	})
	if _, err := timeline.StartNow(context.Background(), broken, session, LaunchSpec{Prompt: "fails"}); err == nil {
		t.Fatal("expected launch failure")
	}
	if timeline.Running(session.ID()) {
		t.Fatal("failed launch stayed busy")
	}
	working := newTestRunner(t, helperPrepare(t, "retry works", false))
	handle, err := timeline.StartNow(context.Background(), working, session, LaunchSpec{Prompt: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, handle.Done)
	if timeline.Running(session.ID()) {
		t.Fatal("completed turn stayed busy")
	}
}

func TestSendNowReplacesTurnWaitingForSlot(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(Meta{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	slow, fast := helperPrepare(t, "", true), helperPrepare(t, "ready", false)
	runner := NewRunner(1)
	runner.Prepare = func(ctx context.Context, spec LaunchSpec) (*exec.Cmd, func(), error) {
		if spec.Prompt == "hold" {
			return slow(ctx, spec)
		}
		return fast(ctx, spec)
	}
	timeline := NewTimeline()
	active, err := timeline.Start(runner, first, LaunchSpec{Prompt: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Cancel()
	queued, err := timeline.Start(runner, second, LaunchSpec{Prompt: "old"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := timeline.StartNow(context.Background(), runner, second, LaunchSpec{Prompt: "new"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, queued.Done)
	active.Cancel()
	waitDone(t, active.Done)
	waitDone(t, next.Done)
	events, err := second.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var results []ResultPayload
	for _, event := range events {
		if event.Type == EventResult {
			var result ResultPayload
			_ = json.Unmarshal(event.Payload, &result)
			results = append(results, result)
		}
	}
	if len(results) != 2 || !results[0].Cancelled || results[1].Cancelled || results[1].IsError {
		t.Fatalf("results=%+v", results)
	}
}
