package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
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
	body  []byte
	model string
}

// convertResponsesToAnthropicMessages translates a Responses request into an
// Anthropic Messages request. The upstream request always streams; a
// non-streaming client response is assembled from that stream.
func convertResponsesToAnthropicMessages(raw []byte) (*anthropicConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripContextModelSuffix(request.Model)

	messages := anthropicMessages(request.Turns)
	if len(messages) == 0 {
		return nil, fmt.Errorf("request has no messages for the Anthropic Messages upstream")
	}

	system := strings.TrimSpace(request.System)
	if schema := structuredOutputSchema(request.Format); schema != nil {
		// Messages has no response_format; ask for the schema in the prompt.
		encoded, _ := json.Marshal(schema["schema"])
		instruction := "Respond only with a JSON value that matches this JSON Schema, with no other text:\n" + string(encoded)
		if system != "" {
			system += "\n\n"
		}
		system += instruction
	}

	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	body := map[string]any{
		"model":      upstreamModel,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}
	if system != "" {
		// A cache breakpoint on the system prompt lets agent turns reuse it.
		body["system"] = []map[string]any{{
			"type": "text", "text": system, "cache_control": map[string]any{"type": "ephemeral"},
		}}
	}
	if len(request.Stop) > 0 {
		body["stop_sequences"] = request.Stop
	}

	forcedTool := false
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, anthropicTool(tool))
		}
		body["tools"] = tools
		if choice, ok := anthropicToolChoice(request.ToolChoice, request.ParallelToolCalls); ok {
			body["tool_choice"] = choice
			kind, _ := choice["type"].(string)
			forcedTool = kind == "any" || kind == "tool"
		}
	}

	// Messages rejects extended thinking together with a forced tool choice, and
	// when the assistant turn of an ongoing tool loop carries no signed thinking
	// (history produced by another backend).
	thinking := request.thinkingEnabled() && !forcedTool && anthropicThinkingReplayable(messages)
	if thinking {
		budget := max(1024, effortBudget(request.Effort))
		if maxTokens <= budget {
			maxTokens = budget + anthropicThinkingHeadroom
			body["max_tokens"] = maxTokens
		}
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	} else {
		// Thinking requires the default sampling parameters.
		if request.Temperature != nil {
			body["temperature"] = *request.Temperature
		} else if request.TopP != nil {
			body["top_p"] = *request.TopP
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic Messages request: %w", err)
	}
	adapter := request.adapter(upstreamModel)
	adapter.thinkingEnabled = thinking
	adapter.maxTokens = maxTokens
	return &anthropicConvertedRequest{adapterRequest: adapter, body: encoded, model: upstreamModel}, nil
}

// anthropicMessages renders conversation turns as Messages, merging adjacent
// turns of one role and dropping turns left empty.
func anthropicMessages(turns []conversationTurn) []map[string]any {
	var messages []map[string]any
	for _, turn := range turns {
		content := anthropicTurnContent(turn)
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
	// A breakpoint on the newest user content caches the conversation prefix.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] != "user" {
			continue
		}
		blocks := messages[i]["content"].([]map[string]any)
		blocks[len(blocks)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		break
	}
	return messages
}

// anthropicThinkingReplayable reports whether thinking may be enabled for this
// history: the latest assistant message, when it called tools, must begin with
// a thinking block this backend signed.
func anthropicThinkingReplayable(messages []map[string]any) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] != "assistant" {
			continue
		}
		blocks := messages[i]["content"].([]map[string]any)
		usesTools := false
		for _, block := range blocks {
			if block["type"] == "tool_use" {
				usesTools = true
			}
		}
		if !usesTools {
			return true
		}
		kind, _ := blocks[0]["type"].(string)
		return kind == "thinking" || kind == "redacted_thinking"
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
	assistant := turn.Role == "assistant"
	var blocks []map[string]any
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			if part.Text != "" {
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
				if block := anthropicReasoning(part); block != nil {
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
// any other backend has no valid signature here and is dropped.
func anthropicReasoning(part conversationPart) map[string]any {
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
		schema = ensureObjectSchema(schema)
	}
	out := map[string]any{"name": tool.Name, "input_schema": schema}
	if tool.Description != "" {
		out["description"] = tool.Description
	}
	return out
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
