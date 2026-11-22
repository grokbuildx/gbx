package codex

import (
	"strings"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/grokbuildx/gbx/internal/provider"
)

// Converted models share the cached models.dev effort metadata when the
// upstream catalog has not already declared authoritative per-model reasoning
// levels (ReasoningKnown). Provider metadata still supplies names, images and
// prices when explicitly declared, while models.dev backfills missing
// modalities and output limits on known IDs. Native Responses bypasses effort
// overrides; unknown models remain text-only.
func withModelsDevReasoning(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider) (map[string]protocol.ModelInfo, error) {
	return withModelsDevMetadata(models, details, names, configured, true)
}

func withModelsDevMetadata(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider, includeReasoning bool) (map[string]protocol.ModelInfo, error) {
	return withModelsDevMetadataPerModel(models, details, names, configured, func(string) bool { return includeReasoning })
}

func withModelsDevMetadataPerModel(models []string, details map[string]protocol.ModelInfo, names map[string]string, configured provider.Provider, includeReasoningFor func(model string) bool) (map[string]protocol.ModelInfo, error) {
	index, err := modelsdev.CachedIndex()
	if err != nil {
		// Metadata is optional. A first offline setup must not block model use.
		return details, nil
	}
	if details == nil {
		details = make(map[string]protocol.ModelInfo, len(models))
	}
	providerID := oauthproxy.ModelsDevProviderID(configured)
	for _, id := range models {
		key, info, _ := lookupModelInfo(details, id)
		name := lookupModelDisplayName(names, id)
		if name == "" {
			name = info.DisplayName
		}
		modified := false
		if len(info.InputModalities) == 0 {
			if modalities, found := index.InputModalities(providerID, id, name); found && len(modalities) > 0 {
				info.InputModalities = modalities
				modified = true
			}
		}
		if info.MaxOutputTokens <= 0 {
			if maxOut, found := index.MaxOutputTokens(providerID, id, name); found && maxOut > 0 {
				info.MaxOutputTokens = maxOut
				modified = true
			}
		}
		includeReasoning := includeReasoningFor == nil || includeReasoningFor(id)
		if !includeReasoning || info.ReasoningKnown {
			if modified {
				details[key] = info
			}
			continue
		}
		if protocol.HasGeminiEffortSuffix(id) {
			info.ReasoningKnown = true
			info.ReasoningLevels = nil
			info.DefaultReasoningLevel = ""
			details[key] = info
			continue
		}
		efforts, found := index.Efforts(providerID, id, name)
		if !found {
			if modified {
				details[key] = info
			}
			continue
		}
		info.ReasoningKnown = true
		info.ReasoningLevels = nil
		chosen := strings.ToLower(strings.TrimSpace(info.DefaultReasoningLevel))
		validDefault := false
		for _, effort := range efforts {
			info.ReasoningLevels = append(info.ReasoningLevels, protocol.ReasoningLevel{Effort: effort, Description: protocol.EffortDescription(effort)})
			validDefault = validDefault || effort == chosen
		}
		if !validDefault {
			chosen = ""
			if len(efforts) > 0 {
				chosen = efforts[0]
				for _, effort := range efforts {
					if effort == protocol.GPTDefaultReasoningLevel {
						chosen = effort
						break
					}
				}
			}
		}
		info.DefaultReasoningLevel = chosen
		details[key] = info
	}
	return details, nil
}
