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

func inferModelsDevProviderFromHint(name, endpoint string) string {
	host := ""
	if parsed, err := url.Parse(strings.TrimSpace(endpoint)); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	haystack := strings.ToLower(strings.TrimSpace(name) + " " + host + " " + strings.TrimSpace(endpoint))
	switch {
	case strings.Contains(haystack, "api.x.ai"), strings.Contains(haystack, "xai"), strings.Contains(haystack, "grok"):
		return "xai"
	case strings.Contains(haystack, "api.openai.com"), strings.Contains(haystack, "openai"):
		return "openai"
	case strings.Contains(haystack, "generativelanguage.googleapis.com"), strings.Contains(haystack, "google"), strings.Contains(haystack, "gemini"):
		return "google"
	case strings.Contains(haystack, "api.kimi.com"), strings.Contains(haystack, "kimi-for-coding"), strings.Contains(haystack, "kimi-code"):
		return "kimi-code-plan-global"
	case strings.Contains(haystack, "api.moonshot"), strings.Contains(haystack, "moonshot"):
		return "moonshotai"
	case strings.Contains(haystack, "openrouter"):
		return "openrouter"
	case strings.Contains(haystack, "anthropic"):
		return "anthropic"
	case strings.Contains(haystack, "deepseek"):
		return "deepseek"
	case strings.Contains(haystack, "dashscope"), strings.Contains(haystack, "alibaba"), strings.Contains(haystack, "qwen"):
		return "alibaba"
	case strings.Contains(haystack, "bigmodel.cn"), strings.Contains(haystack, "z.ai"), strings.Contains(haystack, "zai"), strings.Contains(haystack, "glm"):
		return "zai"
	default:
		return ""
	}
}
