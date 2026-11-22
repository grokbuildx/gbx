package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openPresenceStream(t *testing.T, baseURL, token string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/presence?token="+token, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("presence status = %d, want 200", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "event: connected") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("expected connected event, got %q err=%v", line, err)
	}
	return resp, cancel
}

func TestPresenceAutoExitAfterLastTabCloses(t *testing.T) {
	exited := make(chan struct{}, 1)
	srv, err := New(Config{
		Token:             "tok-presence",
		RequireToken:      true,
		Workspaces:        []string{t.TempDir()},
		StoreRoot:         t.TempDir(),
		AutoExitGrace:     80 * time.Millisecond,
		PresenceHeartbeat: 20 * time.Millisecond,
		OnAutoExit: func() {
			select {
			case exited <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close(context.Background()) }()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Auth and method guards.
	badResp, err := http.Get(ts.URL + "/api/presence?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", badResp.StatusCode)
	}
	postReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/presence?token=tok-presence", nil)
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", postResp.StatusCode)
	}

	// Open two tabs.
	r1, cancel1 := openPresenceStream(t, ts.URL, "tok-presence")
	r2, cancel2 := openPresenceStream(t, ts.URL, "tok-presence")
	if got := srv.PresenceCount(); got != 2 {
		t.Fatalf("PresenceCount = %d, want 2", got)
	}

	// Closing tab 1 leaves tab 2 open: must not auto-exit.
	cancel1()
	_ = r1.Body.Close()
	select {
	case <-exited:
		t.Fatal("auto-exit fired while second tab was still open")
	case <-time.After(180 * time.Millisecond):
	}
	if got := srv.PresenceCount(); got != 1 {
		t.Fatalf("PresenceCount after closing tab 1 = %d, want 1", got)
	}

	// Closing last tab triggers OnAutoExit after grace.
	cancel2()
	_ = r2.Body.Close()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("expected OnAutoExit after last tab closed")
	}
}

func TestPresenceReloadSurvivesGraceWindow(t *testing.T) {
	var fired atomic.Int32
	exited := make(chan struct{}, 1)
	srv, err := New(Config{
		Token:             "tok-reload",
		RequireToken:      true,
		Workspaces:        []string{t.TempDir()},
		StoreRoot:         t.TempDir(),
		AutoExitGrace:     180 * time.Millisecond,
		PresenceHeartbeat: 20 * time.Millisecond,
		OnAutoExit: func() {
			fired.Add(1)
			select {
			case exited <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close(context.Background()) }()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Tab connects then closes as in a page refresh, reconnecting before grace ends.
	r1, cancel1 := openPresenceStream(t, ts.URL, "tok-reload")
	cancel1()
	_ = r1.Body.Close()
	time.Sleep(40 * time.Millisecond)

	r2, cancel2 := openPresenceStream(t, ts.URL, "tok-reload")
	time.Sleep(260 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("auto-exit fired during page reload (%d)", fired.Load())
	}
	if got := srv.PresenceCount(); got != 1 {
		t.Fatalf("PresenceCount = %d, want 1", got)
	}

	// Now actually close the reloaded tab.
	cancel2()
	_ = r2.Body.Close()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("expected OnAutoExit after reloaded tab closed")
	}
}

func TestPresenceShutdownEmitsShutdownEventWithoutAutoExit(t *testing.T) {
	var fired atomic.Int32
	srv, err := New(Config{
		Token:             "tok-shut",
		RequireToken:      true,
		Workspaces:        []string{t.TempDir()},
		StoreRoot:         t.TempDir(),
		AutoExitGrace:     50 * time.Millisecond,
		PresenceHeartbeat: 20 * time.Millisecond,
		OnAutoExit:        func() { fired.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/presence?token=tok-shut", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "event: connected") {
		t.Fatalf("first line = %q err=%v", first, err)
	}

	if err := srv.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var sawShutdown bool
	for i := 0; i < 10; i++ {
		line, readErr := reader.ReadString('\n')
		if strings.Contains(line, "event: shutdown") {
			sawShutdown = true
			break
		}
		if readErr != nil {
			break
		}
	}
	if !sawShutdown {
		t.Fatal("expected event: shutdown when server closes")
	}
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("OnAutoExit fired %d times during manual Close", fired.Load())
	}
}

func TestLifecycleManagerControlStatusStopAndStaleCleanup(t *testing.T) {
	mgr := &LifecycleManager{Dir: t.TempDir()}

	// Acquire daemon lock as a running instance would.
	releaseLock, ok, err := mgr.TryLockDaemon()
	if err != nil || !ok {
		t.Fatalf("TryLockDaemon = (%v, %v)", ok, err)
	}
	stopCalled := make(chan struct{}, 1)
	clients := atomic.Int32{}
	clients.Store(2)
	state := &DaemonState{
		PID:        os.Getpid(),
		Mode:       ServeModeStart,
		URL:        "http://127.0.0.1:43210/",
		Workspaces: 3,
		StartedAt:  time.Now().UTC(),
	}
	closeCtrl, err := StartControlServer(state, func() int { return int(clients.Load()) }, func() {
		select {
		case stopCalled <- struct{}{}:
		default:
		}
		releaseLock()
	})
	if err != nil {
		releaseLock()
		t.Fatal(err)
	}
	defer closeCtrl()
	if err := mgr.WriteState(state); err != nil {
		t.Fatal(err)
	}

	// Unauthorized access to control server must fail.
	unauth, err := http.Get(state.ControlURL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth control status = %d, want 401", unauth.StatusCode)
	}

	// Status returns live client count and fields.
	live, err := mgr.Status(context.Background())
	if err != nil || live == nil {
		t.Fatalf("Status = (%+v, %v)", live, err)
	}
	if live.Clients != 2 || live.Workspaces != 3 || live.Mode != ServeModeStart || live.URL != state.URL {
		t.Fatalf("unexpected live status: %+v", live)
	}

	// Stop triggers remote stop and waits for daemon lock release.
	stopped, prev, err := mgr.Stop(context.Background())
	if err != nil || !stopped || prev == nil || prev.ID != state.ID {
		t.Fatalf("Stop = (%v, %+v, %v)", stopped, prev, err)
	}
	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("control /stop did not invoke onStop callback")
	}
	if _, err := os.Stat(mgr.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("expected state.json removed after Stop, stat err=%v", err)
	}

	// Stale state.json (no daemon running, unreachable control port) is cleaned up by Status and Stop.
	stale := &DaemonState{
		ID:           "stale-id",
		PID:          999999,
		Mode:         ServeModeStart,
		URL:          "http://127.0.0.1:1/",
		ControlURL:   "http://127.0.0.1:1",
		ControlToken: "dead-token",
	}
	if err := mgr.WriteState(stale); err != nil {
		t.Fatal(err)
	}
	live, err = mgr.Status(context.Background())
	if err != nil || live != nil {
		t.Fatalf("stale Status = (%+v, %v), want (nil, nil)", live, err)
	}
	if _, err := os.Stat(mgr.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("Status did not clean up stale state.json: %v", err)
	}

	if err := mgr.WriteState(stale); err != nil {
		t.Fatal(err)
	}
	stopped, prev, err = mgr.Stop(context.Background())
	if err != nil || stopped || prev == nil || prev.ID != "stale-id" {
		t.Fatalf("stale Stop = (%v, %+v, %v)", stopped, prev, err)
	}
	if _, err := os.Stat(mgr.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("Stop did not clean up stale state.json: %v", err)
	}
}
