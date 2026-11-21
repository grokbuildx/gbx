package protocol

import (
	"strings"
)

// ReasoningLevel uses the Codex model-catalog shape so a runtime can retain
// upstream capability metadata without inferring it from a model's name.
type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// EffortDescription returns the canonical human description for a standard
// reasoning effort rung. Unknown values are returned trimmed as-is.
func EffortDescription(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "Minimal reasoning for fast responses"
	case "low":
		return "Fast responses with lighter reasoning"
	case "medium":
		return "Balances speed and reasoning depth for everyday tasks"
	case "high":
		return "Greater reasoning depth for complex problems"
	case "xhigh":
		return "Extra high reasoning depth for complex problems"
	case "max":
		return "Maximum reasoning depth for the hardest problems"
	case "ultra":
		return "Maximum reasoning with automatic task delegation"
	default:
		return strings.TrimSpace(effort)
	}
}

// ChatReasoningLevels are the standard Chat Completions effort controls for a
// backend that explicitly advertises supportsReasoning.
func ChatReasoningLevels() []ReasoningLevel {
	return []ReasoningLevel{
		{Effort: "low", Description: "Faster responses with less reasoning"},
		{Effort: "medium", Description: "Balance reasoning depth and response time"},
		{Effort: "high", Description: "More reasoning for complex tasks"},
	}
}

// GPTDefaultReasoningLevel is the picker default on GPT flagship models such
// as gpt-6.1-sol. Callers that invent a ladder use this instead of medium.
const GPTDefaultReasoningLevel = "low"

// GPTReasoningLevels is the six-rung effort ladder GPT flagship models
// advertise, in desktop slider order. Descriptions match that catalog.
// Each call returns a new slice.
func GPTReasoningLevels() []ReasoningLevel {
	return []ReasoningLevel{
		{Effort: "low", Description: EffortDescription("low")},
		{Effort: "medium", Description: EffortDescription("medium")},
		{Effort: "high", Description: EffortDescription("high")},
		{Effort: "xhigh", Description: EffortDescription("xhigh")},
		{Effort: "max", Description: EffortDescription("max")},
		{Effort: "ultra", Description: EffortDescription("ultra")},
	}
}

// EffortRank orders standard reasoning effort tiers from lowest (none) to
// highest (ultra), returning -1 for unrecognized values.
func EffortRank(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh":
		return 5
	case "max":
		return 6
	case "ultra":
		return 7
	default:
		return -1
	}
}

// ClampEffort maps desired onto the closest supported value in allowed:
// exact case-insensitive match first (preserving candidate casing), then the
// highest allowed tier <= desired, then the lowest allowed tier > desired.
// Returns "" when allowed is empty or desired is unrecognized.
func ClampEffort(desired string, allowed []string) string {
	want := strings.ToLower(strings.TrimSpace(desired))
	if want == "" || len(allowed) == 0 {
		return ""
	}
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), want) {
			return strings.TrimSpace(candidate)
		}
	}
	wantRank := EffortRank(want)
	if wantRank < 0 {
		return ""
	}
	bestLower := ""
	bestLowerRank := -1
	lowestHigher := ""
	lowestHigherRank := 999
	for _, candidate := range allowed {
		trimmed := strings.TrimSpace(candidate)
		rank := EffortRank(trimmed)
		if rank < 0 {
			continue
		}
		if rank <= wantRank && rank > bestLowerRank {
			bestLower = trimmed
			bestLowerRank = rank
		} else if rank > wantRank && rank < lowestHigherRank {
			lowestHigher = trimmed
			lowestHigherRank = rank
		}
	}
	if bestLower != "" {
		return bestLower
	}
	return lowestHigher
}

// ClampReasoningLevel maps desired onto the closest supported ReasoningLevel
// effort in lower-case canonical form, or "" when levels is empty or desired
// cannot be matched.
func ClampReasoningLevel(desired string, levels []ReasoningLevel) string {
	if len(levels) == 0 {
		return ""
	}
	allowed := make([]string, 0, len(levels))
	for _, level := range levels {
		if eff := strings.ToLower(strings.TrimSpace(level.Effort)); eff != "" {
			allowed = append(allowed, eff)
		}
	}
	return strings.ToLower(ClampEffort(desired, allowed))
}

// ChooseDefaultEffort selects preferred when present in levels, otherwise
// preferring medium, high, low, and finally the first level.
func ChooseDefaultEffort(levels []ReasoningLevel, preferred string) string {
	if len(levels) == 0 {
		return ""
	}
	want := strings.ToLower(strings.TrimSpace(preferred))
	if want != "" {
		for _, level := range levels {
			if strings.EqualFold(strings.TrimSpace(level.Effort), want) {
				return strings.ToLower(strings.TrimSpace(level.Effort))
			}
		}
	}
	for _, candidate := range []string{"medium", "high", "low"} {
		for _, level := range levels {
			if strings.EqualFold(strings.TrimSpace(level.Effort), candidate) {
				return candidate
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(levels[0].Effort))
}
