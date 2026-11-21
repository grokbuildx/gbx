package oauthproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// A Responses upstream normally receives the client's request unchanged (see
// handleResponses). The conversion below is only for a request whose tool
// dialect the upstream does not implement: it is re-rendered with every tool
// flattened to a plain function, and the assembler maps returned calls back.

// codexResponsesConvertedRequest is one Responses request re-rendered for a
// Responses upstream.
type codexResponsesConvertedRequest struct {
	adapterRequest
	body      []byte
	model     string
	sessionID string
}

// responsesNeedFlattening reports whether a request uses tool kinds only the
// OpenAI/Codex backend implements: custom (freeform) tools, namespaces and
// client-executed tool search.
func responsesNeedFlattening(payload map[string]any) bool {
	tools, _ := payload["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		switch kind {
		case "custom", "namespace", "computer", "computer_use_preview", "shell", "local_shell", "apply_patch":
			return true
		case "tool_search":
			if tool["execution"] == "client" {
				return true
			}
		default:
			if strings.HasPrefix(kind, "computer_use") {
				return true
			}
		}
	}
	items, _ := payload["input"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "custom_tool_call", "custom_tool_call_output",
			"tool_search_call", "tool_search_output", "compaction",
			"computer_call", "computer_call_output",
			"shell_call", "shell_call_output",
			"local_shell_call", "local_shell_call_output",
			"apply_patch_call", "apply_patch_call_output",
			"mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response":
			return true
		case "function_call_output":
			if responsesOutputNeedsImageNormalization(item["output"]) {
				return true
			}
		}
		if namespace, _ := item["namespace"].(string); namespace != "" {
			return true
		}
	}
	return false
}

func responsesOutputNeedsImageNormalization(raw any) bool {
	switch value := raw.(type) {
	case map[string]any:
		return responsesImagePartNeedsNormalization(value)
	case []any:
		for _, elem := range value {
			if m, ok := elem.(map[string]any); ok && responsesImagePartNeedsNormalization(m) {
				return true
			}
		}
	}
	return false
}

func responsesImagePartNeedsNormalization(part map[string]any) bool {
	kind, _ := part["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "computer_screenshot", "image", "image_url":
		return true
	case "input_image":
		if _, isMap := part["image_url"].(map[string]any); isMap {
			return true
		}
		if urlVal, _ := part["image_url"].(string); strings.HasPrefix(strings.TrimSpace(urlVal), "data:") {
			if header, _, ok := strings.Cut(strings.TrimSpace(urlVal), ","); ok {
				meta := strings.TrimPrefix(header, "data:")
				// Standard data:image/<type>;base64 has no extra parameters or empty MIME.
				mime, isBase64 := strings.CutSuffix(meta, ";base64")
				if !isBase64 || strings.TrimSpace(mime) == "" || strings.Contains(mime, ";") {
					return true
				}
			}
		}
	}
	return false
}

// convertResponsesForUpstream re-renders a Responses request with plain
// function tools.
func convertResponsesForUpstream(raw []byte, sessionID string) (*codexResponsesConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	upstreamModel := stripContextModelSuffix(request.Model)
	input := make([]any, 0, len(request.Turns))
	for _, turn := range request.Turns {
		items, err := codexTurnItems(turn)
		if err != nil {
			return nil, err
		}
		input = append(input, items...)
	}
	reasoning := map[string]any{}
	if request.Effort != "" {
		reasoning["effort"] = request.Effort
	}
	if request.thinkingEnabled() {
		reasoning["summary"] = "auto"
	}
	body := map[string]any{
		"model":        upstreamModel,
		"instructions": request.System,
		"input":        input,
		"stream":       true,
		"store":        false,
		"include":      []string{"reasoning.encrypted_content"},
	}
	if len(reasoning) > 0 {
		body["reasoning"] = reasoning
	}
	if request.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	if request.MaxOutputTokens > 0 {
		body["max_output_tokens"] = request.MaxOutputTokens
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	if request.User != "" {
		body["user"] = request.User
	}
	if format := structuredOutputSchema(request.Format); format != nil {
		format["type"] = "json_schema"
		body["text"] = map[string]any{"format": format}
	}
	switch request.ServiceTier {
	case "priority", "flex", "default", "auto":
		body["service_tier"] = request.ServiceTier
	}
	switch request.Truncation {
	case "auto", "disabled":
		body["truncation"] = request.Truncation
	}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			parameters := map[string]any{}
			if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
				if err := decodeProtocolJSON(tool.Parameters, &parameters); err != nil {
					return nil, fmt.Errorf("invalid parameters for tool %s: %w", tool.Name, err)
				}
			}
			if stringValue(parameters["type"]) == "" {
				parameters["type"] = "object"
			}
			if parameters["type"] == "object" && parameters["properties"] == nil {
				parameters["properties"] = map[string]any{}
			}
			delete(parameters, "$schema")
			tools = append(tools, map[string]any{
				"type": "function", "name": tool.Name, "description": tool.Description,
				"parameters": parameters, "strict": false,
			})
		}
		body["tools"] = tools
		switch request.ToolChoice.Mode {
		case toolChoiceTool:
			body["tool_choice"] = map[string]any{"type": "function", "name": request.ToolChoice.Name}
		case toolChoiceNone, toolChoiceRequired:
			body["tool_choice"] = request.ToolChoice.Mode
		default:
			body["tool_choice"] = toolChoiceAuto
		}
	}
	if request.CacheKey != "" {
		body["prompt_cache_key"] = request.CacheKey
	} else if sessionID != "" {
		body["prompt_cache_key"] = sessionID
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Responses request: %w", err)
	}
	return &codexResponsesConvertedRequest{
		adapterRequest: request.adapter(upstreamModel),
		body:           encoded, model: upstreamModel, sessionID: sessionID,
	}, nil
}

// codexTurnItems renders one conversation turn as Responses input items.
func codexTurnItems(turn conversationTurn) ([]any, error) {
	textType := "input_text"
	if turn.Role == "assistant" {
		textType = "output_text"
	}
	result := make([]any, 0, len(turn.Parts))
	var content []any
	flush := func() {
		if len(content) == 0 {
			return
		}
		result = append(result, map[string]any{"type": "message", "role": turn.Role, "content": content})
		content = nil
	}
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText:
			content = append(content, map[string]any{"type": textType, "text": part.Text})
		case partImage:
			content = append(content, responsesInputImagePart(part))
		case partReasoning:
			if turn.Role != "assistant" || !strings.HasPrefix(part.Signature, codexReasoningSignaturePrefix) {
				continue
			}
			item, err := codexReasoningFromSignature(part.Signature)
			if err != nil {
				return nil, err
			}
			flush()
			result = append(result, item)
		case partToolCall:
			flush()
			result = append(result, map[string]any{
				"type": "function_call", "call_id": part.CallID, "name": part.Name, "arguments": string(part.Arguments),
			})
		case partToolResult:
			flush()
			var output any = part.outputText()
			if images := part.outputImages(); len(images) > 0 {
				parts := make([]any, 0, len(part.Output))
				for _, piece := range part.Output {
					if piece.Kind == partImage {
						parts = append(parts, responsesInputImagePart(piece))
					} else if piece.Text != "" {
						parts = append(parts, map[string]any{"type": "input_text", "text": piece.Text})
					}
				}
				output = parts
			}
			result = append(result, map[string]any{"type": "function_call_output", "call_id": part.CallID, "output": output})
		}
	}
	flush()
	return result, nil
}

func responsesInputImagePart(part conversationPart) map[string]any {
	out := map[string]any{"type": "input_image", "image_url": part.imageURL()}
	if part.Detail != "" {
		out["detail"] = part.Detail
	}
	return out
}

// The client persists a reasoning item's encrypted_content verbatim. Carry the
// entire upstream reasoning item there so IDs, status, summary, null/absent
// fields and future fields survive the round trip, including across restarts.
const codexReasoningSignaturePrefix = "gbx-responses-reasoning-v1:"

func codexReasoningSignature(item map[string]any) (string, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	return codexReasoningSignaturePrefix + base64.StdEncoding.EncodeToString(raw), nil
}

func codexReasoningFromSignature(signature string) (any, error) {
	encoded, _ := strings.CutPrefix(signature, codexReasoningSignaturePrefix)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	var item map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &item) != nil || string(item["type"]) != `"reasoning"` {
		return nil, fmt.Errorf("invalid GBX Responses reasoning payload")
	}
	return json.RawMessage(raw), nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
