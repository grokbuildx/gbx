// Package webui serves gbx conversations over HTTP and Server-Sent Events.
//
// A conversation is a series of gbx-launched Grok Build ACP processes that
// share one ACP session id and are resumed with LoadSession. Everything about
// the conversation (its provider, model, effort, history) lives under
// ~/.gbx/serve/sessions; the user's ~/.gbx/config.yaml is only read unless the
// settings UI updates it through config.Update.
package webui

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/grokbuildx/gbx/internal/fsutil"
	"github.com/grokbuildx/gbx/internal/paths"
)

// Usage is token, cost and time accounting, for one turn or a whole
// conversation.
type Usage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
	DurationMs   int64   `json:"durationMs"`
}

// Meta is the persisted description of one conversation.
type Meta struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	// PermissionMode is the permission mode of the next turn; empty uses
	// the server default (bypassPermissions when gbx's bypass mode is on).
	PermissionMode string `json:"permissionMode,omitempty"`
	// Sandbox is pinned at creation; legacy conversations use workspace.
	Sandbox string `json:"sandbox,omitempty"`
	// SandboxProfile pins the resolved Grok profile before the first launch.
	SandboxProfile string `json:"sandboxProfile,omitempty"`
	// AgentSessionID is the ACP session id Grok Build created for this
	// conversation. Subsequent turns load it instead of starting a new session.
	AgentSessionID string `json:"agentSessionId,omitempty"`
	Workspace      string `json:"workspace"`
	// Worktree is set when the conversation runs on a branch of its own in
	// a git worktree of Workspace (worktree.go).
	Worktree *SessionWorktree `json:"worktree,omitempty"`
	// Pinned and Archived only change where the page lists the conversation.
	Pinned   bool `json:"pinned,omitempty"`
	Archived bool `json:"archived,omitempty"`
	// ShareToken opens the conversation read-only at /share/<token>.
	ShareToken string    `json:"shareToken,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Turns counts the messages sent, so a conversation list needs no history.
	Turns int `json:"turns"`
	// Usage is the conversation total: the sum of every turn's TurnUsage.
	Usage Usage `json:"usage"`
	// SessionCostUSD is the running cost the agent last reported for the
	// session. The next turn's cost is the difference from it.
	SessionCostUSD float64 `json:"sessionCostUsd,omitempty"`
}

// recordTurn adds one turn's result to the conversation totals and returns
// what the turn alone spent.
func (m *Meta) recordTurn(reported Usage) Usage {
	turn := reported
	switch {
	case reported.CostUSD >= m.SessionCostUSD:
		turn.CostUSD = reported.CostUSD - m.SessionCostUSD
	case reported.CostUSD == 0:
		// A crash or startup-error result carries zeroed values; the session's
		// running total has not really gone back to zero.
	default:
		// The running total restarted (a new or cleared session).
	}
	if reported.CostUSD > 0 {
		m.SessionCostUSD = reported.CostUSD
	}
	m.Usage.InputTokens += turn.InputTokens
	m.Usage.OutputTokens += turn.OutputTokens
	m.Usage.CostUSD += turn.CostUSD
	m.Usage.DurationMs += turn.DurationMs
	return turn
}

// Event is one persisted entry of a conversation's history. Seq is strictly
// increasing within a conversation, so a client reconnecting with ?since= can
// ask for exactly what it missed.
type Event struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	TS      int64           `json:"ts"`
}

const (
	// TitleLimit is the rune count of the title derived from the first message.
	TitleLimit = 40
	// maxEventLine bounds one events.jsonl line so a corrupt write cannot make
	// the reader allocate unbounded memory.
	maxEventLine = 8 << 20
	// sessionTailCapacity bounds how many recent events stay cached in memory
	// per open session so live SSE delivery never re-scans events.jsonl.
	sessionTailCapacity = 2048
	// maxOpenSessions bounds how many idle sessions stay pinned in Store.open.
	maxOpenSessions = 128
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ErrNotFound reports a conversation that does not exist on disk.
var ErrNotFound = errors.New("session not found")

// Store is the on-disk conversation repository:
//
//	<root>/<id>/meta.json
//	<root>/<id>/events.jsonl
type Store struct {
	root string

	// OnAppend is called after a session stores an event. The server uses it to
	// wake open SSE streams.
	OnAppend func(sessionID string)
	// CanEvict reports whether an open session is idle (not running and has no
	// active SSE subscribers) and may be evicted from Store.open.
	CanEvict func(sessionID string) bool

	mu              sync.Mutex
	open            map[string]*Session
	metaIndex       map[string]Meta
	shareIndex      map[string]string // shareToken -> sessionID
	indexLoaded     bool
	indexDirModTime time.Time
	gen             uint64
}

// NewStore creates root when needed and returns a store over it.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create web session directory: %w", err)
	}
	return &Store{
		root:       root,
		open:       make(map[string]*Session),
		metaIndex:  make(map[string]Meta),
		shareIndex: make(map[string]string),
	}, nil
}

// DefaultRoot is where gbx serve keeps conversations.
func DefaultRoot() (string, error) {
	return paths.ServeSessionsDir()
}

// Root is the directory holding every conversation.
func (s *Store) Root() string { return s.root }

// Close releases cached append file handles for all open sessions.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.open))
	for _, session := range s.open {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	for _, session := range sessions {
		session.closeAppendFile()
	}
}

func (s *Store) ensureIndex() error {
	for attempt := 0; attempt < 8; attempt++ {
		info, statErr := os.Stat(s.root)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				s.mu.Lock()
				s.metaIndex = make(map[string]Meta)
				s.shareIndex = make(map[string]string)
				s.indexLoaded = true
				s.indexDirModTime = time.Time{}
				s.mu.Unlock()
				return nil
			}
			return fmt.Errorf("read web sessions: %w", statErr)
		}
		dirModTime := info.ModTime()

		s.mu.Lock()
		if s.indexLoaded && dirModTime.Equal(s.indexDirModTime) {
			s.mu.Unlock()
			return nil
		}
		startGen := s.gen
		openSnapshot := make(map[string]*Session, len(s.open))
		for id, sess := range s.open {
			openSnapshot[id] = sess
		}
		s.mu.Unlock()

		entries, err := os.ReadDir(s.root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read web sessions: %w", err)
		}
		metas := make(map[string]Meta, len(entries))
		shares := make(map[string]string)
		for _, entry := range entries {
			if !entry.IsDir() || !sessionIDPattern.MatchString(entry.Name()) {
				continue
			}
			id := entry.Name()
			if sess, ok := openSnapshot[id]; ok {
				meta := sess.Meta()
				metas[id] = meta
				if meta.ShareToken != "" {
					shares[meta.ShareToken] = id
				}
				continue
			}
			dir := filepath.Join(s.root, id)
			meta, err := readMeta(dir)
			if err != nil {
				// A half-written directory must not hide the other conversations.
				continue
			}
			if meta.Turns == 0 {
				// Count turns of a legacy conversation only when events.jsonl has data,
				// without pinning empty conversations in s.open.
				if evInfo, err := os.Stat(filepath.Join(dir, "events.jsonl")); err == nil && evInfo.Size() > 0 {
					if events, err := readEvents(dir, 0); err == nil {
						if turns := countTurns(events); turns > 0 {
							meta.Turns = turns
							_ = writeMeta(dir, meta)
						}
					}
				}
			}
			metas[id] = meta
			if meta.ShareToken != "" {
				shares[meta.ShareToken] = id
			}
		}

		s.mu.Lock()
		if s.gen != startGen {
			// A concurrent Create/Delete invalidated this scan; retry so a
			// deleted conversation is never resurrected into metaIndex.
			s.mu.Unlock()
			continue
		}
		// Merge any live open sessions updated concurrently while scanning.
		for id, sess := range s.open {
			meta := sess.Meta()
			if old, exists := metas[id]; exists && old.ShareToken != "" && old.ShareToken != meta.ShareToken {
				delete(shares, old.ShareToken)
			}
			metas[id] = meta
			if meta.ShareToken != "" {
				shares[meta.ShareToken] = id
			}
		}
		s.metaIndex = metas
		s.shareIndex = shares
		s.indexLoaded = true
		s.indexDirModTime = dirModTime
		s.mu.Unlock()
		return nil
	}
	return nil
}

func (s *Store) recordMeta(session *Session, meta Meta) {
	if s == nil || meta.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		deleted := session.deleted
		session.mu.Unlock()
		if deleted {
			return
		}
		if cur, ok := s.open[meta.ID]; ok && cur != session {
			return
		}
	}
	if s.indexLoaded {
		if _, exists := s.metaIndex[meta.ID]; !exists && s.open[meta.ID] != session {
			return
		}
	}
	if s.metaIndex == nil {
		s.metaIndex = make(map[string]Meta)
	}
	if s.shareIndex == nil {
		s.shareIndex = make(map[string]string)
	}
	if prev, ok := s.metaIndex[meta.ID]; ok && prev.ShareToken != "" && prev.ShareToken != meta.ShareToken {
		delete(s.shareIndex, prev.ShareToken)
	}
	s.metaIndex[meta.ID] = meta
	if meta.ShareToken != "" {
		s.shareIndex[meta.ShareToken] = meta.ID
	}
}

func (s *Store) refreshDirModTimeLocked() {
	if info, err := os.Stat(s.root); err == nil {
		s.indexDirModTime = info.ModTime()
	}
}

func (s *Store) maybeEvictIdle(keepID string) {
	s.mu.Lock()
	if len(s.open) <= maxOpenSessions {
		s.mu.Unlock()
		return
	}
	type candidate struct {
		id         string
		sess       *Session
		lastAccess time.Time
	}
	cands := make([]candidate, 0, len(s.open))
	for id, sess := range s.open {
		if id == keepID {
			continue
		}
		sess.mu.Lock()
		la := sess.lastAccess
		busy := sess.deleting
		sess.mu.Unlock()
		if !busy {
			cands = append(cands, candidate{id: id, sess: sess, lastAccess: la})
		}
	}
	s.mu.Unlock()

	sort.Slice(cands, func(i, j int) bool {
		return cands[i].lastAccess.Before(cands[j].lastAccess)
	})
	canEvict := s.CanEvict
	var eligible []candidate
	for _, c := range cands {
		if canEvict == nil || canEvict(c.id) {
			eligible = append(eligible, c)
		}
	}

	var evicted []*Session
	s.mu.Lock()
	for _, c := range eligible {
		if len(s.open) <= maxOpenSessions {
			break
		}
		if s.open[c.id] != c.sess {
			continue
		}
		delete(s.open, c.id)
		evicted = append(evicted, c.sess)
	}
	s.mu.Unlock()
	for _, sess := range evicted {
		sess.closeAppendFile()
	}
}

// Count returns the number of conversations without sorting or copying all metadata.
func (s *Store) Count() int {
	if err := s.ensureIndex(); err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.metaIndex)
}

// FindByShareToken resolves a share token to its conversation id in O(1).
func (s *Store) FindByShareToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	if err := s.ensureIndex(); err != nil {
		return "", false
	}
	s.mu.Lock()
	id, ok := s.shareIndex[token]
	var stored string
	if ok {
		stored = s.metaIndex[id].ShareToken
	}
	s.mu.Unlock()
	if !ok || stored == "" || subtle.ConstantTimeCompare([]byte(stored), []byte(token)) != 1 {
		return "", false
	}
	return id, true
}

// List returns every conversation, most recently updated first.
func (s *Store) List() ([]Meta, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	metas := make([]Meta, 0, len(s.metaIndex))
	for _, meta := range s.metaIndex {
		metas = append(metas, meta)
	}
	s.mu.Unlock()
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].ID < metas[j].ID
		}
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})
	return metas, nil
}

// Create writes a new conversation and returns it.
func (s *Store) Create(meta Meta) (*Session, error) {
	if meta.ID == "" {
		meta.ID = uuid.NewString()
	}
	if !sessionIDPattern.MatchString(meta.ID) {
		return nil, fmt.Errorf("invalid session id %q", meta.ID)
	}
	now := time.Now().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.UpdatedAt = now
	meta.Title = strings.TrimSpace(meta.Title)

	dir := filepath.Join(s.root, meta.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	if err := writeMeta(dir, meta); err != nil {
		return nil, err
	}
	session, err := s.openSession(dir, meta)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.gen++
	s.open[meta.ID] = session
	if s.indexLoaded {
		if prev, ok := s.metaIndex[meta.ID]; ok && prev.ShareToken != "" && prev.ShareToken != meta.ShareToken {
			delete(s.shareIndex, prev.ShareToken)
		}
		s.metaIndex[meta.ID] = session.meta
		if session.meta.ShareToken != "" {
			s.shareIndex[session.meta.ShareToken] = meta.ID
		}
		s.refreshDirModTimeLocked()
	}
	s.mu.Unlock()
	s.maybeEvictIdle(meta.ID)
	return session, nil
}

// Open returns the conversation with that id.
func (s *Store) Open(id string) (*Session, error) {
	if !sessionIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	if session, ok := s.open[id]; ok {
		session.mu.Lock()
		session.lastAccess = time.Now()
		session.mu.Unlock()
		s.mu.Unlock()
		return session, nil
	}
	startGen := s.gen
	s.mu.Unlock()

	dir := filepath.Join(s.root, id)
	meta, err := readMeta(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session, err := s.openSession(dir, meta)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if existing, ok := s.open[id]; ok {
		existing.mu.Lock()
		existing.lastAccess = time.Now()
		existing.mu.Unlock()
		s.mu.Unlock()
		return existing, nil
	}
	if s.gen != startGen {
		if _, statErr := os.Stat(filepath.Join(dir, "meta.json")); statErr != nil {
			s.mu.Unlock()
			return nil, ErrNotFound
		}
	}
	s.open[id] = session
	if s.indexLoaded {
		s.metaIndex[id] = session.meta
		if session.meta.ShareToken != "" {
			s.shareIndex[session.meta.ShareToken] = id
		}
	}
	s.mu.Unlock()
	s.maybeEvictIdle(id)
	return session, nil
}

func (s *Store) openSession(dir string, meta Meta) (*Session, error) {
	session := &Session{id: meta.ID, dir: dir, meta: meta, store: s, tailComplete: true, lastAccess: time.Now()}
	events, err := readEvents(dir, 0)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		session.seq = events[len(events)-1].Seq
		if len(events) > sessionTailCapacity {
			session.tail = append([]Event(nil), events[len(events)-sessionTailCapacity:]...)
			session.tailComplete = false
		} else {
			session.tail = events
		}
	}
	// A conversation from before the turn counter is counted once, from the
	// history this read already loaded, and keeps the count.
	if meta.Turns == 0 {
		if turns := countTurns(events); turns > 0 {
			session.meta.Turns = turns
			_ = writeMeta(dir, session.meta)
		}
	}
	return session, nil
}

func countTurns(events []Event) int {
	turns := 0
	for _, event := range events {
		if event.Type == EventUser {
			turns++
		}
	}
	return turns
}

// Delete removes a conversation and its history.
func (s *Store) Delete(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return ErrNotFound
	}
	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	s.mu.Lock()
	session := s.open[id]
	delete(s.open, id)
	if prev, ok := s.metaIndex[id]; ok {
		if prev.ShareToken != "" {
			delete(s.shareIndex, prev.ShareToken)
		}
		delete(s.metaIndex, id)
	}
	s.gen++
	if session != nil {
		session.mu.Lock()
		session.deleted = true
		if session.appendFile != nil {
			_ = session.appendFile.Close()
			session.appendFile = nil
		}
		session.mu.Unlock()
	}
	s.mu.Unlock()

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	s.mu.Lock()
	if s.indexLoaded {
		s.refreshDirModTimeLocked()
	}
	s.mu.Unlock()
	return nil
}

// Session is one open conversation: its metadata and append-only history.
type Session struct {
	id    string
	dir   string
	store *Store

	mu           sync.Mutex
	meta         Meta
	seq          int64
	tail         []Event
	tailComplete bool
	appendFile   *os.File
	lastAccess   time.Time
	deleting     bool
	deleted      bool
}

func (s *Session) beginDelete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return ErrNotFound
	}
	if s.deleting {
		return ErrBusy
	}
	s.deleting = true
	return nil
}

func (s *Session) cancelDelete() {
	s.mu.Lock()
	s.deleting = false
	s.mu.Unlock()
}

func (s *Session) closeAppendFile() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.appendFile != nil {
		_ = s.appendFile.Close()
		s.appendFile = nil
	}
	s.mu.Unlock()
}

// ID is the conversation id.
func (s *Session) ID() string { return s.id }

// Meta returns a copy of the conversation metadata.
func (s *Session) Meta() Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta
}

// UpdateMeta applies fn to the metadata and persists it.
func (s *Session) UpdateMeta(fn func(*Meta)) (Meta, error) {
	return s.updateMeta(fn, true)
}

// RelabelMeta changes how a conversation is listed (pinned, archived)
// without making it the most recently updated.
func (s *Session) RelabelMeta(fn func(*Meta)) (Meta, error) {
	return s.updateMeta(fn, false)
}

func (s *Session) updateMeta(fn func(*Meta), touch bool) (Meta, error) {
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return Meta{}, ErrNotFound
	}
	if s.deleting {
		s.mu.Unlock()
		return Meta{}, ErrBusy
	}
	next := s.meta
	fn(&next)
	if touch {
		next.UpdatedAt = time.Now().UTC()
	}
	if err := writeMeta(s.dir, next); err != nil {
		s.mu.Unlock()
		return Meta{}, err
	}
	s.meta = next
	s.lastAccess = time.Now()
	updated := s.meta
	s.mu.Unlock()
	if s.store != nil {
		s.store.recordMeta(s, updated)
	}
	return updated, nil
}

// Append stores one event and returns it with its assigned seq.
func (s *Session) Append(kind string, payload any) (Event, error) {
	return s.AppendAt(kind, payload, time.Time{})
}

// AppendAt stores an event that happened at ts (zero means now), such as one
// replayed from an imported transcript.
func (s *Session) AppendAt(kind string, payload any, ts time.Time) (Event, error) {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("encode %s event: %w", kind, err)
		}
		raw = data
	}
	return s.appendRaw(kind, raw, ts)
}

func (s *Session) appendRaw(kind string, payload json.RawMessage, ts time.Time) (Event, error) {
	event, err := s.write(kind, payload, ts)
	if err == nil && s.store != nil && s.store.OnAppend != nil {
		s.store.OnAppend(s.ID())
	}
	return event, err
}

func (s *Session) write(kind string, payload json.RawMessage, ts time.Time) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return Event{}, ErrNotFound
	}
	if s.deleting {
		return Event{}, ErrBusy
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	nextSeq := s.seq + 1
	event := Event{Seq: nextSeq, Type: kind, Payload: payload, TS: ts.UnixMilli()}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event: %w", kind, err)
	}
	if s.appendFile == nil {
		path := filepath.Join(s.dir, "events.jsonl")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return Event{}, fmt.Errorf("open session history: %w", err)
		}
		s.appendFile = file
	}
	if _, err := s.appendFile.Write(append(line, '\n')); err != nil {
		_ = s.appendFile.Close()
		s.appendFile = nil
		return Event{}, fmt.Errorf("append session history: %w", err)
	}
	s.seq = nextSeq
	s.lastAccess = time.Now()
	s.tail = append(s.tail, event)
	if len(s.tail) > sessionTailCapacity {
		s.tailComplete = false
		if cap(s.tail) > sessionTailCapacity*2 {
			compacted := make([]Event, sessionTailCapacity)
			copy(compacted, s.tail[len(s.tail)-sessionTailCapacity:])
			s.tail = compacted
		} else {
			s.tail = s.tail[len(s.tail)-sessionTailCapacity:]
		}
	}
	// Metadata is written on turn boundaries (see Touch), not per token: a
	// streamed answer appends thousands of events.
	return event, nil
}

// Events returns every stored event with Seq greater than since.
func (s *Session) Events(since int64) ([]Event, error) {
	s.mu.Lock()
	s.lastAccess = time.Now()
	if since >= s.seq {
		s.mu.Unlock()
		return nil, nil
	}
	if len(s.tail) > 0 && (s.tailComplete || since >= s.tail[0].Seq-1) {
		idx := sort.Search(len(s.tail), func(i int) bool { return s.tail[i].Seq > since })
		var out []Event
		if idx < len(s.tail) {
			out = make([]Event, len(s.tail)-idx)
			copy(out, s.tail[idx:])
		}
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	return readEvents(s.dir, since)
}

func readEvents(dir string, since int64) ([]Event, error) {
	path := filepath.Join(dir, "events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session history: %w", err)
	}
	defer file.Close()

	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			continue // A torn final line is a crash, not a reason to lose history.
		}
		if event.Seq <= since {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	return events, nil
}

// LastSeq is the newest event seq, 0 for an empty conversation.
func (s *Session) LastSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Touch persists the metadata as it stands, refreshing UpdatedAt. It is called
// once per turn rather than per event.
func (s *Session) Touch() error {
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return ErrNotFound
	}
	if s.deleting {
		s.mu.Unlock()
		return ErrBusy
	}
	next := s.meta
	next.UpdatedAt = time.Now().UTC()
	if err := writeMeta(s.dir, next); err != nil {
		s.mu.Unlock()
		return err
	}
	s.meta = next
	s.lastAccess = time.Now()
	updated := s.meta
	s.mu.Unlock()
	if s.store != nil {
		s.store.recordMeta(s, updated)
	}
	return nil
}

func readMeta(dir string) (Meta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("decode session metadata: %w", err)
	}
	if meta.ID == "" {
		meta.ID = filepath.Base(dir)
	}
	return meta, nil
}

func writeMeta(dir string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "meta.json"), data, 0o600); err != nil {
		return fmt.Errorf("write session metadata: %w", err)
	}
	return nil
}

// TitleFrom derives a conversation title from its first user message.
func TitleFrom(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " "))
	if text == "" {
		return "New conversation"
	}
	if utf8.RuneCountInString(text) <= TitleLimit {
		return text
	}
	runes := []rune(text)
	return string(runes[:TitleLimit]) + "…"
}
