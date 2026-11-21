package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/providersession"
)

const runtimePoolIdleTTL = 10 * time.Minute

type pooledRuntime struct {
	name        string
	fingerprint string
	session     *providersession.Session
	cancel      context.CancelFunc
	refs        int
	retired     bool
	lastUsed    time.Time
	idleTimer   *time.Timer
	activeTurns map[*atomic.Bool]struct{}
	closeOnce   sync.Once
}

func (e *pooledRuntime) closeSession() {
	if e == nil {
		return
	}
	e.closeOnce.Do(func() {
		if e.session != nil {
			e.session.Close()
		}
		if e.cancel != nil {
			e.cancel()
		}
	})
}

func (e *pooledRuntime) alive() bool {
	if e == nil || e.session == nil || e.session.Runtime == nil {
		return false
	}
	select {
	case <-e.session.Runtime.Done():
		return false
	default:
		return true
	}
}

func (e *pooledRuntime) stopIdleTimerLocked() {
	if e.idleTimer != nil {
		e.idleTimer.Stop()
		e.idleTimer = nil
	}
}

func (e *pooledRuntime) registerTurnLocked() *atomic.Bool {
	shared := &atomic.Bool{}
	if e.activeTurns == nil {
		e.activeTurns = make(map[*atomic.Bool]struct{})
	}
	if len(e.activeTurns) > 0 {
		shared.Store(true)
		for other := range e.activeTurns {
			other.Store(true)
		}
	}
	e.activeTurns[shared] = struct{}{}
	return shared
}

// runtimePool keeps warm provider runtimes across webui turns so each message
// does not pay cold OAuth refresh, model discovery, and loopback startup costs.
// Entries are keyed by provider name and validated against a fingerprint of the
// runtime configuration, with reference-counted retirement on config edits or
// idle expiry.
type runtimePool struct {
	mu        sync.Mutex
	entries   map[string]*pooledRuntime
	gen       map[string]uint64
	globalGen uint64
	closed    bool
	prepare   func(context.Context, provider.Provider) (*providersession.Session, error)
}

func newRuntimePool() *runtimePool {
	return &runtimePool{
		entries: make(map[string]*pooledRuntime),
		gen:     make(map[string]uint64),
		prepare: providersession.Prepare,
	}
}

func providerRuntimeFingerprint(p provider.Provider) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Acquire returns a warm provider session matching p, starting one on demand.
// The caller must invoke release when the turn finishes.
func (p *runtimePool) Acquire(ctx context.Context, cfg provider.Provider) (*providersession.Session, func(), error) {
	session, release, _, err := p.AcquireWithExclusivity(ctx, cfg)
	return session, release, err
}

// AcquireWithExclusivity returns a warm provider session along with a release
// callback and an exclusive() predicate that stays true only when no other turn
// overlapped on the same runtime during this lease.
func (p *runtimePool) AcquireWithExclusivity(ctx context.Context, cfg provider.Provider) (*providersession.Session, func(), func() bool, error) {
	if p == nil {
		session, err := providersession.Prepare(ctx, cfg)
		if err != nil {
			return nil, nil, nil, err
		}
		return session, session.Close, func() bool { return true }, nil
	}
	fingerprint := providerRuntimeFingerprint(cfg)
	now := time.Now()

	var toClose []*pooledRuntime
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, nil, ErrClosing
	}
	for name, entry := range p.entries {
		if entry.refs == 0 && (!entry.alive() || now.Sub(entry.lastUsed) > runtimePoolIdleTTL) {
			entry.retired = true
			entry.stopIdleTimerLocked()
			delete(p.entries, name)
			toClose = append(toClose, entry)
		}
	}
	if entry, ok := p.entries[cfg.Name]; ok && !entry.retired && entry.fingerprint == fingerprint && entry.alive() {
		entry.stopIdleTimerLocked()
		entry.refs++
		entry.lastUsed = now
		shared := entry.registerTurnLocked()
		p.mu.Unlock()
		closePooled(toClose)
		return entry.session, p.releaser(entry, shared), func() bool { return !shared.Load() }, nil
	}
	startGen := p.gen[cfg.Name]
	startGlobalGen := p.globalGen
	p.mu.Unlock()
	closePooled(toClose)

	// Start the runtime with a server-lifetime context so pooled sessions outlive
	// the first turn's cancellation, while still respecting ctx cancellation
	// during startup discovery.
	startCtx, cancelStart := context.WithCancel(context.Background())
	stopWatch := context.AfterFunc(ctx, cancelStart)
	prepareFn := p.prepare
	if prepareFn == nil {
		prepareFn = providersession.Prepare
	}
	session, err := prepareFn(startCtx, cfg)
	stopWatch()
	if err != nil {
		cancelStart()
		return nil, nil, nil, err
	}
	if ctx.Err() != nil {
		session.Close()
		cancelStart()
		return nil, nil, nil, ctx.Err()
	}

	entry := &pooledRuntime{
		name:        cfg.Name,
		fingerprint: fingerprint,
		session:     session,
		cancel:      cancelStart,
		refs:        1,
		lastUsed:    time.Now(),
	}
	shared := entry.registerTurnLocked()

	var replaced *pooledRuntime
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		entry.closeSession()
		return nil, nil, nil, ErrClosing
	}
	if p.globalGen != startGlobalGen || p.gen[cfg.Name] != startGen {
		// An invalidation raced with Prepare; do not poison the pool with a
		// runtime started against stale credentials/config, and close it when
		// this turn finishes.
		entry.retired = true
		p.mu.Unlock()
		return entry.session, p.releaser(entry, shared), func() bool { return !shared.Load() }, nil
	}
	if existing, ok := p.entries[cfg.Name]; ok {
		if !existing.retired && existing.fingerprint == fingerprint && existing.alive() {
			// Another concurrent turn finished starting the same runtime first.
			existing.stopIdleTimerLocked()
			existing.refs++
			existing.lastUsed = time.Now()
			existingShared := existing.registerTurnLocked()
			p.mu.Unlock()
			entry.closeSession()
			return existing.session, p.releaser(existing, existingShared), func() bool { return !existingShared.Load() }, nil
		}
		existing.retired = true
		existing.stopIdleTimerLocked()
		if existing.refs == 0 {
			replaced = existing
		}
	}
	p.entries[cfg.Name] = entry
	p.mu.Unlock()
	if replaced != nil {
		replaced.closeSession()
	}
	return entry.session, p.releaser(entry, shared), func() bool { return !shared.Load() }, nil
}

func (p *runtimePool) releaser(entry *pooledRuntime, shared *atomic.Bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			var shouldClose bool
			p.mu.Lock()
			if entry.activeTurns != nil {
				delete(entry.activeTurns, shared)
			}
			entry.refs--
			entry.lastUsed = time.Now()
			if entry.refs <= 0 {
				entry.stopIdleTimerLocked()
				if entry.retired || p.closed || !entry.alive() {
					if p.entries[entry.name] == entry {
						delete(p.entries, entry.name)
					}
					shouldClose = true
				} else if p.entries[entry.name] == entry {
					entry.idleTimer = time.AfterFunc(runtimePoolIdleTTL, func() {
						p.evictIfIdle(entry)
					})
				}
			}
			p.mu.Unlock()
			if shouldClose {
				entry.closeSession()
			}
		})
	}
}

func (p *runtimePool) evictIfIdle(entry *pooledRuntime) {
	if p == nil || entry == nil {
		return
	}
	var shouldClose bool
	p.mu.Lock()
	if entry.refs <= 0 && !entry.retired && p.entries[entry.name] == entry && (!entry.alive() || time.Since(entry.lastUsed) >= runtimePoolIdleTTL) {
		entry.retired = true
		entry.idleTimer = nil
		delete(p.entries, entry.name)
		shouldClose = true
	}
	p.mu.Unlock()
	if shouldClose {
		entry.closeSession()
	}
}

// Invalidate retires any pooled runtime for provider name, closing it
// immediately if idle or as soon as active turns finish.
func (p *runtimePool) Invalidate(name string) {
	if p == nil || name == "" {
		return
	}
	var toClose *pooledRuntime
	p.mu.Lock()
	if p.gen == nil {
		p.gen = make(map[string]uint64)
	}
	p.gen[name]++
	if entry, ok := p.entries[name]; ok {
		entry.retired = true
		entry.stopIdleTimerLocked()
		delete(p.entries, name)
		if entry.refs <= 0 {
			toClose = entry
		}
	}
	p.mu.Unlock()
	if toClose != nil {
		toClose.closeSession()
	}
}

// InvalidateAll retires every pooled runtime and bumps the global generation so
// in-flight startups cannot publish stale credentials or config after OAuth
// login or cloud pull.
func (p *runtimePool) InvalidateAll() {
	if p == nil {
		return
	}
	var toClose []*pooledRuntime
	p.mu.Lock()
	p.globalGen++
	for name, entry := range p.entries {
		entry.retired = true
		entry.stopIdleTimerLocked()
		delete(p.entries, name)
		if entry.refs <= 0 {
			toClose = append(toClose, entry)
		}
	}
	p.mu.Unlock()
	closePooled(toClose)
}

// Close stops all idle runtimes and marks active ones to close on release.
func (p *runtimePool) Close() {
	if p == nil {
		return
	}
	var toClose []*pooledRuntime
	p.mu.Lock()
	p.closed = true
	p.globalGen++
	for name, entry := range p.entries {
		entry.retired = true
		entry.stopIdleTimerLocked()
		delete(p.entries, name)
		if entry.refs <= 0 {
			toClose = append(toClose, entry)
		}
	}
	p.mu.Unlock()
	closePooled(toClose)
}

func closePooled(entries []*pooledRuntime) {
	for _, entry := range entries {
		entry.closeSession()
	}
}

func subtractUsage(after, before Usage) Usage {
	out := Usage{
		InputTokens:  max(0, after.InputTokens-before.InputTokens),
		OutputTokens: max(0, after.OutputTokens-before.OutputTokens),
		DurationMs:   max(0, after.DurationMs-before.DurationMs),
	}
	if after.CostUSD >= before.CostUSD {
		out.CostUSD = after.CostUSD - before.CostUSD
	}
	return out
}
