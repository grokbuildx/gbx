package provider

import (
	"slices"
	"strings"
	"time"
)

// OAuthProviderSpec is the canonical metadata registry entry for a supported
// OAuth subscription backend. Subsystems (oauthproxy, codex, webui, cmd,
// proxymanager) read shared identity, protocol, and models.dev metadata from
// here instead of maintaining diverging switches.
type OAuthProviderSpec struct {
	// PublicName is the canonical CLI/UI name accepted by `gbx oauth <name>`.
	PublicName string
	// Backend is the internal credential `type` and runtime dispatch key.
	Backend string
	// Aliases are legacy or alternate names accepted at runtime/credential load.
	Aliases []string
	// RuntimeType is the compatibility protocol type persisted/inferred for p.Type.
	RuntimeType string
	// ModelsDevProviderID is the preferred provider ID in the models.dev index.
	ModelsDevProviderID string
	// DefaultModels is the static CSV model pool for backends without eager
	// live /models discovery at runtime start.
	DefaultModels string
	// SupportsFastMode reports whether the provider honors Provider.FastMode.
	SupportsFastMode bool
	// SupportsDesktopImport reports whether `gbx oauth --from-desktop` is supported.
	SupportsDesktopImport bool
	// DiscoveryInterval is the periodic background rediscovery interval (zero if none).
	DiscoveryInterval time.Duration
	// NativeCodexTools marks GPT/Codex OAuth accounts that pass native Responses
	// tools through directly on Codex routes.
	NativeCodexTools bool
}

var oauthProviderSpecs = []OAuthProviderSpec{
	{
		PublicName:          "gpt",
		Backend:             "codex",
		Aliases:             []string{"codex"},
		RuntimeType:         "openai_responses",
		ModelsDevProviderID: "openai",
		SupportsFastMode:    true,
		NativeCodexTools:    true,
	},
	{
		PublicName:          "grok",
		Backend:             "xai",
		Aliases:             []string{"xai"},
		RuntimeType:         "openai_responses",
		ModelsDevProviderID: "xai",
	},
	{
		PublicName:  "qoder",
		Backend:     "qoder",
		RuntimeType: "openai_responses",
	},
	{
		PublicName:          "kimi",
		Backend:             "kimi",
		RuntimeType:         "openai",
		ModelsDevProviderID: "kimi-for-coding",
		DefaultModels:       "kimi-for-coding,kimi-for-coding-highspeed",
	},
	{
		PublicName:          "kiro",
		Backend:             "kiro",
		RuntimeType:         "openai_responses",
		ModelsDevProviderID: "anthropic",
		DefaultModels:       "claude-opus-4-6,claude-sonnet-4-6,claude-haiku-4-5",
	},
	{
		PublicName:          "workbuddy",
		Backend:             "workbuddy",
		RuntimeType:         "openai",
		ModelsDevProviderID: "workbuddy",
	},
	{
		PublicName:          "workbuddycn",
		Backend:             "workbuddycn",
		RuntimeType:         "openai",
		ModelsDevProviderID: "workbuddy",
	},
	{
		PublicName:            "autoclaw",
		Backend:               "autoclaw",
		RuntimeType:           "autoclaw",
		SupportsDesktopImport: true,
	},
	{
		PublicName:  "cursor",
		Backend:     "cursor",
		RuntimeType: "openai_responses",
	},
}

var (
	oauthSpecByPublic = make(map[string]OAuthProviderSpec, len(oauthProviderSpecs))
	oauthSpecByAny    = make(map[string]OAuthProviderSpec, len(oauthProviderSpecs)*2)
	retiredOAuthNames = map[string]struct{}{
		"claude":      {},
		"zed":         {},
		"gemini":      {},
		"antigravity": {},
		"gemini-cli":  {},
	}
)

func init() {
	for _, spec := range oauthProviderSpecs {
		pub := strings.ToLower(spec.PublicName)
		oauthSpecByPublic[pub] = spec
		oauthSpecByAny[pub] = spec
		oauthSpecByAny[strings.ToLower(spec.Backend)] = spec
		for _, alias := range spec.Aliases {
			oauthSpecByAny[strings.ToLower(alias)] = spec
		}
	}
}

// OAuthSpecs returns a copy of all supported OAuth provider specifications in
// display order.
func OAuthSpecs() []OAuthProviderSpec {
	out := make([]OAuthProviderSpec, len(oauthProviderSpecs))
	for i, spec := range oauthProviderSpecs {
		spec.Aliases = slices.Clone(spec.Aliases)
		out[i] = spec
	}
	return out
}

// PublicOAuthProviders returns the canonical public OAuth provider names in
// display order.
func PublicOAuthProviders() []string {
	names := make([]string, 0, len(oauthProviderSpecs))
	for _, spec := range oauthProviderSpecs {
		names = append(names, spec.PublicName)
	}
	return names
}

// LookupOAuthSpec looks up a supported OAuth provider by public name, internal
// backend key, or legacy alias.
func LookupOAuthSpec(name string) (OAuthProviderSpec, bool) {
	spec, ok := oauthSpecByAny[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return OAuthProviderSpec{}, false
	}
	spec.Aliases = slices.Clone(spec.Aliases)
	return spec, true
}

// LookupPublicOAuthSpec looks up a supported OAuth provider strictly by its
// public login name (excluding internal/legacy aliases such as codex/xai).
func LookupPublicOAuthSpec(name string) (OAuthProviderSpec, bool) {
	spec, ok := oauthSpecByPublic[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return OAuthProviderSpec{}, false
	}
	spec.Aliases = slices.Clone(spec.Aliases)
	return spec, true
}

// IsRetiredOAuthProvider reports whether name is a retired OAuth backend.
func IsRetiredOAuthProvider(name string) bool {
	_, ok := retiredOAuthNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// OAuthModelsDevProviderID returns the canonical models.dev catalog provider ID
// for an OAuth provider name or alias, or "" when none applies.
func OAuthModelsDevProviderID(oauthProvider string) string {
	spec, ok := LookupOAuthSpec(oauthProvider)
	if !ok {
		return ""
	}
	return spec.ModelsDevProviderID
}

// IsReservedProviderName reports whether name collides with the built-in
// default selector or any supported/legacy/retired OAuth provider identifier.
func IsReservedProviderName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if lower == DefaultProvider || IsRetiredOAuthProvider(lower) {
		return true
	}
	_, ok := oauthSpecByAny[lower]
	return ok
}

// PromoteDefaultModel moves the model matching preferred (case-insensitive,
// matching with or without [1m]) to index 0, preserving [1m] if either the
// preferred value or matched entry carries it. When prependMissing is true and
// preferred is absent from models, preferred is prepended.
func PromoteDefaultModel(models []string, preferred string, prependMissing bool) []string {
	preferred = NormalizeModelID(preferred)
	prefBase := stripContextSuffix(preferred)
	if prefBase == "" {
		return models
	}
	if len(models) == 0 {
		if prependMissing {
			return []string{preferred}
		}
		return models
	}
	prefHas1M := HasOneMSuffix(preferred)
	matchIdx := -1
	for i, m := range models {
		if strings.EqualFold(strings.TrimSpace(m), preferred) {
			matchIdx = i
			break
		}
		if matchIdx < 0 && strings.EqualFold(stripContextSuffix(m), prefBase) {
			matchIdx = i
		}
	}
	if matchIdx < 0 {
		if prependMissing {
			out := make([]string, 0, len(models)+1)
			out = append(out, preferred)
			out = append(out, models...)
			return out
		}
		return models
	}
	chosen := strings.TrimSpace(models[matchIdx])
	if prefHas1M && !HasOneMSuffix(chosen) {
		chosen = stripContextSuffix(chosen) + "[1m]"
	}
	out := make([]string, 0, len(models))
	out = append(out, chosen)
	for i, m := range models {
		if i == matchIdx || strings.EqualFold(stripContextSuffix(m), prefBase) {
			continue
		}
		out = append(out, m)
	}
	return out
}
