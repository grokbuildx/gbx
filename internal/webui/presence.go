package webui

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultAutoExitGrace is how long gbx serve run waits after the last
	// browser tab disconnects before stopping, so normal page reloads survive.
	DefaultAutoExitGrace = 4 * time.Second
	defaultPresencePing  = 20 * time.Second
)

// presenceTracker tracks active authenticated browser tabs connected to
// /api/presence and optionally triggers auto-exit when the last tab closes.
type presenceTracker struct {
	mu            sync.Mutex
	active        int
	everConnected bool
	grace         time.Duration
	heartbeat     time.Duration
	onAutoExit    func()
	timer         *time.Timer
	closed        bool
	fired         bool
}

func newPresenceTracker(grace, heartbeat time.Duration, onAutoExit func()) *presenceTracker {
	if heartbeat <= 0 {
		heartbeat = defaultPresencePing
	}
	return &presenceTracker{
		grace:      grace,
		heartbeat:  heartbeat,
		onAutoExit: onAutoExit,
	}
}

func (p *presenceTracker) connect() func() {
	p.mu.Lock()
	p.active++
	p.everConnected = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if p.active > 0 {
				p.active--
			}
			if p.active == 0 && p.everConnected && p.grace > 0 && p.onAutoExit != nil && !p.closed && !p.fired {
				if p.timer != nil {
					p.timer.Stop()
				}
				p.timer = time.AfterFunc(p.grace, p.fire)
			}
			p.mu.Unlock()
		})
	}
}

func (p *presenceTracker) fire() {
	p.mu.Lock()
	if p.closed || p.fired || p.active > 0 || !p.everConnected || p.onAutoExit == nil {
		p.timer = nil
		p.mu.Unlock()
		return
	}
	p.fired = true
	p.timer = nil
	cb := p.onAutoExit
	p.mu.Unlock()
	cb()
}

func (p *presenceTracker) close() {
	p.mu.Lock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
}

func (p *presenceTracker) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

func (p *presenceTracker) autoExitEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grace > 0 && p.onAutoExit != nil
}

// PresenceCount reports the number of currently connected browser tabs.
func (s *Server) PresenceCount() int {
	if s == nil || s.presence == nil {
		return 0
	}
	return s.presence.count()
}

// AutoExitEnabled reports whether closing the last browser tab stops this server.
func (s *Server) AutoExitEnabled() bool {
	if s == nil || s.presence == nil {
		return false
	}
	return s.presence.autoExitEnabled()
}

// handlePresence holds a lightweight SSE stream open for each authenticated
// browser tab so gbx serve run can detect when the browser closes and gbx serve
// status can report connected tabs.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) && !s.queryAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.closing.Load() {
		writeError(w, http.StatusServiceUnavailable, ErrClosing.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	disconnect := s.presence.connect()
	defer disconnect()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "event: connected\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(s.presence.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			_, _ = fmt.Fprint(w, "event: shutdown\ndata: {}\n\n")
			flusher.Flush()
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
