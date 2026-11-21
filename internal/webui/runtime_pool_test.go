package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/grokbuildx/gbx/internal/providersession"
)

func TestSubtractUsage(t *testing.T) {
	before := Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.25, DurationMs: 1000}
	after := Usage{InputTokens: 160, OutputTokens: 80, CostUSD: 0.40, DurationMs: 1800}
	delta := subtractUsage(after, before)
	if delta.InputTokens != 60 || delta.OutputTokens != 30 || delta.DurationMs != 800 {
		t.Fatalf("unexpected delta: %+v", delta)
	}
	if delta.CostUSD < 0.1499 || delta.CostUSD > 0.1501 {
		t.Fatalf("unexpected cost delta: %f", delta.CostUSD)
	}
	// Reset / negative deltas clamp to zero.
	clamped := subtractUsage(Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01, DurationMs: 10}, after)
	if clamped != (Usage{}) {
		t.Fatalf("expected clamped zero usage, got %+v", clamped)
	}
}

func TestSessionTailCacheAndDiskFallback(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	session, err := store.Create(Meta{Title: "tail test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	total := sessionTailCapacity + 25
	for i := 1; i <= total; i++ {
		ev, err := session.Append(EventToken, TokenPayload{Text: "x"})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if ev.Seq != int64(i) {
			t.Fatalf("seq = %d, want %d", ev.Seq, i)
		}
	}

	// Tail hit (within last sessionTailCapacity events).
	recent, err := session.Events(int64(total - 5))
	if err != nil {
		t.Fatalf("Events(recent): %v", err)
	}
	if len(recent) != 5 || recent[0].Seq != int64(total-4) || recent[4].Seq != int64(total) {
		t.Fatalf("unexpected recent events: len=%d first=%d last=%d", len(recent), recent[0].Seq, recent[len(recent)-1].Seq)
	}

	// Tail empty hit (since == latest seq).
	none, err := session.Events(int64(total))
	if err != nil {
		t.Fatalf("Events(latest): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected 0 events for latest seq, got %d", len(none))
	}

	// Disk fallback (since older than in-memory tail window).
	all, err := session.Events(0)
	if err != nil {
		t.Fatalf("Events(0): %v", err)
	}
	if len(all) != total || all[0].Seq != 1 || all[len(all)-1].Seq != int64(total) {
		t.Fatalf("unexpected full history: len=%d", len(all))
	}
}

func TestStoreIndexCountAndShareToken(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	if got := store.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
	s1, err := store.Create(Meta{Title: "one"})
	if err != nil {
		t.Fatalf("Create s1: %v", err)
	}
	s2, err := store.Create(Meta{Title: "two"})
	if err != nil {
		t.Fatalf("Create s2: %v", err)
	}
	if got := store.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}

	tok, err := newShareToken()
	if err != nil {
		t.Fatalf("newShareToken: %v", err)
	}
	if _, err := s1.RelabelMeta(func(m *Meta) { m.ShareToken = tok }); err != nil {
		t.Fatalf("RelabelMeta: %v", err)
	}
	id, ok := store.FindByShareToken(tok)
	if !ok || id != s1.ID() {
		t.Fatalf("FindByShareToken = (%q, %v), want (%q, true)", id, ok, s1.ID())
	}

	// Revoking share token removes it from index.
	if _, err := s1.RelabelMeta(func(m *Meta) { m.ShareToken = "" }); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	if _, ok := store.FindByShareToken(tok); ok {
		t.Fatal("expected revoked share token to be removed from index")
	}

	// Re-share and delete session verifies Delete clears both metaIndex and shareIndex.
	if _, err := s2.RelabelMeta(func(m *Meta) { m.ShareToken = tok }); err != nil {
		t.Fatalf("re-share: %v", err)
	}
	if err := store.Delete(s2.ID()); err != nil {
		t.Fatalf("Delete s2: %v", err)
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("Count after delete = %d, want 1", got)
	}
	if _, ok := store.FindByShareToken(tok); ok {
		t.Fatal("expected deleted session share token to be removed")
	}
}

func TestRuntimePoolReuseInvalidateAndClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer upstream.Close()

	pool := newRuntimePool()
	defer pool.Close()

	var prepares atomic.Int32
	pool.prepare = func(ctx context.Context, p provider.Provider) (*providersession.Session, error) {
		prepares.Add(1)
		return providersession.Prepare(ctx, p)
	}

	cfg := provider.Provider{
		Name:     "local-openai",
		Type:     "openai",
		Endpoint: upstream.URL + "/v1",
		APIKey:   "sk-test-1",
		Model:    "gpt-4o",
	}

	// First acquire starts a new runtime.
	s1, rel1, err := pool.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if prepares.Load() != 1 {
		t.Fatalf("prepares = %d, want 1", prepares.Load())
	}

	// Second acquire with identical config reuses the warm runtime.
	s2, rel2, err := pool.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	if s2 != s1 {
		t.Fatalf("expected warm runtime reuse, got distinct sessions")
	}
	if prepares.Load() != 1 {
		t.Fatalf("prepares = %d, want 1", prepares.Load())
	}

	// Invalidating while leases are held keeps runtime alive until all leases release.
	pool.Invalidate(cfg.Name)
	select {
	case <-s1.Runtime.Done():
		t.Fatal("runtime closed while leases were still held")
	default:
	}

	rel1()
	select {
	case <-s1.Runtime.Done():
		t.Fatal("runtime closed while second lease was still held")
	default:
	}
	rel2()
	<-s1.Runtime.Done()

	// Next acquire after invalidation prepares a fresh runtime.
	s3, rel3, err := pool.Acquire(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Acquire 3: %v", err)
	}
	if s3 == s1 || prepares.Load() != 2 {
		t.Fatalf("expected fresh runtime after invalidation (prepares=%d)", prepares.Load())
	}
	rel3()

	// Fingerprint change (updated API key) starts a new runtime and retires idle old one.
	cfg2 := cfg
	cfg2.APIKey = "sk-test-2"
	s4, rel4, err := pool.Acquire(context.Background(), cfg2)
	if err != nil {
		t.Fatalf("Acquire 4: %v", err)
	}
	if s4 == s3 || prepares.Load() != 3 {
		t.Fatalf("expected fresh runtime after fingerprint change (prepares=%d)", prepares.Load())
	}
	<-s3.Runtime.Done()

	// Closing the pool waits for active lease then shuts down runtime, and refuses new acquires.
	pool.Close()
	if _, _, err := pool.Acquire(context.Background(), cfg2); err != ErrClosing {
		t.Fatalf("Acquire after Close = %v, want ErrClosing", err)
	}
	rel4()
	<-s4.Runtime.Done()
}
