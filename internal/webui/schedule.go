package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/grokbuildx/gbx/internal/fsutil"
)

// Scheduled messages: a message the user wants sent later. They survive a
// restart of gbx serve; one whose time passed while it was down goes out when
// it is back. A conversation busy at the time gets it after its turn.

// Schedule is one message waiting for its time.
type Schedule struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

const (
	scheduleTick    = 10 * time.Second
	maxScheduleSpan = 366 * 24 * time.Hour
)

type scheduler struct {
	mu    sync.Mutex
	path  string
	items []Schedule
}

func newScheduler(path string) *scheduler {
	sched := &scheduler{path: path}
	if data, err := fsutil.ReadRegularFileLimited(path, 4<<20); err == nil {
		_ = json.Unmarshal(data, &sched.items)
	}
	return sched
}

func (s *scheduler) saveLocked() error {
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, data, 0o600)
}

func (s *scheduler) forSession(id string) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Schedule{}
	for _, item := range s.items {
		if item.Session == id {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func (s *scheduler) add(item Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	if err := s.saveLocked(); err != nil {
		s.items = s.items[:len(s.items)-1]
		return err
	}
	return nil
}

func (s *scheduler) remove(session, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.items {
		if item.ID == id && (session == "" || item.Session == session) {
			prev := append([]Schedule(nil), s.items...)
			s.items = append(s.items[:i:i], s.items[i+1:]...)
			if err := s.saveLocked(); err != nil {
				s.items = prev
				return false
			}
			return true
		}
	}
	return false
}

func (s *scheduler) removeSession(session string) {
	if session == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]Schedule, 0, len(s.items))
	removed := false
	for _, item := range s.items {
		if item.Session == session {
			removed = true
			continue
		}
		kept = append(kept, item)
	}
	if !removed {
		return
	}
	prev := s.items
	s.items = kept
	if err := s.saveLocked(); err != nil {
		s.items = prev
	}
}

func (s *scheduler) due(now time.Time) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Schedule
	for _, item := range s.items {
		if !item.At.After(now) {
			due = append(due, item)
		}
	}
	return due
}

// runSchedules sends due messages; it runs for the life of the server.
func (s *Server) runSchedules(ctx context.Context) {
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sendDue(time.Now())
		}
	}
}

func (s *Server) sendDue(now time.Time) {
	if s.closing.Load() {
		return
	}
	for _, item := range s.schedules.due(now) {
		session, err := s.store.Open(item.Session)
		if errors.Is(err, ErrNotFound) {
			s.schedules.remove("", item.ID)
			continue
		}
		if err != nil || s.timeline.Running(item.Session) {
			continue // try again on the next tick
		}
		if !s.schedules.remove("", item.ID) {
			continue // cancelled meanwhile
		}
		if _, err := s.startTurn(session, item.Text, nil); errors.Is(err, ErrBusy) {
			// A manual turn started between the Running check and startTurn;
			// put the scheduled message back so it fires on the next tick.
			_ = s.schedules.add(item)
		}
	}
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request, session *Session, id string) {
	switch {
	case id == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"schedules": s.schedules.forSession(session.ID())})
	case id == "" && r.Method == http.MethodPost:
		var body struct {
			At   time.Time `json:"at"`
			Text string    `json:"text"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		now := time.Now()
		if !body.At.After(now) || body.At.After(now.Add(maxScheduleSpan)) {
			writeError(w, http.StatusBadRequest, "the time must be in the future, within a year")
			return
		}
		item := Schedule{ID: uuid.NewString(), Session: session.ID(), At: body.At.UTC(), Text: text}
		if err := s.schedules.add(item); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, item)
	case id != "" && r.Method == http.MethodDelete:
		if !s.schedules.remove(session.ID(), id) {
			writeError(w, http.StatusNotFound, "no such scheduled message")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func schedulePath(storeRoot string) string {
	return filepath.Join(storeRoot, "schedules.json")
}
