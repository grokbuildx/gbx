package proxymanager

import (
	"encoding/json"
	"testing"

	"github.com/grokbuildx/gbx/internal/codex"
	"github.com/grokbuildx/gbx/internal/provider"
)

func TestCatalogCapabilityPolicyAndJournalCompatibility(t *testing.T) {
	var state State
	if err := json.Unmarshal([]byte(`{"catalog_mode":"remote","codex_mode":"independent"}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.CatalogMode != codex.CatalogRemote || !state.CatalogCapabilities().SupportsRemoteFetch || state.CatalogCapabilities().SupportsLiveReload {
		t.Fatal("remote transport implied Desktop reload or broke the journal")
	}
	state.CodexMode = "shared"
	if state.CatalogCapabilities().SupportsRemoteFetch {
		t.Fatal("shared mode claimed a loopback remote source")
	}
	var absent *State
	if absent.CatalogCapabilities().SupportsLiveReload || absent.SupportsGatewayHotReload() {
		t.Fatal("missing runtime advertised capabilities")
	}
}

func TestRuntimeProvidersHashHandlesOneMSuffixByProviderKind(t *testing.T) {
	// OAuth providers publish bare IDs from discovery, so [1m]/[1M] toggles stay metadata-only.
	oauthA := []provider.Provider{{Name: "k", OAuthProvider: "kiro", Model: "claude-sonnet[1M]", DefaultModel: "claude-sonnet[1M]"}}
	oauthB := []provider.Provider{{Name: "k", OAuthProvider: "kiro", Model: "claude-sonnet", DefaultModel: "claude-sonnet"}}
	if runtimeProvidersHash(oauthA) != runtimeProvidersHash(oauthB) {
		t.Fatal("OAuth [1M] toggle should not force runtime replacement")
	}
	// API-key providers derive route slugs directly from Model, so removing [1m] must change runtime hash.
	apiA := []provider.Provider{{Name: "p", Type: "openai", Endpoint: "https://example.com/v1", APIKey: "k", Model: "model-a[1m]"}}
	apiB := []provider.Provider{{Name: "p", Type: "openai", Endpoint: "https://example.com/v1", APIKey: "k", Model: "model-a"}}
	if runtimeProvidersHash(apiA) == runtimeProvidersHash(apiB) {
		t.Fatal("API-key [1m] toggle must change runtimeProvidersHash to drop stale slugs")
	}
}
