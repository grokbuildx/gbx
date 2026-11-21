package provider

import (
	"strings"
	"testing"
)

func TestProviderCloneDeepCopiesModelLabels(t *testing.T) {
	orig := Provider{
		Name:           "demo",
		Env:            map[string]string{"A": "1"},
		ModelProtocols: map[string]string{"m1": "openai"},
		ModelEndpoints: map[string]string{"m1": "https://example.com"},
		ModelLabels:    map[string]string{"m1": "Model One"},
	}
	cloned := orig.Clone()
	cloned.ModelLabels["m1"] = "Mutated"
	cloned.ModelLabels["m2"] = "Added"
	if orig.ModelLabels["m1"] != "Model One" {
		t.Fatalf("orig.ModelLabels mutated: %v", orig.ModelLabels)
	}
	if _, exists := orig.ModelLabels["m2"]; exists {
		t.Fatalf("orig.ModelLabels aliased new key: %v", orig.ModelLabels)
	}
}

func TestOAuthProviderSpecRegistryConsistency(t *testing.T) {
	pubs := PublicOAuthProviders()
	if len(pubs) != 9 {
		t.Fatalf("PublicOAuthProviders() len = %d, want 9", len(pubs))
	}
	for _, pub := range pubs {
		spec, ok := LookupPublicOAuthSpec(pub)
		if !ok {
			t.Fatalf("LookupPublicOAuthSpec(%q) missing", pub)
		}
		if spec.PublicName != pub || spec.Backend == "" || spec.RuntimeType == "" {
			t.Fatalf("incomplete spec for %q: %+v", pub, spec)
		}
		if !IsReservedProviderName(pub) || !IsReservedProviderName(spec.Backend) {
			t.Fatalf("expected %q and %q to be reserved", pub, spec.Backend)
		}
	}
	for _, retired := range []string{"gemini", "antigravity", "gemini-cli", "claude", "zed"} {
		if !IsRetiredOAuthProvider(retired) {
			t.Fatalf("expected %q to be retired", retired)
		}
		if !IsReservedProviderName(retired) {
			t.Fatalf("expected retired OAuth name %q to stay reserved", retired)
		}
		if _, ok := LookupOAuthSpec(retired); ok {
			t.Fatalf("retired OAuth provider %q must not resolve as active", retired)
		}
		err := ValidateRuntime(Provider{Name: retired, OAuthProvider: retired})
		if err == nil || !strings.Contains(err.Error(), "retired") {
			t.Fatalf("ValidateRuntime(%q) = %v, want retired error", retired, err)
		}
	}
	if _, ok := LookupPublicOAuthSpec("codex"); ok {
		t.Fatal("legacy alias codex must not be a public login provider")
	}
	if spec, ok := LookupOAuthSpec("codex"); !ok || spec.PublicName != "gpt" {
		t.Fatalf("LookupOAuthSpec(codex) = %+v, %v", spec, ok)
	}
	if got := OAuthModelsDevProviderID("kimi"); got != "kimi-for-coding" {
		t.Fatalf("OAuthModelsDevProviderID(kimi) = %q, want kimi-for-coding", got)
	}
	if got := OAuthModelsDevProviderID("kiro"); got != "anthropic" {
		t.Fatalf("OAuthModelsDevProviderID(kiro) = %q, want anthropic", got)
	}
	if got := OAuthModelsDevProviderID("workbuddycn"); got != "workbuddy" {
		t.Fatalf("OAuthModelsDevProviderID(workbuddycn) = %q, want workbuddy", got)
	}
}
