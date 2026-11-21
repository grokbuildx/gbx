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
