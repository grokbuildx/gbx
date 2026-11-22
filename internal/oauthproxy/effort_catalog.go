package oauthproxy

import (
	"net/url"
	"strings"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/grokbuildx/gbx/internal/provider"
)

var grokAcceptedEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// GrokAcceptedEffortLevels returns the ordered reasoning effort values Grok
// Build accepts on /v1/models and in session pickers.
func GrokAcceptedEffortLevels() []string {
	return append([]string(nil), grokAcceptedEffortLevels...)
}

// DefaultNativeGrokEffortLevels returns the default effort ladder exposed for
// the built-in Grok Build native account.
func DefaultNativeGrokEffortLevels() []string {
	return GrokAcceptedEffortLevels()
}

// ResolveGrokModelEffort resolves Grok-facing reasoning effort support, valid
// effort rungs, and default effort for one model using the exact same rules as
// loopback /v1/models emission.
func ResolveGrokModelEffort(index *modelsdev.Index, providerID, modelID, displayName string, detail protocol.ModelInfo, hasDetail bool) (supports bool, levels []string, defaultEffort string) {
	target := stripContextModelSuffix(modelID)
	if hasDetail && len(detail.ReasoningLevels) > 0 {
		_, grokLevels := partitionReasoningLevels(detail.ReasoningLevels)
		if len(grokLevels) == 0 {
			return false, nil, ""
		}
		return true, reasoningLevelEfforts(grokLevels), chooseGrokDefaultEffort(grokLevels, detail.DefaultReasoningLevel)
	}
	if hasDetail && detail.ReasoningKnown {
		return false, nil, ""
	}
	fallbackLevels, fallbackDefault, ok := resolveFallbackModelReasoning(index, providerID, target, firstNonEmptyString(displayName, modelID))
	if !ok || len(fallbackLevels) == 0 {
		return false, nil, ""
	}
	_, grokLevels := partitionReasoningLevels(fallbackLevels)
	if len(grokLevels) == 0 {
		return false, nil, ""
	}
	return true, reasoningLevelEfforts(grokLevels), chooseGrokDefaultEffort(grokLevels, fallbackDefault)
}

func reasoningLevelEfforts(levels []protocol.ReasoningLevel) []string {
	if len(levels) == 0 {
		return nil
	}
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		out = append(out, level.Effort)
	}
	return out
}

// ModelsDevProviderID resolves the canonical models.dev provider identifier for
// p, checking explicit ModelsDevProvider, models.dev catalog type, OAuth specs,
// and endpoint/name inference in priority order.
func ModelsDevProviderID(p provider.Provider) string {
	if explicit := strings.TrimSpace(p.ModelsDevProvider); explicit != "" {
		return explicit
	}
	if oauthName := strings.ToLower(strings.TrimSpace(p.OAuthProvider)); oauthName != "" {
		if id := provider.OAuthModelsDevProviderID(oauthName); id != "" {
			return id
		}
	}
	return inferModelsDevProviderID(p)
}

// StaticProviderModelDetails collects offline/cached model IDs, display labels,
// protocol model metadata, and the models.dev provider identifier for p without
// making network requests.
func StaticProviderModelDetails(p provider.Provider) (models []string, labels map[string]string, details map[string]protocol.ModelInfo, modelsDevProviderID string) {
	labels = make(map[string]string)
	details = make(map[string]protocol.ModelInfo)
	oauthName := strings.ToLower(strings.TrimSpace(p.OAuthProvider))
	modelSpec := provider.RuntimeModelSpec(p)
	modelsDevProviderID = ModelsDevProviderID(p)

	switch oauthName {
	case ProviderGrok, backendXAI:
		fb := xaiFallbackModels()
		models = mergeUniqueModels(runtimeModelAliases(modelSpec), xaiModelIDs(fb))
		for id, name := range xaiModelDisplayNames(fb) {
			labels[id] = name
		}
		for id, info := range xaiModelDetails(modelSpec, fb) {
			details[id] = info
		}
	case ProviderKimi:
		spec := firstNonEmptyString(modelSpec, provider.OAuthDefaultModels(ProviderKimi))
		routes := runtimeModelRoutes(spec)
		models = runtimeModelAliases(spec)
		for id, info := range kimiModelDetails(routes) {
			details[id] = info
		}
	case ProviderKiro:
		spec := firstNonEmptyString(modelSpec, provider.OAuthDefaultModels(ProviderKiro))
		models = kiroRuntimeModels(spec)
		for id, info := range kiroInitialModelDetails(spec, models) {
			details[id] = info
		}
	case ProviderAutoClaw:
		contract, _ := autoClawEffectiveContract()
		routes := autoClawModelRoutesWithCatalog(modelSpec, contract.models)
		models = mergeUniqueModels(runtimeModelAliases(modelSpec), autoClawCatalogIDs(contract.models))
		for id, name := range autoClawModelDisplayNamesWithCatalog(routes, contract.models) {
			labels[id] = name
		}
		for id, info := range autoClawModelDetailsWithCatalog(routes, contract.models) {
			details[id] = info
		}
	case ProviderCursor:
		models = runtimeModelAliases(modelSpec)
		if len(models) == 0 {
			models = append([]string(nil), cursorFallbackModels...)
		}
		routeMap := make(map[string]string, len(models))
		for _, r := range runtimeModelRoutes(modelSpec) {
			routeMap[strings.ToLower(strings.TrimSpace(r.Alias))] = r.Name
		}
		for _, m := range models {
			key := strings.ToLower(strings.TrimSpace(m))
			if _, ok := routeMap[key]; !ok {
				routeMap[key] = stripContextModelSuffix(m)
			}
		}
		for id, info := range cursorModelDetails(models, routeMap) {
			details[id] = info
		}
	case ProviderQoder:
		models = runtimeModelAliases(modelSpec)
		fb := qoderFallbackModels()
		useFallbackList := len(models) == 0
		for _, model := range fb {
			if useFallbackList {
				models = append(models, model.ID)
			}
			if name := strings.TrimSpace(model.Name); name != "" && name != model.ID {
				labels[model.ID] = name
			}
			if info, ok := qoderModelDetail(model); ok {
				details[strings.ToLower(model.ID)] = info
			}
		}
	case ProviderWorkBuddy, ProviderWorkBuddyCN:
		models = runtimeModelAliases(modelSpec)
	case ProviderChatGPT, ProviderCodex:
		spec := firstNonEmptyString(modelSpec, "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna")
		models = runtimeModelAliases(spec)
	default:
		models = runtimeModelAliases(modelSpec)
	}

	for id, name := range p.ModelLabels {
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || name == "" {
			continue
		}
		if existing, ok := labels[id]; !ok || strings.TrimSpace(existing) == "" {
			labels[id] = name
		}
	}

	if def := provider.EffectiveDefaultModel(p); def != "" {
		models = promoteDefaultModelID(models, def)
	}
	return models, labels, details, modelsDevProviderID
}

func mergeUniqueModels(lists ...[]string) []string {
	out := make([]string, 0)
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, item := range list {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" {
				continue
			}
			key := strings.ToLower(trimmed)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, trimmed)
		}
	}
	return out
}

func promoteDefaultModelID(models []string, preferred string) []string {
	return provider.PromoteDefaultModel(models, preferred, true)
}

func inferModelsDevProviderID(p provider.Provider) string {
	if provider.IsModelsDevType(p.Type) {
		return provider.ModelsDevCatalogID(p)
	}
	return inferModelsDevProviderFromHint(p.Name, p.Endpoint)
}

// IsOfficialXaiEndpoint reports whether endpoint targets an official xAI API host.
func IsOfficialXaiEndpoint(endpoint string) bool {
	host := endpointHostname(endpoint)
	return hostMatchesDomain(host, "api.x.ai") || hostMatchesDomain(host, "x.ai")
}

// IsXaiAPIProvider reports whether an API-key provider should use xAI's
// Responses compatibility rules (tool flattening, effort clamping, local
// compaction). Free-form provider names never hijack protocol routing.
func IsXaiAPIProvider(p provider.Provider) bool {
	if strings.TrimSpace(p.OAuthProvider) != "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(p.ModelsDevProvider), "xai") {
		return true
	}
	if provider.IsModelsDevType(p.Type) && strings.EqualFold(provider.ModelsDevCatalogID(p), "xai") {
		return true
	}
	return IsOfficialXaiEndpoint(p.Endpoint)
}

func endpointHostname(endpoint string) string {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return ""
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	if !strings.Contains(trimmed, "://") {
		if parsed, err := url.Parse("https://" + trimmed); err == nil {
			return strings.ToLower(parsed.Hostname())
		}
	}
	return ""
}

func hostMatchesDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func nameTokens(name string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func hasAnyToken(tokens []string, candidates ...string) bool {
	for _, token := range tokens {
		for _, want := range candidates {
			if token == want {
				return true
			}
		}
	}
	return false
}

func inferModelsDevProviderFromHint(name, endpoint string) string {
	// Stage 1: exact/suffix hostname match only (never inspect URL path segments
	// like /v1beta/openai or /inference/openai/v1).
	if host := endpointHostname(endpoint); host != "" {
		switch {
		case hostMatchesDomain(host, "api.x.ai"), hostMatchesDomain(host, "x.ai"):
			return "xai"
		case hostMatchesDomain(host, "generativelanguage.googleapis.com"), hostMatchesDomain(host, "aiplatform.googleapis.com"):
			return "google"
		case hostMatchesDomain(host, "api.openai.com"), hostMatchesDomain(host, "openai.com"):
			return "openai"
		case hostMatchesDomain(host, "api.kimi.com"):
			return "kimi-code-plan-global"
		case hostMatchesDomain(host, "moonshot.cn"), hostMatchesDomain(host, "moonshot.ai"):
			return "moonshotai"
		case hostMatchesDomain(host, "openrouter.ai"):
			return "openrouter"
		case hostMatchesDomain(host, "anthropic.com"):
			return "anthropic"
		case hostMatchesDomain(host, "deepseek.com"):
			return "deepseek"
		case hostMatchesDomain(host, "dashscope.aliyuncs.com"),
			hostMatchesDomain(host, "dashscope-intl.aliyuncs.com"),
			hostMatchesDomain(host, "dashscope-us.aliyuncs.com"):
			return "alibaba"
		case hostMatchesDomain(host, "bigmodel.cn"), hostMatchesDomain(host, "z.ai"):
			return "zai"
		}
	}

	// Stage 2: token-bounded provider name fallback.
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "" {
		return ""
	}
	tokens := nameTokens(lowerName)
	switch {
	case hasAnyToken(tokens, "xai", "grok"):
		return "xai"
	case hasAnyToken(tokens, "google", "gemini"):
		return "google"
	case hasAnyToken(tokens, "openai"):
		return "openai"
	case strings.Contains(lowerName, "kimi-for-coding"), strings.Contains(lowerName, "kimi-code"), hasAnyToken(tokens, "kimi"):
		return "kimi-code-plan-global"
	case hasAnyToken(tokens, "moonshot", "moonshotai"):
		return "moonshotai"
	case hasAnyToken(tokens, "openrouter"):
		return "openrouter"
	case hasAnyToken(tokens, "anthropic", "claude"):
		return "anthropic"
	case hasAnyToken(tokens, "deepseek"):
		return "deepseek"
	case hasAnyToken(tokens, "dashscope", "alibaba", "qwen"):
		return "alibaba"
	case hasAnyToken(tokens, "bigmodel", "zai", "glm"):
		return "zai"
	default:
		return ""
	}
}
