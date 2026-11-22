package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatCompletionsConvertedRequest is the GBX-owned wire representation of one
// Responses request destined for an OpenAI Chat Completions upstream.
type chatCompletionsConvertedRequest struct {
	adapterRequest
	body      []byte
	model     string
	sessionID string
}

// convertResponsesToChatCompletions translates a Responses request into an
// OpenAI Chat Completions request.
func convertResponsesToChatCompletions(raw []byte) (*chatCompletionsConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripContextModelSuffix(request.Model)

	messages := make([]map[string]any, 0, len(request.Turns)+1)
	if strings.TrimSpace(request.System) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.System})
	}
	for _, turn := range request.Turns {
		convertedTurns, err := chatConvertTurn(turn)
		if err != nil {
			return nil, err
		}
		messages = append(messages, convertedTurns...)
	}

	body := map[string]any{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   request.Stream,
	}
	if request.MaxOutputTokens > 0 {
		if chatUsesMaxCompletionTokens(upstreamModel) {
			body["max_completion_tokens"] = request.MaxOutputTokens
		} else {
			body["max_tokens"] = request.MaxOutputTokens
		}
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	if len(request.Stop) > 0 {
		body["stop"] = request.Stop
	}
	if request.User != "" {
		body["user"] = request.User
	}
	if request.SafetyIdentifier != "" {
		body["safety_identifier"] = request.SafetyIdentifier
	}
	switch request.ServiceTier {
	case "auto", "default", "flex", "scale", "priority":
		body["service_tier"] = request.ServiceTier
	}
	if request.CacheKey != "" {
		body["prompt_cache_key"] = request.CacheKey
	}
	if request.PromptCacheRetention != "" {
		body["prompt_cache_retention"] = request.PromptCacheRetention
	}
	if request.Seed != nil {
		body["seed"] = *request.Seed
	}
	if request.PresencePenalty != nil {
		body["presence_penalty"] = *request.PresencePenalty
	}
	if request.FrequencyPenalty != nil {
		body["frequency_penalty"] = *request.FrequencyPenalty
	}
	if len(request.LogitBias) > 0 {
		body["logit_bias"] = request.LogitBias
	}
	if request.Logprobs != nil {
		body["logprobs"] = *request.Logprobs
	}
	if request.TopLogprobs != nil {
		body["top_logprobs"] = *request.TopLogprobs
	}
	if request.Effort != "" {
		body["reasoning_effort"] = request.Effort
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, chatTool(tool))
		}
		body["tools"] = tools
		if choice, ok := chatToolChoice(request.ToolChoice); ok {
			body["tool_choice"] = choice
		}
		if request.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *request.ParallelToolCalls
		}
	}
	if request.Format != nil {
		if request.Format.Type == "json_object" {
			body["response_format"] = map[string]any{"type": "json_object"}
		} else if format := structuredOutputSchema(request.Format); format != nil {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": format}
		}
	}
	// Ask the upstream to include usage in the final streaming chunk so token
	// statistics survive even when no dedicated usage event is emitted. Only
	// valid on a streaming request — some upstreams reject stream_options on a
	// non-stream request ("stream_options should be set along with stream = true").
	if request.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI Chat Completions request: %w", err)
	}
	return &chatCompletionsConvertedRequest{
		adapterRequest: request.adapter(upstreamModel),
		body:           encoded,
		model:          upstreamModel,
		sessionID:      firstNonEmptyString(request.CacheKey, request.SafetyIdentifier),
	}, nil
}

// chatUsesMaxCompletionTokens reports whether an OpenAI Chat model requires
// max_completion_tokens instead of the deprecated max_tokens field.
func chatUsesMaxCompletionTokens(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	if idx := strings.LastIndexByte(lower, '/'); idx >= 0 {
		lower = lower[idx+1:]
	}
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5", "gpt-6"} {
		if lower == prefix || strings.HasPrefix(lower, prefix+"-") || strings.HasPrefix(lower, prefix+".") {
			return true
		}
	}
	return false
}

// chatConvertTurn renders one conversation turn into zero or more OpenAI
// messages. Tool results are emitted first (as role:"tool"), then the turn
// body, so OpenAI's "tool must follow the assistant tool_calls" invariant holds.
func chatConvertTurn(turn conversationTurn) ([]map[string]any, error) {
	if turn.Role == "developer" || turn.Role == "system" {
		var texts []string
		for _, part := range turn.Parts {
			if part.Kind == partText && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) == 0 {
			return nil, nil
		}
		return []map[string]any{{"role": turn.Role, "content": strings.Join(texts, "\n")}}, nil
	}

	assistant := turn.Role == "assistant"
	var toolResults []map[string]any
	var contentParts []any
	var textParts []string
	hasMedia := false
	var reasoningParts []string
	var toolCalls []map[string]any

	for _, part := range turn.Parts {
		switch part.Kind {
		case partToolResult:
			if assistant {
				continue
			}
			text := part.outputText()
			images := part.outputImages()
			if len(images) > 0 {
				if text != "" {
					text += "\n"
				}
				text += "[image attached in the following user message]"
			}
			toolResults = append(toolResults, map[string]any{
				"role": "tool", "tool_call_id": part.CallID, "content": text,
			})
			// Chat tool messages accept text only. Keep images in a user message
			// after all tool replies and label their originating call.
			if len(images) > 0 {
				hasMedia = true
				contentParts = append(contentParts, map[string]any{"type": "text", "text": "Images from tool result " + part.CallID + ":"})
				for _, image := range images {
					if image.FileID != "" && image.URL == "" && image.Data == "" {
						return nil, fmt.Errorf("OpenAI Chat Completions does not support image file_id references")
					}
					contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": chatImageURLObject(image)})
				}
			}
		case partReasoning:
			// Only assistant reasoning is honored; anything else is dropped to
			// prevent reasoning-content injection.
			if assistant && part.Text != "" {
				reasoningParts = append(reasoningParts, part.Text)
			}
		case partText, partRefusal:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
				contentParts = append(contentParts, map[string]any{"type": "text", "text": part.Text})
			}
		case partImage:
			if assistant {
				continue
			}
			if part.FileID != "" && part.URL == "" && part.Data == "" {
				return nil, fmt.Errorf("OpenAI Chat Completions does not support image file_id references")
			}
			hasMedia = true
			contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": chatImageURLObject(part)})
		case partFile:
			if assistant {
				continue
			}
			hasMedia = true
			fileObj := map[string]any{}
			if part.FileID != "" {
				fileObj["file_id"] = part.FileID
			}
			if part.Filename != "" {
				fileObj["filename"] = part.Filename
			}
			if part.Data != "" {
				mediaType := part.MediaType
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				fileObj["file_data"] = "data:" + mediaType + ";base64," + part.Data
			} else if part.URL != "" {
				fileObj["file_url"] = part.URL
			}
			contentParts = append(contentParts, map[string]any{"type": "file", "file": fileObj})
		case partAudio:
			if assistant {
				continue
			}
			hasMedia = true
			contentParts = append(contentParts, map[string]any{
				"type": "input_audio",
				"input_audio": map[string]any{
					"data":   part.Data,
					"format": part.Format,
				},
			})
		case partToolCall:
			if assistant && part.CallID != "" {
				toolCalls = append(toolCalls, map[string]any{
					"id":   part.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      part.Name,
						"arguments": string(part.Arguments),
					},
				})
			}
		}
	}

	out := toolResults
	if assistant {
		joined := strings.Join(textParts, "\n")
		if joined == "" && len(reasoningParts) == 0 && len(toolCalls) == 0 {
			return out, nil
		}
		message := map[string]any{"role": "assistant", "content": joined}
		if len(reasoningParts) > 0 {
			message["reasoning_content"] = strings.Join(reasoningParts, "")
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		return append(out, message), nil
	}
	if len(contentParts) > 0 {
		if hasMedia {
			out = append(out, map[string]any{"role": "user", "content": contentParts})
		} else {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
		}
	} else if len(toolResults) == 0 {
		// Preserve an explicit empty user turn boundary separating assistant turns.
		out = append(out, map[string]any{"role": "user", "content": ""})
	}
	return out, nil
}

// chatTool renders one flattened tool as an OpenAI function tool.
func chatTool(tool conversationTool) map[string]any {
	parameters := tool.Parameters
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	} else {
		parameters = ensureObjectSchema(parameters)
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  parameters,
		},
	}
}

// ensureObjectSchema guarantees a parameter schema is an object with a
// properties field and without meta-schema keys that strict gateways reject.
func ensureObjectSchema(schema json.RawMessage) json.RawMessage {
	var value map[string]any
	if decodeProtocolJSON(schema, &value) != nil || value == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	delete(value, "$schema")
	if _, hasProperties := value["properties"]; !hasProperties {
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			variants, _ := value[key].([]any)
			for _, variant := range variants {
				candidate, _ := variant.(map[string]any)
				if candidate["type"] != "object" {
					continue
				}
				for _, field := range []string{"properties", "required", "additionalProperties"} {
					if v, exists := candidate[field]; exists {
						value[field] = v
					}
				}
				break
			}
			if _, hasProperties := value["properties"]; hasProperties {
				break
			}
		}
	}
	if _, ok := value["properties"].(map[string]any); !ok {
		value["properties"] = map[string]any{}
	}
	value["type"] = "object"
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return encoded
}

// chatToolChoice maps a tool choice onto OpenAI Chat's tool_choice. The boolean
// is false when no tool_choice should be sent.
func chatToolChoice(choice conversationToolChoice) (any, bool) {
	switch choice.Mode {
	case toolChoiceAuto, toolChoiceRequired, toolChoiceNone:
		// "none" is sent explicitly: omitting it would let the upstream default
		// to "auto", contradicting the caller's request to disable tools.
		return choice.Mode, true
	case toolChoiceTool:
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, true
	default:
		return nil, false
	}
}

// chatImageURLObject encodes an image part for Chat Completions-style backends.
// Responses' "original" detail is mapped to "high" because Chat Completions
// validates image_url.detail against auto|low|high.
func chatImageURLObject(part conversationPart) map[string]any {
	out := map[string]any{"url": part.imageURL()}
	switch part.Detail {
	case "original", "high":
		out["detail"] = "high"
	case "low", "auto":
		out["detail"] = part.Detail
	}
	return out
}
