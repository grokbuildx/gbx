package provider_test

import (
	"testing"

	"github.com/grokbuildx/gbx/internal/provider"
)

func TestOAuthDefaultModels(t *testing.T) {
	if got := provider.OAuthDefaultModels("kimi"); got != "kimi-for-coding,kimi-for-coding-highspeed" {
		t.Fatalf("kimi default models = %q", got)
	}
	if got := provider.OAuthDefaultModels("kiro"); got != "claude-opus-4-6,claude-sonnet-4-6,claude-haiku-4-5" {
		t.Fatalf("kiro default models = %q", got)
	}
	for _, live := range []string{"gpt", "grok", "qoder", "workbuddy", "workbuddycn", "cursor", "autoclaw", ""} {
		if got := provider.OAuthDefaultModels(live); got != "" {
			t.Fatalf("%s should rely on live discovery, got %q", live, got)
		}
	}
}

// Kimi exposes no model-list endpoint, so RuntimeModelSpec falls back to the
// built-in Kimi pool when Model is empty.
func TestRuntimeModelSpecMakesKimiAndKiroRunnable(t *testing.T) {
	for _, backend := range []string{"kimi", "kiro"} {
		p := provider.Provider{OAuthProvider: backend}
		if spec := provider.RuntimeModelSpec(p); spec == "" {
			t.Fatalf("%s has an empty runtime model spec", backend)
		}
	}
}
