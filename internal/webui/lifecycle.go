package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grokbuildx/gbx/internal/fsutil"
	"github.com/grokbuildx/gbx/internal/paths"
)

const (
	// ServeModeRun couples the server lifetime to connected browser tabs.
	ServeModeRun = "run"
	// ServeModeStart runs a persistent background daemon until gbx serve stop.
	ServeModeStart = "start"

	// EnvServeInstanceID carries the expected instance ID into detached _daemon
	// processes started by gbx serve start.
	EnvServeInstanceID = "GBX_SERVE_INSTANCE_ID"
)

// DaemonState describes the active gbx serve instance on disk (0600).
// It intentionally excludes the WebUI API token; ControlToken is scoped solely
// to the loopback control server on ControlURL.
type DaemonState struct {
	ID           string    `json:"id"`
	PID          int       `json:"pid"`
	Mode         string    `json:"mode"`
	URL          string    `json:"url,omitempty"`
	Socket       string    `json:"socket,omitempty"`
	ControlURL   string    `json:"control_url"`
	ControlToken string    `json:"control_token"`
	Workspaces   int       `json:"workspaces"`
	AutoExit     bool      `json:"auto_exit,omitempty"`
	StartedAt    time.Time `json:"started_at"`
}

// LiveStatus is returned by the authenticated loopback /status endpoint.
type LiveStatus struct {
	ID         string    `json:"id"`
	PID        int       `json:"pid"`
	Mode       string    `json:"mode"`
	URL        string    `json:"url,omitempty"`
	Socket     string    `json:"socket,omitempty"`
	Workspaces int       `json:"workspaces"`
	AutoExit   bool      `json:"auto_exit,omitempty"`
	Clients    int       `json:"clients"`
	StartedAt  time.Time `json:"started_at"`
}

// LifecycleManager coordinates gbx serve run/start/stop/status state under Dir
// (default ~/.gbx/serve).
type LifecycleManager struct {
	Dir string
}

// DefaultLifecycleManager returns a LifecycleManager rooted at ~/.gbx/serve.
func DefaultLifecycleManager() (*LifecycleManager, error) {
	dir, err := paths.ServeDir()
	if err != nil {
		return nil, err
	}
	return &LifecycleManager{Dir: dir}, nil
}

func (m *LifecycleManager) StatePath() string       { return filepath.Join(m.Dir, "state.json") }
func (m *LifecycleManager) LogPath() string         { return filepath.Join(m.Dir, "serve.log") }
func (m *LifecycleManager) commandLockPath() string { return filepath.Join(m.Dir, "command.lock") }
func (m *LifecycleManager) daemonLockPath() string  { return filepath.Join(m.Dir, "daemon.lock") }

func (m *LifecycleManager) lockCommand(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	return fsutil.AcquireFileLock(ctx, m.commandLockPath())
}

// TryLockDaemon attempts to acquire the non-blocking daemon lifecycle lock.
// Foreground `gbx serve run` uses this so it can register itself for
// status/stop without colliding with an already running background daemon.
func (m *LifecycleManager) TryLockDaemon() (func(), bool, error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, false, err
	}
	return fsutil.TryFileLock(m.daemonLockPath())
}

// AcquireDaemonLock blocks until the daemon lifecycle lock is held (used by
// detached `_daemon` processes after `start` stops any prior daemon).
func (m *LifecycleManager) AcquireDaemonLock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	return fsutil.AcquireFileLock(ctx, m.daemonLockPath())
}

// Read returns the persisted DaemonState, or (nil, nil) if none exists.
func (m *LifecycleManager) Read() (*DaemonState, error) {
	data, err := fsutil.ReadFile(m.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state DaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("read serve state: %w", err)
	}
	if state.ID == "" || state.ControlURL == "" {
		return nil, nil
	}
	return &state, nil
}

// WriteState atomically persists state.json with mode 0600.
func (m *LifecycleManager) WriteState(state *DaemonState) error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(m.StatePath(), data, 0o600)
}

// RemoveStateIfOwner removes state.json only if it still belongs to instanceID.
func (m *LifecycleManager) RemoveStateIfOwner(instanceID string) {
	if instanceID == "" {
		return
	}
	cur, err := m.Read()
	if err != nil || cur == nil || cur.ID != instanceID {
		return
	}
	_ = os.Remove(m.StatePath())
}

func validateControlEndpoint(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil {
		return fmt.Errorf("invalid local serve control endpoint")
	}
	return nil
}

func controlRequest(ctx context.Context, state *DaemonState, method, path string) ([]byte, error) {
	if state == nil {
		return nil, errors.New("missing serve state")
	}
	if err := validateControlEndpoint(state.ControlURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, state.ControlURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+state.ControlToken)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serve control returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// QueryStatus checks whether state refers to a live gbx serve instance and
// returns its live status.
func (m *LifecycleManager) QueryStatus(ctx context.Context, state *DaemonState) (*LiveStatus, error) {
	if state == nil {
		return nil, nil
	}
	body, err := controlRequest(ctx, state, http.MethodGet, "/status")
	if err != nil {
		return nil, err
	}
	var status LiveStatus
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("decode serve status: %w", err)
	}
	if status.ID != state.ID {
		return nil, fmt.Errorf("serve control identity mismatch")
	}
	return &status, nil
}

// Status returns the live status of the managed gbx serve instance, or
// (nil, nil) if not running. Stale state files left by a crashed process are
// cleaned up automatically.
func (m *LifecycleManager) Status(ctx context.Context) (*LiveStatus, error) {
	state, err := m.Read()
	if err != nil || state == nil {
		return nil, err
	}
	live, err := m.QueryStatus(ctx, state)
	if err == nil && live != nil {
		return live, nil
	}
	// If the daemon lock can be acquired immediately, the recorded process is
	// gone; remove its stale state file.
	if release, ok, lockErr := m.TryLockDaemon(); lockErr == nil && ok {
		defer release()
		m.RemoveStateIfOwner(state.ID)
	}
	return nil, nil
}

// Stop stops the managed gbx serve instance (whether started via run or start)
// and waits until it releases its lifecycle lock. Reports whether an instance
// was running (or stale state was cleaned up).
func (m *LifecycleManager) Stop(ctx context.Context) (bool, *DaemonState, error) {
	releaseCmd, err := m.lockCommand(ctx)
	if err != nil {
		return false, nil, err
	}
	defer releaseCmd()
	return m.stopLocked(ctx)
}

func (m *LifecycleManager) stopLocked(ctx context.Context) (bool, *DaemonState, error) {
	state, err := m.Read()
	if err != nil || state == nil {
		return false, nil, err
	}
	wasRunning := false
	if _, err := m.QueryStatus(ctx, state); err == nil {
		wasRunning = true
		_, _ = controlRequest(ctx, state, http.MethodPost, "/stop")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	releaseDaemon, err := fsutil.AcquireFileLock(waitCtx, m.daemonLockPath())
	if err != nil {
		return wasRunning, state, fmt.Errorf("wait for gbx serve shutdown: %w", err)
	}
	defer releaseDaemon()
	_ = os.Remove(m.StatePath())
	return wasRunning, state, nil
}

// StartControlServer binds an authenticated loopback-only control server on
// 127.0.0.1:0, populates state.ID / ControlURL / ControlToken, and returns a
// shutdown closure.
func StartControlServer(state *DaemonState, clients func() int, onStop func()) (func(), error) {
	if state == nil {
		return nil, errors.New("state is required")
	}
	if state.ID == "" {
		state.ID = rand.Text()
	}
	token := rand.Text()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen serve control server: %w", err)
	}
	state.ControlURL = "http://" + listener.Addr().String()
	state.ControlToken = token

	var stopOnce sync.Once
	triggerStop := func() {
		if onStop != nil {
			stopOnce.Do(func() { go onStop() })
		}
	}

	authorized := func(r *http.Request) bool {
		got, ok := cutPrefixFold(strings.TrimSpace(r.Header.Get("Authorization")), "bearer ")
		if !ok || got == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, state.ID)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		active := 0
		if clients != nil {
			active = clients()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(LiveStatus{
			ID:         state.ID,
			PID:        state.PID,
			Mode:       state.Mode,
			URL:        state.URL,
			Socket:     state.Socket,
			Workspaces: state.Workspaces,
			AutoExit:   state.AutoExit,
			Clients:    active,
			StartedAt:  state.StartedAt,
		})
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, state.ID)
		triggerStop()
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(listener) }()

	var closeOnce sync.Once
	return func() {
		closeOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
	}, nil
}

// StartDetached replaces any running managed gbx serve instance with a detached
// background daemon running `executable args...`, waits until its loopback
// control endpoint responds healthy with the expected instance ID, and returns
// its DaemonState.
func (m *LifecycleManager) StartDetached(ctx context.Context, executable string, args []string, extraEnv []string) (*DaemonState, error) {
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("cannot resolve gbx executable for detached daemon")
	}
	releaseCmd, err := m.lockCommand(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseCmd()

	if _, _, err := m.stopLocked(ctx); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open serve log: %w", err)
	}

	instanceID := rand.Text()
	child := exec.Command(executable, args...)
	detachProcess(child)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = mergeEnv(os.Environ(), append([]string{EnvServeInstanceID + "=" + instanceID}, extraEnv...))

	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start gbx serve daemon: %w", err)
	}
	_ = logFile.Close()

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		state, readErr := m.Read()
		if readErr == nil && state != nil && state.ID == instanceID {
			if _, statusErr := m.QueryStatus(ctx, state); statusErr == nil {
				return state, nil
			}
		}
		select {
		case waitErr := <-exited:
			detail := readLogTail(m.LogPath(), 12)
			if detail != "" {
				return nil, fmt.Errorf("gbx serve daemon exited during startup (%v):\n%s", waitErr, detail)
			}
			return nil, fmt.Errorf("gbx serve daemon exited during startup (%v); see %s", waitErr, m.LogPath())
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			detail := readLogTail(m.LogPath(), 12)
			if detail != "" {
				return nil, fmt.Errorf("timed out waiting for gbx serve daemon:\n%s", detail)
			}
			return nil, fmt.Errorf("timed out waiting for gbx serve daemon; see %s", m.LogPath())
		}
	}
}

func readLogTail(path string, maxLines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func mergeEnv(base, overrides []string) []string {
	drop := make(map[string]bool, len(overrides))
	for _, entry := range overrides {
		if k, _, ok := strings.Cut(entry, "="); ok {
			drop[strings.ToUpper(k)] = true
		}
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		if k, _, ok := strings.Cut(entry, "="); ok && drop[strings.ToUpper(k)] {
			continue
		}
		out = append(out, entry)
	}
	return append(out, overrides...)
}
