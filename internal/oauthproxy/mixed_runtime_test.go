package oauthproxy

import (
	"strings"
	"testing"
)

func TestBuildMixedRoutesBucketsByProtocol(t *testing.T) {
	protocols := map[string]string{
		"qwen3.8-max": "anthropic",
		"grok-4.5":    "openai_responses",
		"glm-5.2":     "openai",
	}
	modelSpec := "qwen3.8-max,grok-4.5,glm-5.2"

	routes, err := buildMixedRoutes(modelSpec, protocols)
	if err != nil {
		t.Fatalf("buildMixedRoutes() error: %v", err)
	}
	if len(routes.responses) != 1 || routes.responses[0].Name != "grok-4.5" {
		t.Fatalf("responses routes = %+v", routes.responses)
	}
	if len(routes.chat) != 1 || routes.chat[0].Name != "glm-5.2" {
		t.Fatalf("chat routes = %+v", routes.chat)
	}
	if len(routes.messages) != 1 || routes.messages[0].Name != "qwen3.8-max" {
		t.Fatalf("messages routes = %+v", routes.messages)
	}
	if strings.Join(routes.models, ",") != "qwen3.8-max,grok-4.5,glm-5.2" {
		t.Fatalf("models = %v, want all three in spec order", routes.models)
	}
}

func TestBuildMixedRoutesFallsBackToChat(t *testing.T) {
	protocols := map[string]string{"qwen3.8-max": "anthropic"}
	// glm-5.2 is not in the table: it must fall back to chat, not error.
	routes, err := buildMixedRoutes("qwen3.8-max,glm-5.2", protocols)
	if err != nil {
		t.Fatalf("buildMixedRoutes() error: %v", err)
	}
	if len(routes.chat) != 1 || routes.chat[0].Name != "glm-5.2" {
		t.Fatalf("chat routes = %+v, want glm-5.2 fallback", routes.chat)
	}
	if len(routes.messages) != 1 || strings.Join(routes.models, ",") != "qwen3.8-max,glm-5.2" {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestBuildMixedRoutesAnthropicOnlyGateway(t *testing.T) {
	routes, err := buildMixedRoutes("qwen3.8-max", map[string]string{"qwen3.8-max": "anthropic"})
	if err != nil {
		t.Fatalf("buildMixedRoutes(anthropic-only) error: %v", err)
	}
	if len(routes.messages) != 1 || len(routes.chat)+len(routes.responses) != 0 {
		t.Fatalf("routes = %+v", routes)
	}
}

func TestBuildMixedRoutesEmptySpecErrors(t *testing.T) {
	if _, err := buildMixedRoutes("", map[string]string{"a": "openai"}); err == nil {
		t.Fatal("buildMixedRoutes(empty) = nil error, want error")
	}
}

func TestMixedProtocolForModelAliasWins(t *testing.T) {
	protocols := map[string]string{
		"grok-4.5":     "openai_responses",
		"grok-4.5[1m]": "anthropic",
	}
	if got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5[1m]"); got != "anthropic" {
		t.Fatalf("alias protocol = %q, want anthropic (alias wins)", got)
	}
	if got := mixedProtocolForModel(protocols, "glm-5.2", "glm-5.2"); got != "openai" {
		t.Fatalf("unknown model fallback = %q, want openai", got)
	}
	if got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5"); got != "openai_responses" {
		t.Fatalf("name protocol = %q, want openai_responses", got)
	}
}

func TestMixedProtocolForModelFallsBackWhenAliasHasSuffix(t *testing.T) {
	// The alias carries [1m]; the bare name is in the table and must be resolved.
	protocols := map[string]string{"grok-4.5": "openai_responses"}
	got := mixedProtocolForModel(protocols, "grok-4.5", "grok-4.5[1m]")
	if got != "openai_responses" {
		t.Fatalf("protocol = %q, want openai_responses (name fallback)", got)
	}
}
