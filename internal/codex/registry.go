package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
)

// RuntimeBinding gives each provider an explicit account/runtime owner. The
// gateway takes ownership of Close only after a successful publication.
type RuntimeBinding struct {
	Provider provider.Provider
	Runtime  *oauthproxy.Runtime
	Close    func()
}

type runtimeGeneration struct {
	active  atomic.Int64
	retired atomic.Bool
	mu      sync.Mutex
	closed  bool
	close   func()
}

func (gen *runtimeGeneration) setClose(fn func()) {
	if gen == nil {
		return
	}
	gen.mu.Lock()
	if gen.closed {
		gen.mu.Unlock()
		if fn != nil {
			fn()
		}
		return
	}
	gen.close = fn
	gen.mu.Unlock()
}

func (gen *runtimeGeneration) retire() {
	if gen == nil {
		return
	}
	gen.retired.Store(true)
}

func (gen *runtimeGeneration) maybeClose() {
	if gen == nil || !gen.retired.Load() || gen.active.Load() != 0 {
		return
	}
	gen.mu.Lock()
	if gen.closed {
		gen.mu.Unlock()
		return
	}
	gen.closed = true
	fn := gen.close
	gen.close = nil
	gen.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type catalogSnapshot struct {
	models           []Model
	routes           map[string]Route
	runtimes         map[string]*oauthproxy.Runtime
	legacyCompaction map[string]Route
	native           Catalog
	generation       *runtimeGeneration
}

type runtimeRequestKey struct{}

func (g *Gateway) requestRuntime(r *http.Request) *oauthproxy.Runtime {
	if runtime, ok := r.Context().Value(runtimeRequestKey{}).(*oauthproxy.Runtime); ok && runtime != nil {
		return runtime
	}
	return g.runtime
}

func (g *Gateway) currentLocked() *catalogSnapshot {
	if snapshot := g.modelSnapshot.Load(); snapshot != nil {
		return snapshot
	}
	if g.generation == nil {
		g.generation = &runtimeGeneration{}
	}
	runtimes := make(map[string]*oauthproxy.Runtime, len(g.routes))
	for id := range g.routes {
		runtimes[id] = g.runtime
	}
	return &catalogSnapshot{models: g.models, routes: g.routes, runtimes: runtimes, legacyCompaction: g.legacyCompaction, native: g.nativeCatalog, generation: g.generation}
}

func (g *Gateway) OwnRuntime(close func()) {
	g.catalogMu.Lock()
	gen := g.currentLocked().generation
	g.catalogMu.Unlock()
	gen.setClose(close)
}

func (g *Gateway) acquireSnapshot() (*catalogSnapshot, func()) {
	g.catalogMu.RLock()
	snapshot := g.modelSnapshot.Load()
	if snapshot == nil {
		g.catalogMu.RUnlock()
		g.catalogMu.Lock()
		snapshot = g.currentLocked()
		g.modelSnapshot.Store(snapshot)
		snapshot.generation.active.Add(1)
		g.catalogMu.Unlock()
	} else {
		snapshot.generation.active.Add(1)
		g.catalogMu.RUnlock()
	}
	return snapshot, func() {
		if snapshot.generation.active.Add(-1) == 0 {
			snapshot.generation.maybeClose()
		}
	}
}

func remapLegacyCompaction(old map[string]Route, nextRoutes map[string]Route, defaultRoute Route) map[string]Route {
	out := make(map[string]Route, len(old))
	for id, route := range old {
		if replacement, ok := nextRoutes[route.Alias]; ok {
			out[id] = replacement
		} else {
			out[id] = defaultRoute
		}
	}
	return out
}

// ReplaceProviders atomically publishes a complete route set after persistence.
// In-flight requests lease the previous immutable snapshot until their bodies
// (including streaming responses) finish. Port, token and histories stay put.
func (g *Gateway) ReplaceProviders(bindings []RuntimeBinding, persist func([]Model) error) error {
	if len(bindings) == 0 {
		return fmt.Errorf("a Codex registry needs at least one provider")
	}
	next := &catalogSnapshot{routes: make(map[string]Route), runtimes: make(map[string]*oauthproxy.Runtime), generation: &runtimeGeneration{}}
	names := make(map[string]bool)
	var cleanups []func()
	for _, binding := range bindings {
		p := binding.Provider
		if binding.Runtime == nil || p.Name == "" || names[p.Name] || provider.ValidateRuntime(p) != nil {
			return fmt.Errorf("invalid or duplicate Codex runtime binding")
		}
		names[p.Name] = true
		models, routes, err := buildProviderCatalog(binding.Runtime, p)
		if err != nil {
			return err
		}
		for alias, route := range routes {
			next.routes[alias] = route
			next.runtimes[alias] = binding.Runtime
		}
		next.models = append(next.models, models...)
		if binding.Close != nil {
			cleanups = append(cleanups, binding.Close)
		}
	}
	// Distinguish identical model labels from different accounts only when
	// necessary; single-provider picker labels retain their existing names.
	labelCounts := make(map[string]int)
	for _, model := range next.models {
		label, _ := model["display_name"].(string)
		labelCounts[strings.ToLower(label)]++
	}
	for _, model := range next.models {
		label, _ := model["display_name"].(string)
		if labelCounts[strings.ToLower(label)] > 1 {
			alias, _ := model["slug"].(string)
			model["display_name"] = label + " (" + next.routes[alias].Provider + ")"
		}
	}
	next.generation.setClose(func() {
		for _, close := range cleanups {
			close()
		}
	})

	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	g.catalogMu.RLock()
	closed := g.closed
	old := g.modelSnapshot.Load()
	if !closed && old == nil {
		g.catalogMu.RUnlock()
		g.catalogMu.Lock()
		closed = g.closed
		if !closed {
			old = g.currentLocked()
			g.modelSnapshot.Store(old)
		}
		g.catalogMu.Unlock()
	} else {
		g.catalogMu.RUnlock()
	}
	if closed {
		return fmt.Errorf("Codex gateway is closed")
	}
	next.native = old.native
	applyClientToolMode(next.native, next.models, next.routes)
	defaultRoute := next.routes[next.models[0]["slug"].(string)]
	next.legacyCompaction = remapLegacyCompaction(old.legacyCompaction, next.routes, defaultRoute)
	// Persist outside catalogMu so inference requests never stall on disk I/O.
	if err := persist(cloneCatalogModels(next.models)); err != nil {
		return err
	}
	g.catalogMu.Lock()
	if g.closed {
		g.catalogMu.Unlock()
		return fmt.Errorf("Codex gateway is closed")
	}
	current := g.currentLocked()
	next.native = current.native
	next.legacyCompaction = remapLegacyCompaction(current.legacyCompaction, next.routes, defaultRoute)
	applyClientToolMode(next.native, next.models, next.routes)
	g.modelSnapshot.Store(next)
	current.generation.retire()
	g.catalogMu.Unlock()
	current.generation.maybeClose()
	return nil
}

func (g *Gateway) serveCatalog(w http.ResponseWriter, r *http.Request, snapshot *catalogSnapshot) {
	catalog := Catalog{Models: cloneCatalogModels(snapshot.models)}
	if !g.options.Independent {
		var err error
		catalog, err = MergeCatalog(snapshot.native, catalog.Models)
		if err != nil {
			writeError(w, 500, "api_error", "cannot build Codex catalog", "")
			return
		}
	}
	raw, err := json.Marshal(catalog)
	if err != nil || len(raw) > 1<<20 {
		writeError(w, 500, "api_error", "Codex remote catalog exceeds 1 MiB", "")
		return
	}
	hash := sha256.Sum256(append(append([]byte(nil), raw...), []byte(r.URL.Query().Get("client_version"))...))
	etag := `"` + hex.EncodeToString(hash[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization, ChatGPT-Account-Id")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// RefreshProvidersMetadata keeps each route bound to its existing account.
func (g *Gateway) RefreshProvidersMetadata(providers []provider.Provider, persist func([]Model) error) error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()

	g.catalogMu.RLock()
	closed := g.closed
	current := g.modelSnapshot.Load()
	if !closed && current == nil {
		g.catalogMu.RUnlock()
		g.catalogMu.Lock()
		closed = g.closed
		if !closed {
			current = g.currentLocked()
			g.modelSnapshot.Store(current)
		}
		g.catalogMu.Unlock()
	} else {
		g.catalogMu.RUnlock()
	}
	if closed {
		return fmt.Errorf("Codex gateway is closed")
	}
	next := *current
	next.models = cloneCatalogModels(current.models)
	next.routes = maps.Clone(current.routes)
	for _, p := range providers {
		var runtime *oauthproxy.Runtime
		for alias, route := range current.routes {
			if route.Provider == p.Name {
				runtime = current.runtimes[alias]
				break
			}
		}
		if runtime == nil {
			continue
		}
		p.Type = provider.EffectiveType(p)
		oauth := strings.ToLower(strings.TrimSpace(p.OAuthProvider))
		mixed := oauth == "" && len(p.ModelProtocols) > 0
		nativeProvider := oauth == "gpt" || oauth == "codex" ||
			oauth == "" && !mixed && provider.IsOpenAIResponsesType(p.Type)
		details := withGPTReasoningDefaults(runtime.Models(), runtime.ModelDetails())
		if _, err := modelsdev.CachedIndex(); err == nil {
			details, _ = withModelsDevMetadata(runtime.Models(), details, runtime.ModelDisplayNames(), p, !nativeProvider)
		}
		for _, model := range next.models {
			alias, _ := model["slug"].(string)
			route, ok := next.routes[alias]
			if !ok || route.Provider != p.Name {
				continue
			}
			applyProviderContext(model, provider.StripContextSuffix(route.Model), p)
			_, info, _ := lookupModelInfo(details, route.Model)
			if len(info.InputModalities) > 0 {
				applyModelModalities(model, info)
			}
			if info.MaxOutputTokens > 0 {
				route.MaxOutputTokens = info.MaxOutputTokens
				next.routes[alias] = route
			}
			if !route.NativeResponses && info.ReasoningKnown {
				model["supported_reasoning_levels"], model["default_reasoning_level"] = reasoningLevels(info)
			}
		}
	}
	if err := persist(cloneCatalogModels(next.models)); err != nil {
		return err
	}
	g.catalogMu.Lock()
	if g.closed {
		g.catalogMu.Unlock()
		return fmt.Errorf("Codex gateway is closed")
	}
	latest := g.currentLocked()
	next.native = latest.native
	next.legacyCompaction = latest.legacyCompaction
	next.generation = latest.generation
	applyClientToolMode(next.native, next.models, next.routes)
	g.modelSnapshot.Store(&next)
	g.catalogMu.Unlock()
	return nil
}

func (g *Gateway) setLegacyCompaction(mapping map[string]Route) {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	g.legacyCompaction = maps.Clone(mapping)
	if current := g.modelSnapshot.Load(); current != nil {
		next := *current
		next.native = g.nativeCatalog
		next.legacyCompaction = maps.Clone(g.legacyCompaction)
		g.modelSnapshot.Store(&next)
	}
}

func (g *Gateway) finishCatalogPlan() {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	if current := g.modelSnapshot.Load(); current != nil {
		next := *current
		next.native = g.nativeCatalog
		next.legacyCompaction = maps.Clone(g.legacyCompaction)
		g.modelSnapshot.Store(&next)
	}
}
