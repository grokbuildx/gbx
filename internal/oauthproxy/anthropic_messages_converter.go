package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/grokbuildx/gbx/internal/protocol"
)

// Anthropic Messages upstream for mixed models.dev gateways. Grok Build and
// Codex speak Responses to GBX; models whose AI SDK package is
// @ai-sdk/anthropic are rendered here as Messages requests, and their SSE is
// decoded back into Responses by anthropic_messages_stream.go.

const (
	// anthropicDefaultMaxTokens applies when the client sets no output limit:
	// Messages requires max_tokens.
	anthropicDefaultMaxTokens = 8192
	// anthropicThinkingHeadroom keeps room for the answer after the thinking
	// budget, which counts against max_tokens.
	anthropicThinkingHeadroom = 4096

	// Anthropic reasoning is replayed only to Anthropic. Its signature (and
	// redacted thinking data) travel in Responses encrypted_content behind these
	// prefixes, so reasoning from another backend is never sent here and this
	// backend's signatures are never sent elsewhere.
	anthropicSignaturePrefix = "gbx-anthropic:"
	anthropicRedactedPrefix  = "gbx-anthropic-redacted:"
)

// anthropicConvertedRequest is one Responses request rendered for Messages.
type anthropicConvertedRequest struct {
	adapterRequest
	body                []byte
	model               string
	originalMaxTokens   int
	originalTemperature *float64
	originalTopP        *float64
	requestedEffort     string
	forcedTool          bool
}

type anthropicConvertOptions struct {
	overrideModel         string
	scope                 string
	maxOutputTokens       int
	adaptiveThinking      bool
	supportsThinkingKnown bool
	supportsThinking      bool
	efforts               []string
}

// convertResponsesToAnthropicMessages translates a Responses request into an
// Anthropic Messages request. The upstream request always streams; a
// non-streaming client response is assembled from that stream.
func convertResponsesToAnthropicMessages(raw []byte) (*anthropicConvertedRequest, error) {
	return convertResponsesToAnthropicMessagesWithOptions(raw, anthropicConvertOptions{})
}

func convertResponsesToAnthropicMessagesWithOptions(raw []byte, opts anthropicConvertOptions) (*anthropicConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripContextModelSuffix(request.Model)
	if strings.TrimSpace(opts.overrideModel) != "" {
		upstreamModel = stripContextModelSuffix(opts.overrideModel)
	}

	messages := anthropicMessagesScoped(request.Turns, opts.scope)
	if len(messages) == 0 {
		return nil, fmt.Errorf("request has no messages for the Anthropic Messages upstream")
	}

	system := strings.TrimSpace(request.System)
	if instruction := structuredOutputPromptInstruction(request.Format); instruction != "" {
		if system != "" {
			system += "\n\n"
		}
		system += instruction
	}

	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	if opts.maxOutputTokens > 0 && maxTokens > opts.maxOutputTokens {
		maxTokens = opts.maxOutputTokens
	}
	originalMaxTokens := maxTokens

	var clampedTemp *float64
	if request.Temperature != nil {
		v := clampAnthropicTemperature(*request.Temperature)
		clampedTemp = &v
	}

	body := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if system != "" {
		// Cache breakpoint on the system prompt (breakpoint 1 of up to 4).
		body["system"] = []map[string]any{{
			"type": "text", "text": system, "cache_control": map[string]any{"type": "ephemeral"},
		}}
	}
	if len(request.Stop) > 0 {
		body["stop_sequences"] = request.Stop
	}
	if userID := firstNonEmptyString(request.SafetyIdentifier, request.User); userID != "" {
		body["metadata"] = map[string]any{"user_id": userID}
	}

	forcedTool := false
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, anthropicTool(tool))
		}
		// Cache breakpoint on the last tool definition (breakpoint 2 of up to 4).
		tools[len(tools)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		body["tools"] = tools
		if choice, ok := anthropicToolChoice(request.ToolChoice, request.ParallelToolCalls); ok {
			body["tool_choice"] = choice
			kind, _ := choice["type"].(string)
			forcedTool = kind == "any" || kind == "tool"
		}
	}

	// Messages rejects extended thinking together with a forced tool choice, and
	// when any assistant turn in the current tool loop carries no signed thinking
	// (history produced by another backend or another Anthropic model/endpoint).
	knownNonThinking := opts.supportsThinkingKnown && !opts.supportsThinking
	thinking := false
	if request.Effort == "none" && !knownNonThinking && ((opts.supportsThinkingKnown && opts.supportsThinking) || isKnownAnthropicAdaptiveModel(upstreamModel)) {
		body["thinking"] = map[string]any{"type": "disabled"}
		applyAnthropicSampling(body, clampedTemp, request.TopP)
	} else if request.thinkingEnabled() && !forcedTool && !knownNonThinking && anthropicThinkingReplayable(messages) {
		useAdaptive := opts.adaptiveThinking || (!opts.supportsThinkingKnown && isKnownAnthropicAdaptiveModel(upstreamModel))
		if useAdaptive {
			thinking = true
			body["thinking"] = map[string]any{"type": "adaptive"}
			body["output_config"] = map[string]any{"effort": clampAnthropicEffort(request.Effort, opts.efforts)}
		} else {
			budget := max(1024, effortBudget(request.Effort))
			if maxTokens <= budget {
				if request.MaxOutputTokens == 0 {
					target := budget + anthropicThinkingHeadroom
					if opts.maxOutputTokens > 0 && target > opts.maxOutputTokens {
						target = opts.maxOutputTokens
					}
					maxTokens = target
				}
				if budget >= maxTokens {
					if maxTokens >= 2048 {
						budget = maxTokens - 1024
					} else {
						budget = 0
					}
				}
			}
			if budget >= 1024 && maxTokens > budget {
				thinking = true
				body["max_tokens"] = maxTokens
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			} else {
				applyAnthropicSampling(body, clampedTemp, request.TopP)
			}
		}
	} else {
		// Thinking requires the default sampling parameters.
		applyAnthropicSampling(body, clampedTemp, request.TopP)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic Messages request: %w", err)
	}
	adapter := request.adapter(upstreamModel)
	adapter.thinkingEnabled = thinking
	adapter.reasoningScope = opts.scope
	return &anthropicConvertedRequest{
		adapterRequest:      adapter,
		body:                encoded,
		model:               upstreamModel,
		originalMaxTokens:   originalMaxTokens,
		originalTemperature: clampedTemp,
		originalTopP:        request.TopP,
		requestedEffort:     request.Effort,
		forcedTool:          forcedTool,
	}, nil
}

func applyAnthropicSampling(body map[string]any, temperature, topP *float64) {
	if temperature != nil {
		body["temperature"] = *temperature
	} else if topP != nil {
		body["top_p"] = *topP
	}
}

func clampAnthropicTemperature(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// isKnownAnthropicAdaptiveModel matches Anthropic models that require
// thinking: {"type": "adaptive"} + output_config.effort instead of
// budget_tokens when models.dev metadata is unavailable.
func isKnownAnthropicAdaptiveModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	if idx := strings.LastIndexByte(lower, '/'); idx >= 0 {
		lower = lower[idx+1:]
	}
	lower = strings.ReplaceAll(lower, ".", "-")
	for _, prefix := range []string{
		"claude-opus-4-7",
		"claude-opus-4-8",
		"claude-opus-5",
		"claude-sonnet-5",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func clampAnthropicEffort(asked string, catalogEfforts []string) string {
	asked = strings.ToLower(strings.TrimSpace(asked))
	if asked == "" || asked == "none" {
		return "medium"
	}
	if len(catalogEfforts) > 0 {
		levels := make([]protocol.ReasoningLevel, 0, len(catalogEfforts))
		for _, e := range catalogEfforts {
			levels = append(levels, protocol.ReasoningLevel{Effort: e})
		}
		if clamped := protocol.ClampReasoningLevel(asked, levels); clamped != "" {
			return clamped
		}
	}
	switch asked {
	case "minimal":
		return "low"
	case "low", "medium", "high", "xhigh", "max":
		return asked
	case "ultra":
		return "max"
	default:
		return "high"
	}
}

// anthropicMessages renders conversation turns as Messages, merging adjacent
// turns of one role and dropping turns left empty.
func anthropicMessages(turns []conversationTurn) []map[string]any {
	return anthropicMessagesScoped(turns, "")
}

func anthropicMessagesScoped(turns []conversationTurn, scope string) []map[string]any {
	var messages []map[string]any
	for _, turn := range turns {
		content := anthropicTurnContentScoped(turn, scope)
		if len(content) == 0 {
			continue
		}
		role := "user"
		if turn.Role == "assistant" {
			role = "assistant"
		}
		if last := len(messages) - 1; last >= 0 && messages[last]["role"] == role {
			merged := append(messages[last]["content"].([]map[string]any), content...)
			messages[last]["content"] = anthropicOrderBlocks(role, merged)
			continue
		}
		messages = append(messages, map[string]any{"role": role, "content": anthropicOrderBlocks(role, content)})
	}
	// Messages must start with a user turn.
	if len(messages) > 0 && messages[0]["role"] != "user" {
		messages = append([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Continue."}}}}, messages...)
	}
	// Rolling cache breakpoints on up to the 2 most recent user turns
	// (combined with tools[last] and system[last], stays within Anthropic's 4-breakpoint cap).
	userBreakpoints := 0
	for i := len(messages) - 1; i >= 0 && userBreakpoints < 2; i-- {
		if messages[i]["role"] != "user" {
			continue
		}
		blocks := messages[i]["content"].([]map[string]any)
		for j := len(blocks) - 1; j >= 0; j-- {
			if blocks[j]["type"] == "text" && strings.TrimSpace(stringValue(blocks[j]["text"])) == "" {
				continue
			}
			blocks[j]["cache_control"] = map[string]any{"type": "ephemeral"}
			userBreakpoints++
			break
		}
	}
	return messages
}

// anthropicThinkingReplayable reports whether thinking may be enabled for this
// history: every assistant message since the most recent non-tool_result user
// prompt that called tools must begin with a thinking block signed for this
// backend.
func anthropicThinkingReplayable(messages []map[string]any) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		blocks, _ := messages[i]["content"].([]map[string]any)
		if role == "user" {
			if !anthropicIsToolResultOnlyMessage(blocks) {
				// Reached the start of the current tool loop.
				return true
			}
			continue
		}
		if role != "assistant" || len(blocks) == 0 {
			continue
		}
		usesTools := false
		for _, block := range blocks {
			if block["type"] == "tool_use" {
				usesTools = true
				break
			}
		}
		if !usesTools {
			continue
		}
		kind, _ := blocks[0]["type"].(string)
		if kind != "thinking" && kind != "redacted_thinking" {
			return false
		}
	}
	return true
}

func anthropicIsToolResultOnlyMessage(blocks []map[string]any) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block["type"] != "tool_result" {
			return false
		}
	}
	return true
}

// anthropicOrderBlocks applies Messages' ordering rules: tool results lead a
// user message, and thinking leads an assistant message.
func anthropicOrderBlocks(role string, blocks []map[string]any) []map[string]any {
	first := "tool_result"
	if role == "assistant" {
		first = "thinking"
	}
	ordered := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		if kind == first || first == "thinking" && kind == "redacted_thinking" {
			ordered = append(ordered, block)
		}
	}
	for _, block := range blocks {
		kind, _ := block["type"].(string)
		if !(kind == first || first == "thinking" && kind == "redacted_thinking") {
			ordered = append(ordered, block)
		}
	}
	return ordered
}

// anthropicTurnContent renders one turn's parts as Messages content blocks.
func anthropicTurnContent(turn conversationTurn) []map[string]any {
	return anthropicTurnContentScoped(turn, "")
}

func anthropicTurnContentScoped(turn conversationTurn, scope string) []map[string]any {
	assistant := turn.Role == "assistant"
	var blocks []map[string]any
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			if strings.TrimSpace(part.Text) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case partImage:
			if !assistant {
				if image := anthropicImage(part); image != nil {
					blocks = append(blocks, image)
				}
			}
		case partReasoning:
			if assistant {
				if block := anthropicReasoningScoped(part, scope); block != nil {
					blocks = append(blocks, block)
				}
			}
		case partToolCall:
			if assistant && part.CallID != "" {
				var input any = map[string]any{}
				if len(part.Arguments) > 0 {
					if err := decodeProtocolJSON(part.Arguments, &input); err != nil || input == nil {
						input = map[string]any{}
					}
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": part.CallID, "name": part.Name, "input": input})
			}
		case partToolResult:
			if !assistant && part.CallID != "" {
				blocks = append(blocks, anthropicToolResult(part))
			}
		}
	}
	return blocks
}

// anthropicReasoning restores thinking this backend produced. Reasoning from
// any other backend or mismatched model/endpoint scope has no valid signature
// here and is dropped.
func anthropicReasoning(part conversationPart) map[string]any {
	return anthropicReasoningScoped(part, "")
}

func anthropicReasoningScoped(part conversationPart, scope string) map[string]any {
	if scope != "" && !protocol.ReasoningScopeMatches(part.Signature, scope) {
		return nil
	}
	if data, ok := strings.CutPrefix(part.Signature, anthropicRedactedPrefix); ok && data != "" {
		return map[string]any{"type": "redacted_thinking", "data": data}
	}
	if signature, ok := strings.CutPrefix(part.Signature, anthropicSignaturePrefix); ok && signature != "" {
		return map[string]any{"type": "thinking", "thinking": part.Text, "signature": signature}
	}
	return nil
}

func anthropicImage(part conversationPart) map[string]any {
	switch {
	case part.Data != "":
		mediaType := part.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": part.Data}}
	case part.URL != "":
		return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": part.URL}}
	default:
		return nil
	}
}

func anthropicToolResult(part conversationPart) map[string]any {
	var content []map[string]any
	for _, piece := range part.Output {
		switch piece.Kind {
		case partText:
			if piece.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": piece.Text})
			}
		case partImage:
			if image := anthropicImage(piece); image != nil {
				content = append(content, image)
			}
		}
	}
	block := map[string]any{"type": "tool_result", "tool_use_id": part.CallID}
	if len(content) > 0 {
		block["content"] = content
	} else {
		block["content"] = ""
	}
	return block
}

func anthropicTool(tool conversationTool) map[string]any {
	schema := tool.Parameters
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	} else {
		schema = ensureAnthropicInputSchema(schema)
	}
	out := map[string]any{"name": tool.Name, "input_schema": schema}
	if tool.Description != "" {
		out["description"] = tool.Description
	}
	return out
}

// ensureAnthropicInputSchema builds a strict object input_schema for Anthropic
// Messages, which rejects top-level $schema and root oneOf/anyOf/allOf.
func ensureAnthropicInputSchema(raw json.RawMessage) json.RawMessage {
	normalized := ensureObjectSchema(raw)
	var value map[string]any
	if decodeProtocolJSON(normalized, &value) != nil || value == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	removed := false
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if _, exists := value[key]; exists {
			delete(value, key)
			removed = true
		}
	}
	if !removed {
		return normalized
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return normalized
	}
	return encoded
}

// anthropicToolChoice maps a tool choice onto Messages' tool_choice. The
// boolean is false when the upstream default (auto, parallel) applies.
func anthropicToolChoice(choice conversationToolChoice, parallel *bool) (map[string]any, bool) {
	var out map[string]any
	switch choice.Mode {
	case toolChoiceNone:
		return map[string]any{"type": "none"}, true
	case toolChoiceRequired:
		out = map[string]any{"type": "any"}
	case toolChoiceTool:
		out = map[string]any{"type": "tool", "name": choice.Name}
	default:
		out = map[string]any{"type": "auto"}
	}
	if parallel != nil && !*parallel {
		out["disable_parallel_tool_use"] = true
	} else if out["type"] == "auto" {
		return nil, false
	}
	return out, true
}
