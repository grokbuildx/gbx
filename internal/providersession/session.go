// Package providersession prepares the one provider runtime shape shared by
// Grok Build, Codex, and management commands.
package providersession

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/grokbuildx/gbx/internal/provider"
)

// Session is the resolved provider view for one operation. Provider is always
// a copy, so preparing a session never mutates persisted configuration.
type Session struct {
	Provider provider.Provider
	Runtime  *oauthproxy.Runtime

	closeOnce sync.Once
}

// PrepareOptions customizes the embedded provider runtime.
type PrepareOptions struct {
	// DisableHistoryCache skips the loopback previous_response_id cache when an
	// outer gateway (such as codex.Gateway) already maintains conversation history.
	DisableHistoryCache bool
}

// Prepare discovers any missing API-key gateway models and starts a Responses runtime.
func Prepare(ctx context.Context, configured provider.Provider) (*Session, error) {
	return PrepareWithOptions(ctx, configured, PrepareOptions{})
}

// PrepareWithOptions discovers any missing API-key gateway models and starts a
// Responses runtime with the supplied options.
func PrepareWithOptions(ctx context.Context, configured provider.Provider, options PrepareOptions) (*Session, error) {
	if err := provider.ValidateRuntime(configured); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resolved := configured.Clone()
	resolved.Type = provider.EffectiveType(resolved)

	if strings.TrimSpace(resolved.OAuthProvider) == "" && strings.TrimSpace(resolved.Endpoint) == "" {
		return nil, fmt.Errorf("provider %q requires an endpoint", resolved.Name)
	}

	// The provider has not declared its own model pool: discover it from the
	// gateway's OpenAI-shaped model list before the runtime starts.
	if resolved.OAuthProvider == "" && strings.TrimSpace(resolved.Model) == "" {
		models, err := protocol.GetOpenAIModelsContext(ctx, resolved.Endpoint, resolved.APIKey)
		if err != nil {
			return nil, fmt.Errorf("discover OpenAI models before starting the provider runtime: %w", err)
		}
		resolved.Model = models
	}

	runtime, err := oauthproxy.StartProvider(ctx, oauthproxy.StartOptions{
		Protocol:               upstreamProtocol(resolved),
		Endpoint:               resolved.Endpoint,
		APIKey:                 resolved.APIKey,
		ModelSpec:              provider.RuntimeModelSpec(resolved),
		OAuthProvider:          resolved.OAuthProvider,
		OAuthAccountCredential: resolved.OAuthAccountCredential,
		ModelProtocols:         resolved.ModelProtocols,
		ModelEndpoints:         resolved.ModelEndpoints,
		ModelsDevProvider:      oauthproxy.ModelsDevProviderID(resolved),
		DisableHistoryCache:    options.DisableHistoryCache,
	})
	if err != nil {
		return nil, fmt.Errorf("start embedded provider runtime: %w", err)
	}

	resolved.Endpoint = runtime.Endpoint()
	resolved.APIKey = runtime.APIKey()
	if runtimeModels := runtime.Models(); len(runtimeModels) > 0 {
		// A successful OAuth catalog fetch is authoritative for the account. This
		// refreshes stale persisted pools (for example Grok 4.5 -> 4.6) before the
		// launcher validates slot mappings. A compatibility fallback never replaces
		// a non-empty saved pool because it is only a guess.
		if strings.TrimSpace(resolved.Model) == "" || (strings.TrimSpace(resolved.OAuthProvider) != "" && !runtime.ModelCatalogIsFallback()) {
			resolved.Model = strings.Join(runtimeModels, ",")
		}
	}
	return &Session{Provider: resolved, Runtime: runtime}, nil
}

func upstreamProtocol(p provider.Provider) oauthproxy.UpstreamProtocol {
	if provider.IsOpenAIResponsesType(p.Type) {
		return oauthproxy.ProtocolOpenAIResponses
	}
	// OAuth backends select their executor by OAuthProvider; this value only
	// matters for manual API-key gateways.
	return oauthproxy.ProtocolOpenAIChat
}

// Close releases the embedded runtime. It is safe to call more than once.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.Runtime != nil {
			s.Runtime.Stop()
		}
	})
}
