package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grokbuildx/gbx/internal/modelrouting"
)

type qoderModel struct {
	ID                 string
	Name               string
	ContextWindow      int
	MaxTokens          int
	PriceFactor        *float64
	Reasoning          bool
	Vision             bool
	IsNew              bool
	PromotionAvailable bool
	Source             string
	Config             map[string]any
}

type qoderConvertedRequest struct {
	clientModel string
	model       qoderModel
	stream      bool
	body        map[string]any
	adapter     *adapterRequest
	inputTokens int
}

type qoderToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func convertResponsesToQoder(raw []byte, catalog []qoderModel) (*qoderConvertedRequest, error) {
	return convertResponsesToQoderWithSession(raw, catalog, "")
}

func convertResponsesToQoderWithSession(raw []byte, catalog []qoderModel, httpSessionID string) (*qoderConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	model, ok := qoderSelectModel(request.Model, catalog)
	if !ok {
		return nil, fmt.Errorf("Qoder has no available models")
	}
	if err := validateQoderTurns(request.Turns); err != nil {
		return nil, err
	}
	messages, lastUser := qoderTransformTurns(request.Turns)
	if system := strings.TrimSpace(request.System); system != "" {
		messages = append([]any{map[string]any{"role": "system", "content": system}}, messages...)
	}
	var tools []any
	if request.ToolChoice.Mode != toolChoiceNone {
		tools, err = qoderTransformTools(request.Tools)
		if err != nil {
			return nil, err
		}
	}
	maxTokens := request.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 32_768
	}
	if model.MaxTokens > 0 && maxTokens > model.MaxTokens {
		maxTokens = model.MaxTokens
	}
	modelConfig := make(map[string]any, len(model.Config)+3)
	for key, value := range model.Config {
		modelConfig[key] = value
	}
	modelConfig["key"] = model.ID
	if _, exists := modelConfig["is_reasoning"]; !exists {
		modelConfig["is_reasoning"] = model.Reasoning
	}
	if _, exists := modelConfig["max_output_tokens"]; !exists {
		modelConfig["max_output_tokens"] = maxTokens
	}
	if _, exists := modelConfig["source"]; !exists {
		modelConfig["source"] = model.Source
	}
	parameters := map[string]any{"max_tokens": maxTokens}
	if request.Temperature != nil {
		parameters["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		parameters["top_p"] = *request.TopP
	}
	if len(request.Stop) > 0 {
		parameters["stop"] = request.Stop
	}
	if request.ParallelToolCalls != nil {
		parameters["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	if request.Format != nil {
		if request.Format.Type == "json_object" {
			parameters["response_format"] = map[string]any{"type": "json_object"}
		} else if format := structuredOutputSchema(request.Format); format != nil {
			parameters["response_format"] = map[string]any{"type": "json_schema", "json_schema": format}
		}
	}
	if len(tools) > 0 {
		switch request.ToolChoice.Mode {
		case toolChoiceTool:
			parameters["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": request.ToolChoice.Name}}
		case toolChoiceRequired:
			parameters["tool_choice"] = toolChoiceRequired
		}
	}
	thinkingOff := strings.EqualFold(request.Effort, "none")
	if levels, _ := qoderReasoningLevels(model.Config); len(levels) > 0 {
		if thinkingOff {
			parameters["enable_thinking"] = false
		} else if matched := qoderMatchEffort(levels, request.Effort); matched != "" {
			parameters["enable_thinking"] = true
			parameters["reasoning_effort"] = matched
			delete(parameters, "temperature")
			delete(parameters, "top_p")
		}
	} else if thinkingOff && model.Reasoning {
		parameters["enable_thinking"] = false
	}
	recordID := qoderStableID("qoder-record", request.Model, string(raw), fmt.Sprintf("mt=%d", maxTokens))
	sessionSeed := firstNonEmptyString(strings.TrimSpace(request.CacheKey), strings.TrimSpace(httpSessionID), strings.TrimSpace(request.SafetyIdentifier), strings.TrimSpace(request.User), uuid.NewString())
	sessionID := qoderStableID("qoder-session", model.ID, sessionSeed) + "-" + sessionSeed
	body := map[string]any{
		"request_id":       uuid.NewString(),
		"request_set_id":   recordID,
		"chat_record_id":   recordID,
		"session_id":       sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		"system":           "",
		"messages":         messages,
		"tools":            tools,
		"parameters":       parameters,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": model.ID, "is_reasoning": model.Reasoning},
				"originalContent": lastUser,
			},
			"features": []any{},
			"text":     lastUser,
		},
		"model_config": modelConfig,
		"business": map[string]any{
			"product":  "cli",
			"version":  "1.0.0",
			"type":     "agent",
			"stage":    "start",
			"id":       uuid.NewString(),
			"name":     qoderPrefix(lastUser, 30),
			"begin_at": time.Now().UnixMilli(),
		},
	}
	adapter := request.adapter(model.ID)
	adapter.toolNameMap = map[string]string{}
	return &qoderConvertedRequest{
		clientModel: request.Model,
		model:       model,
		stream:      request.Stream,
		body:        body,
		adapter:     &adapter,
		inputTokens: adapter.inputTokens,
	}, nil
}

func validateQoderTurns(turns []conversationTurn) error {
	for _, turn := range turns {
		for _, part := range turn.Parts {
			switch part.Kind {
			case partFile:
				return fmt.Errorf("Qoder does not support input_file content parts")
			case partAudio:
				return fmt.Errorf("Qoder does not support input_audio content parts")
			case partImage:
				if part.FileID != "" && part.URL == "" && part.Data == "" {
					return fmt.Errorf("Qoder does not support image file_id references")
				}
			case partToolResult:
				for _, sub := range part.Output {
					if sub.Kind == partFile {
						return fmt.Errorf("Qoder does not support input_file content parts")
					}
					if sub.Kind == partAudio {
						return fmt.Errorf("Qoder does not support input_audio content parts")
					}
					if sub.Kind == partImage && sub.FileID != "" && sub.URL == "" && sub.Data == "" {
						return fmt.Errorf("Qoder does not support image file_id references")
					}
				}
			}
		}
	}
	return nil
}

func qoderSelectModel(requested string, catalog []qoderModel) (qoderModel, bool) {
	base := stripContextModelSuffix(strings.TrimSpace(requested))
	for _, model := range catalog {
		if strings.EqualFold(model.ID, base) {
			return model, true
		}
	}
	for _, model := range catalog {
		if strings.EqualFold(strings.TrimSpace(model.Name), base) {
			return model, true
		}
	}
	ids := make([]string, 0, len(catalog))
	for _, model := range catalog {
		ids = append(ids, model.ID)
	}
	selected := modelrouting.MapModel(base, "", ids)
	for _, model := range catalog {
		if strings.EqualFold(model.ID, selected) {
			return model, true
		}
	}
	return qoderModel{}, false
}

func qoderTransformTools(tools []conversationTool) ([]any, error) {
	transformed := make([]any, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return nil, fmt.Errorf("tool name is required")
		}
		schemaBytes := tool.Parameters
		if len(schemaBytes) == 0 || string(schemaBytes) == "null" {
			schemaBytes = json.RawMessage(`{"type":"object","properties":{}}`)
		} else {
			var probe map[string]any
			if err := decodeProtocolJSON(schemaBytes, &probe); err != nil {
				return nil, fmt.Errorf("tool %s has invalid parameters: %w", name, err)
			}
			schemaBytes = ensureObjectSchema(schemaBytes)
		}
		transformed = append(transformed, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool.Description,
				"parameters":  schemaBytes,
			},
		})
	}
	return transformed, nil
}

func qoderTransformTurns(turns []conversationTurn) ([]any, string) {
	transformed := make([]any, 0, len(turns)+2)
	lastUser := ""
	for _, turn := range turns {
		switch turn.Role {
		case "user":
			mapped, userText := qoderTransformUserTurn(turn)
			transformed = append(transformed, mapped...)
			if strings.TrimSpace(userText) != "" {
				lastUser = userText
			}
		case "developer", "system":
			var texts []string
			for _, part := range turn.Parts {
				if part.Kind == partText && strings.TrimSpace(part.Text) != "" {
					texts = append(texts, part.Text)
				}
			}
			if len(texts) > 0 {
				transformed = append(transformed, map[string]any{
					"role":    "user",
					"content": strings.Join(texts, "\n"),
				})
			}
		case "assistant":
			if msg := qoderTransformAssistantTurn(turn); msg != nil {
				transformed = append(transformed, msg)
			}
		}
	}
	return transformed, lastUser
}

func qoderTransformUserTurn(turn conversationTurn) ([]any, string) {
	toolMessages := make([]any, 0, len(turn.Parts))
	parts := make([]any, 0, len(turn.Parts))
	var toolImages []any
	var textParts []string
	hasImages := false
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText, partRefusal:
			if part.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": part.Text})
				textParts = append(textParts, part.Text)
			}
		case partImage:
			if imageURL := part.imageURL(); imageURL != "" {
				hasImages = true
				parts = append(parts, map[string]any{"type": "image_url", "image_url": chatImageURLObject(part)})
			}
		case partToolResult:
			content := part.outputText()
			var toolText []string
			hasImage := false
			// Tool messages carry text and precede any user parts so
			// assistant(tool_calls) -> tool adjacency is always preserved.
			for _, sub := range part.Output {
				if sub.Kind == partText {
					toolText = append(toolText, sub.Text)
				}
				if sub.Kind == partImage {
					if imageURL := sub.imageURL(); imageURL != "" {
						hasImage = true
						hasImages = true
						toolText = append(toolText, "[image attached in the following user message]")
						toolImages = append(toolImages, map[string]any{"type": "text", "text": "Image from tool result " + part.CallID + ":"}, map[string]any{"type": "image_url", "image_url": chatImageURLObject(sub)})
					}
				}
			}
			if hasImage {
				content = strings.Join(toolText, "\n")
			}
			toolMessages = append(toolMessages, map[string]any{
				"role":         "tool",
				"tool_call_id": part.CallID,
				"content":      content,
			})
		}
	}
	messages := make([]any, 0, len(toolMessages)+1)
	messages = append(messages, toolMessages...)
	parts = append(parts, toolImages...)
	joinedText := strings.Join(textParts, "\n")
	if len(parts) > 0 {
		content := any(parts)
		if !hasImages {
			content = joinedText
		}
		messages = append(messages, map[string]any{"role": "user", "content": content})
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": ""})
	}
	return messages, joinedText
}

func qoderTransformAssistantTurn(turn conversationTurn) any {
	var textParts []string
	var reasoningParts []string
	toolCalls := make([]qoderToolCall, 0)
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText, partRefusal:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		case partReasoning:
			if strings.TrimSpace(part.Text) != "" {
				reasoningParts = append(reasoningParts, part.Text)
			}
		case partToolCall:
			arguments := strings.TrimSpace(string(part.Arguments))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			call := qoderToolCall{ID: part.CallID, Type: "function"}
			call.Function.Name = part.Name
			call.Function.Arguments = arguments
			toolCalls = append(toolCalls, call)
		}
	}
	var content strings.Builder
	for _, r := range reasoningParts {
		content.WriteString("<thinking>")
		content.WriteString(r)
		content.WriteString("</thinking>\n\n")
	}
	if len(textParts) > 0 {
		content.WriteString(strings.Join(textParts, "\n"))
	}
	if content.Len() == 0 && len(toolCalls) == 0 {
		return nil
	}
	message := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else {
		message["content"] = " "
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message
}

func qoderStableID(prefix string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefix))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

func qoderPrefix(value string, maximum int) string {
	characters := []rune(value)
	if maximum > 0 && len(characters) > maximum {
		characters = characters[:maximum]
	}
	return string(characters)
}
