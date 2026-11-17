package cmd

import (
	"strings"
	"testing"

	"github.com/grokbuildx/gbx/internal/modelsdev"
)

func TestModelsDevProviderToDraft(t *testing.T) {
	p := modelsdev.Provider{
		ID:   "opencode-go",
		Name: "OpenCode Go",
		NPM:  "@ai-sdk/openai-compatible",
		API:  "https://opencode.ai/zen/go/v1",
		Models: map[string]modelsdev.Model{
			"glm-5.2": {
				ID:     "glm-5.2",
				Name:   "GLM-5.2",
				Status: "deprecated",
				Limit:  modelsdev.ModelLimit{Context: 1000000, Output: 131072},
			},
			"qwen3.8-max": {
				ID:       "qwen3.8-max",
				Name:     "Qwen3.8 Max",
				Provider: &modelsdev.ModelProvider{NPM: "@ai-sdk/anthropic", API: "https://opencode.ai/zen/go/anthropic/v1"},
				Limit:    modelsdev.ModelLimit{Context: 1000000, Output: 131072},
			},
			"grok-4.5": {
				ID:       "grok-4.5",
				Name:     "Grok 4.5",
				Provider: &modelsdev.ModelProvider{NPM: "@ai-sdk/openai"},
				Limit:    modelsdev.ModelLimit{Context: 500000, Output: 500000},
			},
			"kimi-k3": {
				ID:    "kimi-k3",
				Name:  "Kimi K3",
				Limit: modelsdev.ModelLimit{Context: 1048576, Output: 131072},
			},
		},
	}

	draft, metadata := modelsDevProviderToDraft(p)

	if draft.Type != "modelsdev" {
		t.Fatalf("Type = %q, want modelsdev", draft.Type)
	}
	if draft.Name != "opencode-go" || draft.Endpoint != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("draft identity wrong: %+v", draft)
	}
	// deprecated glm-5.2 must be skipped.
	if strings.Contains(draft.Model, "glm-5.2") {
		t.Fatalf("deprecated model leaked into pool: %q", draft.Model)
	}
	// Anthropic models route over Messages from their own base URL.
	if got := draft.ModelProtocols["qwen3.8-max"]; got != "anthropic" || !strings.Contains(draft.Model, "qwen3.8-max") {
		t.Fatalf("qwen3.8-max protocol = %q in pool %q, want anthropic", got, draft.Model)
	}
	if got := draft.ModelEndpoints["qwen3.8-max"]; got != "https://opencode.ai/zen/go/anthropic/v1" {
		t.Fatalf("qwen3.8-max endpoint = %q", got)
	}
	if len(draft.ModelEndpoints) != 1 {
		t.Fatalf("models on the provider endpoint must not get overrides: %v", draft.ModelEndpoints)
	}
	if got := draft.ModelProtocols["grok-4.5"]; got != "openai_responses" {
		t.Fatalf("grok-4.5 protocol = %q, want openai_responses", got)
	}
	// kimi-k3 inherits the provider default -> chat.
	if got := draft.ModelProtocols["kimi-k3"]; got != "openai" {
		t.Fatalf("kimi-k3 protocol = %q, want openai (inherited)", got)
	}
	// The pool keeps both supported models.
	if !strings.Contains(draft.Model, "grok-4.5") || !strings.Contains(draft.Model, "kimi-k3") {
		t.Fatalf("pool missing models: %q", draft.Model)
	}
	// Metadata carries context window for the 1M advisory.
	if info, ok := metadata["grok-4.5"]; !ok || info.ContextWindow != 500000 {
		t.Fatalf("grok-4.5 metadata = %+v", metadata["grok-4.5"])
	}
}

func TestModelsDevAnthropicProviderDefaultsItsEndpoint(t *testing.T) {
	draft, _ := modelsDevProviderToDraft(modelsdev.Provider{
		ID: "anthropic", NPM: "@ai-sdk/anthropic",
		Models: map[string]modelsdev.Model{"claude-x": {ID: "claude-x"}},
	})
	if draft.Endpoint != modelsdev.AnthropicDefaultAPI || draft.ModelProtocols["claude-x"] != "anthropic" || len(draft.ModelEndpoints) != 0 {
		t.Fatalf("draft = %+v", draft)
	}
}
