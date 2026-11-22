package codex

import (
	"slices"
	"testing"

	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/grokbuildx/gbx/internal/provider"
)

func TestBatch4CodexCatalogCapabilitiesContextAndMixedNativeScope(t *testing.T) {
	// 1. Capabilities propagate from protocol.ModelInfo into Codex catalog entries.
	yes := true
	details := map[string]protocol.ModelInfo{
		"gpt-5": {
			ID:                         "gpt-5",
			ContextWindow:              200_000,
			SupportsParallelToolCalls:  &yes,
			SupportsReasoningSummaries: &yes,
		},
	}
	entry := newModel("gbx/p/gpt-5", "gpt-5", details, map[string]string{"GPT-5": "GPT 5 Display"})
	if entry["supports_parallel_tool_calls"] != true || entry["supports_reasoning_summaries"] != true || entry["supports_reasoning_summary_parameter"] != true {
		t.Fatalf("expected capability flags propagated to catalog model, got: %+v", entry)
	}
	if entry["display_name"] != "GPT 5 Display" {
		t.Fatalf("expected case-insensitive lookupModelDisplayName match, got %v", entry["display_name"])
	}

	// 2. applyProviderContext inspects DefaultModel for [1m] and sets effective_context_window_percent.
	ctxModel := Model{}
	applyProviderContext(ctxModel, "claude-sonnet", provider.Provider{
		Model:        "claude-sonnet",
		DefaultModel: "claude-sonnet[1m]",
	})
	if ctxModel["context_window"] != 1_000_000 || ctxModel["effective_context_window_percent"] != 85 || ctxModel["auto_compact_token_limit"] != 850_000 {
		t.Fatalf("applyProviderContext missed DefaultModel [1m] or percent: %+v", ctxModel)
	}

	// 3. Free-form provider name containing "grok" on non-xAI Responses endpoint stays native (no xAI hijack).
	customGrokNamed := provider.Provider{
		Name:     "my-grok-gateway",
		Type:     "openai_responses",
		Endpoint: "https://gateway.example.com/v1",
		Model:    "gpt-5",
	}
	if native, _ := isNativeCodexModel(customGrokNamed, "gpt-5"); !native {
		t.Fatal("free-form provider name containing grok must not hijack generic openai_responses route")
	}
	officialXai := provider.Provider{
		Name:     "custom",
		Type:     "openai_responses",
		Endpoint: "https://api.x.ai/v1",
		Model:    "grok-4",
	}
	if native, _ := isNativeCodexModel(officialXai, "grok-4"); native {
		t.Fatal("official api.x.ai provider must route through converted bridge (native=false)")
	}

	// 4. Mixed provider scopes native vs converted per model.
	mixedProvider := provider.Provider{
		Name:     "openrouter",
		Type:     "openai",
		Endpoint: "https://openrouter.ai/api/v1",
		ModelProtocols: map[string]string{
			"gpt-5":    "openai_responses",
			"claude-4": "anthropic",
		},
	}
	if native, _ := isNativeCodexModel(mixedProvider, "gpt-5"); !native {
		t.Fatal("expected openai_responses model in mixed provider to be native")
	}
	if native, _ := isNativeCodexModel(mixedProvider, "claude-4"); native {
		t.Fatal("expected anthropic model in mixed provider to be converted")
	}

	// 5. HasGeminiEffortSuffix strips vendor/ prefix and prevents effort sliders in withModelsDevMetadata.
	if !protocol.HasGeminiEffortSuffix("google/gemini-2.5-pro-high") {
		t.Fatal("expected google/gemini-2.5-pro-high to match HasGeminiEffortSuffix")
	}
	seedModelsDevCache(t)
	gemDetails, err := withModelsDevMetadata([]string{"google/gemini-2.5-pro-high"}, nil, nil, provider.Provider{Name: "p", Type: "openai"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !gemDetails["google/gemini-2.5-pro-high"].ReasoningKnown || len(gemDetails["google/gemini-2.5-pro-high"].ReasoningLevels) != 0 {
		t.Fatalf("expected Gemini effort-suffixed model to remain ReasoningKnown with empty levels, got %+v", gemDetails["google/gemini-2.5-pro-high"])
	}

	// 6. EffortRank includes persistent above ultra, and PromoteDefaultModel preserves [1m] across duplicates.
	if protocol.EffortRank("persistent") <= protocol.EffortRank("ultra") {
		t.Fatalf("expected persistent rank > ultra rank, got persistent=%d ultra=%d", protocol.EffortRank("persistent"), protocol.EffortRank("ultra"))
	}
	promoted := provider.PromoteDefaultModel([]string{"claude-sonnet", "claude-sonnet[1m]", "other"}, "claude-sonnet", false)
	if !slices.Equal(promoted, []string{"claude-sonnet[1m]", "other"}) {
		t.Fatalf("PromoteDefaultModel lost [1m] marker across duplicates: %v", promoted)
	}
}
