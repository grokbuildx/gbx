package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Chat Completions front. A Chat request is rewritten as a Responses request,
// served by the backend's Responses handler, and the Responses output is
// rendered back as Chat Completions. Backends therefore implement one inbound
// protocol, and every subscription is reachable over Chat.

const chatFrontMaxBodyBytes = int64(128 << 20)

type chatFrontRequest struct {
	Model                string             `json:"model"`
	Messages             []map[string]any   `json:"messages"`
	Tools                []map[string]any   `json:"tools"`
	ToolChoice           any                `json:"tool_choice"`
	ParallelToolCalls    *bool              `json:"parallel_tool_calls"`
	Stream               bool               `json:"stream"`
	StreamOptions        map[string]any     `json:"stream_options"`
	MaxTokens            int                `json:"max_tokens"`
	MaxCompletionTokens  int                `json:"max_completion_tokens"`
	ReasoningEffort      string             `json:"reasoning_effort"`
	ResponseFormat       map[string]any     `json:"response_format"`
	Temperature          *float64           `json:"temperature"`
	TopP                 *float64           `json:"top_p"`
	Stop                 any                `json:"stop"`
	User                 string             `json:"user"`
	SafetyIdentifier     string             `json:"safety_identifier"`
	ServiceTier          string             `json:"service_tier"`
	PromptCacheKey       string             `json:"prompt_cache_key"`
	PromptCacheRetention string             `json:"prompt_cache_retention"`
	Metadata             map[string]any     `json:"metadata"`
	Verbosity            string             `json:"verbosity"`
	Seed                 *int64             `json:"seed"`
	PresencePenalty      *float64           `json:"presence_penalty"`
	FrequencyPenalty     *float64           `json:"frequency_penalty"`
	LogitBias            map[string]float64 `json:"logit_bias"`
	Logprobs             *bool              `json:"logprobs"`
	TopLogprobs          *int               `json:"top_logprobs"`
}

// chatToResponsesRequest rewrites a Chat Completions body as a Responses body.
func chatToResponsesRequest(raw []byte) ([]byte, *chatFrontRequest, error) {
	var request chatFrontRequest
	if err := decodeProtocolJSON(raw, &request); err != nil {
		return nil, nil, fmt.Errorf("invalid Chat Completions request: %w", err)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, nil, fmt.Errorf("model is required")
	}
	if len(request.Messages) == 0 {
		return nil, nil, fmt.Errorf("messages must not be empty")
	}
	input := make([]map[string]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer", "user":
			content, err := chatFrontContent(message["content"], "input_text")
			if err != nil {
				return nil, nil, err
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": content})
			}
		case "assistant":
			reasoning, _ := message["reasoning_content"].(string)
			if reasoning == "" {
				reasoning, _ = message["reasoning"].(string)
			}
			if reasoning != "" {
				input = append(input, map[string]any{
					"type": "reasoning", "id": assemblerItemID(gbxReasoningIDPrefix),
					"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
				})
			}
			content, err := chatFrontContent(message["content"], "output_text")
			if err != nil {
				return nil, nil, err
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": content})
			}
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := function["name"].(string)
				if id == "" || name == "" {
					return nil, nil, fmt.Errorf("assistant tool_calls require id and function.name")
				}
				arguments, _ := function["arguments"].(string)
				if strings.TrimSpace(arguments) == "" {
					arguments = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments})
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if id == "" {
				return nil, nil, fmt.Errorf("tool messages require tool_call_id")
			}
			var output any = ""
			switch content := message["content"].(type) {
			case nil:
				output = ""
			case string:
				output = content
			case []any:
				parts, err := chatFrontContent(content, "input_text")
				if err != nil {
					return nil, nil, err
				}
				output = parts
			default:
				output = responsesTextValue(content)
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": output})
		default:
			return nil, nil, fmt.Errorf("unsupported Chat Completions message role %q", role)
		}
	}
	body := map[string]any{"model": request.Model, "input": input, "stream": request.Stream}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			function, ok := tool["function"].(map[string]any)
			if tool["type"] != "function" || !ok {
				return nil, nil, fmt.Errorf("only function tools are supported over Chat Completions")
			}
			converted := map[string]any{"type": "function", "name": function["name"]}
			for _, key := range []string{"description", "parameters", "strict"} {
				if value, exists := function[key]; exists {
					converted[key] = value
				}
			}
			tools = append(tools, converted)
		}
		body["tools"] = tools
	}
	switch choice := request.ToolChoice.(type) {
	case string:
		body["tool_choice"] = choice
	case map[string]any:
		choiceType, _ := choice["type"].(string)
		name, _ := choice["name"].(string)
		if function, ok := choice["function"].(map[string]any); ok && name == "" {
			name, _ = function["name"].(string)
		}
		if strings.TrimSpace(name) == "" && (choiceType == "" || choiceType == "function") {
			return nil, nil, fmt.Errorf("tool_choice function requires name")
		}
		if choiceType == "" {
			choiceType = "function"
		}
		mapped := map[string]any{"type": choiceType}
		if strings.TrimSpace(name) != "" {
			mapped["name"] = strings.TrimSpace(name)
		}
		body["tool_choice"] = mapped
	}
	if request.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	limit := request.MaxTokens
	if request.MaxCompletionTokens > 0 {
		limit = request.MaxCompletionTokens
	}
	if limit > 0 {
		body["max_output_tokens"] = limit
	}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	textField := map[string]any{}
	switch stringValue(request.ResponseFormat["type"]) {
	case "json_object":
		textField["format"] = map[string]any{"type": "json_object"}
	case "json_schema":
		schema, _ := request.ResponseFormat["json_schema"].(map[string]any)
		format := map[string]any{"type": "json_schema"}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, exists := schema[key]; exists {
				format[key] = value
			}
		}
		// Chat Completions defaults json_schema.strict to false, whereas
		// Responses defaults it to true; preserve the Chat default explicitly.
		if _, hasStrict := format["strict"]; !hasStrict {
			format["strict"] = false
		}
		textField["format"] = format
	}
	if verbosity := strings.TrimSpace(request.Verbosity); verbosity != "" {
		textField["verbosity"] = verbosity
	}
	if len(textField) > 0 {
		body["text"] = textField
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	if request.Stop != nil {
		body["stop"] = request.Stop
	}
	for key, value := range map[string]string{
		"user":                   request.User,
		"safety_identifier":      request.SafetyIdentifier,
		"service_tier":           request.ServiceTier,
		"prompt_cache_key":       request.PromptCacheKey,
		"prompt_cache_retention": request.PromptCacheRetention,
	} {
		if value != "" {
			body[key] = value
		}
	}
	if len(request.Metadata) > 0 {
		body["metadata"] = request.Metadata
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
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Responses request: %w", err)
	}
	return encoded, &request, nil
}

// chatFrontContent converts Chat message content into Responses content parts.
func chatFrontContent(value any, textType string) ([]any, error) {
	switch content := value.(type) {
	case nil:
		return nil, nil
	case string:
		if content == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": textType, "text": content}}, nil
	case []any:
		parts := make([]any, 0, len(content))
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid Chat Completions content part")
			}
			switch part["type"] {
			case "text", "input_text", "output_text":
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
			case "refusal":
				if text, _ := part["refusal"].(string); text != "" {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
			case "image_url":
				url, _ := part["image_url"].(string)
				detail, _ := part["detail"].(string)
				if nested, ok := part["image_url"].(map[string]any); ok {
					url, _ = nested["url"].(string)
					if detail == "" {
						detail, _ = nested["detail"].(string)
					}
				}
				imgPart := map[string]any{"type": "input_image", "image_url": url}
				if norm := normalizeImageDetail(detail); norm != "" {
					imgPart["detail"] = norm
				}
				parts = append(parts, imgPart)
			default:
				return nil, fmt.Errorf("unsupported Chat Completions content part %q", part["type"])
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("message content must be a string or an array of parts")
	}
}

// chatCompletionsFront serves Chat Completions through a Responses handler.
func chatCompletionsFront(key string, responses http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !inferenceRequestAllowed(writer, request, key) {
			return
		}
		raw, err := readInboundBody(writer, request, chatFrontMaxBodyBytes)
		if err != nil {
			writeInboundBodyError(writer, err)
			return
		}
		body, chat, err := chatToResponsesRequest(raw)
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		inner := request.Clone(request.Context())
		inner.Body = io.NopCloser(bytes.NewReader(body))
		inner.ContentLength = int64(len(body))
		inner.URL.Path = "/v1/responses"
		includeUsage, _ := chat.StreamOptions["include_usage"].(bool)
		translator := newChatFrontWriter(writer, chat.Model, includeUsage)
		responses(translator, inner)
		translator.finish()
	}
}

// chatFrontWriter receives what a Responses handler writes and renders it as
// Chat Completions: stream events become chunks as they arrive, and a JSON
// body is converted once the handler returns.
type chatFrontWriter struct {
	target        http.ResponseWriter
	header        http.Header
	status        int
	wroteHeader   bool
	streaming     bool
	model         string
	id            string
	created       int64
	includeUsage  bool
	pending       bytes.Buffer
	body          bytes.Buffer
	toolIndexes   map[int]int
	toolArgsSeen  map[int]bool
	textPartsSeen map[string]bool
	reasonSeen    map[string]bool
	sawToolCall   bool
	sentRole      bool
	done          bool
}

func newChatFrontWriter(target http.ResponseWriter, model string, includeUsage bool) *chatFrontWriter {
	return &chatFrontWriter{
		target: target, header: make(http.Header), status: http.StatusOK, model: model,
		id: "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""), created: time.Now().Unix(),
		includeUsage: includeUsage, toolIndexes: make(map[int]int), toolArgsSeen: make(map[int]bool),
		textPartsSeen: make(map[string]bool), reasonSeen: make(map[string]bool),
	}
}

func (w *chatFrontWriter) Header() http.Header { return w.header }

func (w *chatFrontWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	ct := strings.ToLower(strings.TrimSpace(w.header.Get("Content-Type")))
	w.streaming = status == http.StatusOK && strings.HasPrefix(ct, "text/event-stream")
	if !w.streaming {
		return
	}
	for key, values := range w.header {
		w.target.Header()[key] = values
	}
	w.target.WriteHeader(status)
}

func (w *chatFrontWriter) Flush() {
	if flusher, ok := w.target.(http.Flusher); ok && w.streaming {
		flusher.Flush()
	}
}

func (w *chatFrontWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.streaming {
		if int64(w.body.Len()+len(data)) > chatFrontMaxBodyBytes {
			return 0, fmt.Errorf("response exceeds the GBX Chat Completions limit")
		}
		return w.body.Write(data)
	}
	if int64(w.pending.Len()+len(data)) > chatFrontMaxBodyBytes {
		return 0, fmt.Errorf("response exceeds the GBX Chat Completions limit")
	}
	w.pending.Write(data)
	for {
		buffered := w.pending.Bytes()
		end, sepLen := nextSSEFrameBoundary(buffered)
		if end < 0 {
			break
		}
		event := append([]byte(nil), buffered[:end]...)
		w.pending.Next(end + sepLen)
		if err := w.streamEvent(event); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (w *chatFrontWriter) chunk(delta map[string]any, finish any, usage map[string]any) error {
	payload := map[string]any{
		"id": w.id, "object": "chat.completion.chunk", "created": w.created, "model": w.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if delta == nil {
		payload["choices"] = []any{}
	}
	if usage != nil {
		payload["usage"] = usage
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w.target, "data: %s\n\n", raw); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func (w *chatFrontWriter) ensureRole() error {
	if w.sentRole {
		return nil
	}
	w.sentRole = true
	return w.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func (w *chatFrontWriter) partKey(outIdx int, kind string, partIdx int) string {
	return fmt.Sprintf("%d:%s:%d", outIdx, kind, partIdx)
}

func (w *chatFrontWriter) emitFallbackItem(outIdx int, item map[string]any) error {
	if item == nil {
		return nil
	}
	switch stringValue(item["type"]) {
	case "message":
		for pIdx, rawPart := range sliceValue(item["content"]) {
			part := mapValue(rawPart)
			if part == nil {
				continue
			}
			switch stringValue(part["type"]) {
			case "output_text":
				key := w.partKey(outIdx, "text", pIdx)
				if w.textPartsSeen[key] {
					continue
				}
				if text := stringValue(part["text"]); text != "" {
					w.textPartsSeen[key] = true
					if err := w.ensureRole(); err != nil {
						return err
					}
					if err := w.chunk(map[string]any{"content": text}, nil, nil); err != nil {
						return err
					}
				}
			case "refusal":
				key := w.partKey(outIdx, "refusal", pIdx)
				if w.textPartsSeen[key] {
					continue
				}
				if refusal := stringValue(part["refusal"]); refusal != "" {
					w.textPartsSeen[key] = true
					if err := w.ensureRole(); err != nil {
						return err
					}
					if err := w.chunk(map[string]any{"refusal": refusal}, nil, nil); err != nil {
						return err
					}
				}
			}
		}
	case "reasoning":
		for pIdx, rawPart := range sliceValue(item["summary"]) {
			part := mapValue(rawPart)
			key := w.partKey(outIdx, "summary", pIdx)
			if w.reasonSeen[key] {
				continue
			}
			if text := stringValue(part["text"]); text != "" {
				w.reasonSeen[key] = true
				if err := w.ensureRole(); err != nil {
					return err
				}
				if err := w.chunk(map[string]any{"reasoning_content": text}, nil, nil); err != nil {
					return err
				}
			}
		}
		for pIdx, rawPart := range sliceValue(item["content"]) {
			part := mapValue(rawPart)
			key := w.partKey(outIdx, "rcontent", pIdx)
			if w.reasonSeen[key] {
				continue
			}
			if text := stringValue(part["text"]); text != "" {
				w.reasonSeen[key] = true
				if err := w.ensureRole(); err != nil {
					return err
				}
				if err := w.chunk(map[string]any{"reasoning_content": text}, nil, nil); err != nil {
					return err
				}
			}
		}
	case "function_call", "custom_tool_call":
		itemType := stringValue(item["type"])
		index, exists := w.toolIndexes[outIdx]
		if !exists {
			if err := w.ensureRole(); err != nil {
				return err
			}
			index = len(w.toolIndexes)
			w.toolIndexes[outIdx] = index
			w.sawToolCall = true
			args := ""
			if itemType == "function_call" {
				args = stringValue(item["arguments"])
			} else {
				encoded, _ := json.Marshal(map[string]any{"input": stringValue(item["input"])})
				args = string(encoded)
			}
			if args != "" {
				w.toolArgsSeen[outIdx] = true
			}
			return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": index, "id": item["call_id"], "type": "function",
				"function": map[string]any{"name": item["name"], "arguments": args},
			}}}, nil, nil)
		}
		if w.toolArgsSeen[outIdx] {
			return nil
		}
		args := ""
		if itemType == "function_call" {
			args = stringValue(item["arguments"])
		} else if inputVal, hasInput := item["input"]; hasInput {
			encoded, _ := json.Marshal(map[string]any{"input": stringValue(inputVal)})
			args = string(encoded)
		}
		if args == "" {
			return nil
		}
		w.toolArgsSeen[outIdx] = true
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": args},
		}}}, nil, nil)
	}
	return nil
}

func (w *chatFrontWriter) emitStreamError(errType, code, message string) error {
	if message == "" {
		message = "the provider reported a stream error"
	}
	if errType == "" {
		errType = "api_error"
	}
	var codeVal any
	if code != "" {
		codeVal = code
	}
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": truncateErrorMessage(message, maxClientErrorMessageBytes),
		"type":    errType,
		"code":    codeVal,
		"param":   nil,
	}})
	if _, err := fmt.Fprintf(w.target, "data: %s\n\n", raw); err != nil {
		return err
	}
	return w.end()
}

func (w *chatFrontWriter) streamEvent(raw []byte) error {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if len(data) == 0 || w.done {
		return nil
	}
	var event map[string]any
	if decodeProtocolJSON(data, &event) != nil {
		return nil
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "response.created":
		return w.ensureRole()
	case "response.output_text.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		w.textPartsSeen[w.partKey(chatFrontInt(event["output_index"]), "text", chatFrontInt(event["content_index"]))] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"content": delta}, nil, nil)
	case "response.output_text.done":
		key := w.partKey(chatFrontInt(event["output_index"]), "text", chatFrontInt(event["content_index"]))
		text := stringValue(event["text"])
		if text == "" || w.textPartsSeen[key] {
			return nil
		}
		w.textPartsSeen[key] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"content": text}, nil, nil)
	case "response.refusal.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		w.textPartsSeen[w.partKey(chatFrontInt(event["output_index"]), "refusal", chatFrontInt(event["content_index"]))] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"refusal": delta}, nil, nil)
	case "response.refusal.done":
		key := w.partKey(chatFrontInt(event["output_index"]), "refusal", chatFrontInt(event["content_index"]))
		refusal := stringValue(event["refusal"])
		if refusal == "" || w.textPartsSeen[key] {
			return nil
		}
		w.textPartsSeen[key] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"refusal": refusal}, nil, nil)
	case "response.content_part.done":
		part := mapValue(event["part"])
		if part == nil {
			return nil
		}
		outIdx := chatFrontInt(event["output_index"])
		pIdx := chatFrontInt(event["content_index"])
		switch stringValue(part["type"]) {
		case "output_text":
			key := w.partKey(outIdx, "text", pIdx)
			if text := stringValue(part["text"]); text != "" && !w.textPartsSeen[key] {
				w.textPartsSeen[key] = true
				if err := w.ensureRole(); err != nil {
					return err
				}
				return w.chunk(map[string]any{"content": text}, nil, nil)
			}
		case "refusal":
			key := w.partKey(outIdx, "refusal", pIdx)
			if refusal := stringValue(part["refusal"]); refusal != "" && !w.textPartsSeen[key] {
				w.textPartsSeen[key] = true
				if err := w.ensureRole(); err != nil {
					return err
				}
				return w.chunk(map[string]any{"refusal": refusal}, nil, nil)
			}
		}
	case "response.reasoning_summary_text.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		w.reasonSeen[w.partKey(chatFrontInt(event["output_index"]), "summary", chatFrontInt(event["summary_index"]))] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"reasoning_content": delta}, nil, nil)
	case "response.reasoning_summary_text.done":
		key := w.partKey(chatFrontInt(event["output_index"]), "summary", chatFrontInt(event["summary_index"]))
		text := stringValue(event["text"])
		if text == "" || w.reasonSeen[key] {
			return nil
		}
		w.reasonSeen[key] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"reasoning_content": text}, nil, nil)
	case "response.reasoning_text.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		w.reasonSeen[w.partKey(chatFrontInt(event["output_index"]), "rcontent", chatFrontInt(event["content_index"]))] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"reasoning_content": delta}, nil, nil)
	case "response.reasoning_text.done":
		key := w.partKey(chatFrontInt(event["output_index"]), "rcontent", chatFrontInt(event["content_index"]))
		text := stringValue(event["text"])
		if text == "" || w.reasonSeen[key] {
			return nil
		}
		w.reasonSeen[key] = true
		if err := w.ensureRole(); err != nil {
			return err
		}
		return w.chunk(map[string]any{"reasoning_content": text}, nil, nil)
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
			return nil
		}
		if err := w.ensureRole(); err != nil {
			return err
		}
		outIdx := chatFrontInt(event["output_index"])
		index := len(w.toolIndexes)
		w.toolIndexes[outIdx] = index
		w.sawToolCall = true
		initialArgs := ""
		if item["type"] == "function_call" {
			if args := stringValue(item["arguments"]); args != "" {
				initialArgs = args
				w.toolArgsSeen[outIdx] = true
			}
		}
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": item["call_id"], "type": "function",
			"function": map[string]any{"name": item["name"], "arguments": initialArgs},
		}}}, nil, nil)
	case "response.function_call_arguments.delta":
		outIdx := chatFrontInt(event["output_index"])
		index, ok := w.toolIndexes[outIdx]
		if !ok {
			return nil
		}
		if delta := stringValue(event["delta"]); delta != "" {
			w.toolArgsSeen[outIdx] = true
		}
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": event["delta"]},
		}}}, nil, nil)
	case "response.function_call_arguments.done":
		outIdx := chatFrontInt(event["output_index"])
		index, ok := w.toolIndexes[outIdx]
		if !ok || w.toolArgsSeen[outIdx] {
			return nil
		}
		args := stringValue(event["arguments"])
		if args == "" {
			return nil
		}
		w.toolArgsSeen[outIdx] = true
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": args},
		}}}, nil, nil)
	case "response.custom_tool_call_input.done":
		outIdx := chatFrontInt(event["output_index"])
		index, ok := w.toolIndexes[outIdx]
		if !ok {
			return nil
		}
		w.toolArgsSeen[outIdx] = true
		encoded, _ := json.Marshal(map[string]any{"input": stringValue(event["input"])})
		return w.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": string(encoded)},
		}}}, nil, nil)
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		return w.emitFallbackItem(chatFrontInt(event["output_index"]), item)
	case "response.completed", "response.incomplete":
		response, _ := event["response"].(map[string]any)
		for idx, rawItem := range sliceValue(response["output"]) {
			if err := w.emitFallbackItem(idx, mapValue(rawItem)); err != nil {
				return err
			}
		}
		if err := w.ensureRole(); err != nil {
			return err
		}
		if err := w.chunk(map[string]any{}, chatFrontFinishReason(response, w.sawToolCall), nil); err != nil {
			return err
		}
		if w.includeUsage {
			if err := w.chunk(nil, nil, chatFrontUsage(response["usage"])); err != nil {
				return err
			}
		}
		return w.end()
	case "response.failed", "error":
		var failure map[string]any
		if response, ok := event["response"].(map[string]any); ok {
			failure, _ = response["error"].(map[string]any)
		}
		if failure == nil {
			failure, _ = event["error"].(map[string]any)
		}
		if failure == nil {
			// Flat OpenAI Responses SSE error: {"type":"error","code":"...","message":"..."}
			failure = event
		}
		message := stringValue(failure["message"])
		code := stringValue(failure["code"])
		errType := stringValue(failure["type"])
		if errType == "" || errType == "error" {
			errType = firstNonEmptyString(code, "api_error")
		}
		return w.emitStreamError(errType, code, message)
	}
	return nil
}

func (w *chatFrontWriter) end() error {
	if w.done {
		return nil
	}
	w.done = true
	_, err := io.WriteString(w.target, "data: [DONE]\n\n")
	w.Flush()
	return err
}

// finish converts a buffered JSON body, or closes a stream the handler left
// without a terminal event.
func (w *chatFrontWriter) finish() {
	if w.streaming {
		if w.pending.Len() > 0 && !w.done {
			_ = w.streamEvent(w.pending.Bytes())
			w.pending.Reset()
		}
		if !w.done {
			_ = w.emitStreamError("api_error", "upstream_stream_error", "upstream stream ended before a terminal response")
		}
		return
	}
	for key, values := range w.header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		w.target.Header()[key] = values
	}
	body := w.body.Bytes()
	if w.status == http.StatusOK {
		var response map[string]any
		if decodeProtocolJSON(body, &response) == nil && response["object"] == "response" {
			if strings.EqualFold(stringValue(response["status"]), "failed") {
				envErr := detectJSONErrorEnvelope(body, "openai_responses")
				if envErr != nil {
					writeProxyErrorDetail(w.target, envErr.status, envErr.errType, envErr.code, envErr.param, envErr.Error())
					return
				}
				writeProxyErrorDetail(w.target, http.StatusBadGateway, "api_error", "server_error", "", "upstream Responses request failed")
				return
			}
			if converted, err := json.Marshal(chatFrontCompletion(response, w.id, w.created, w.model)); err == nil {
				body = converted
			}
		}
	}
	w.target.WriteHeader(w.status)
	_, _ = w.target.Write(body)
}

func chatFrontInt(value any) int {
	switch number := value.(type) {
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}

func chatFrontFinishReason(response map[string]any, sawToolCall bool) string {
	if response["status"] == "incomplete" {
		details, _ := response["incomplete_details"].(map[string]any)
		if details["reason"] == "content_filter" {
			return "content_filter"
		}
		return "length"
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}

func chatFrontUsage(value any) map[string]any {
	usage, _ := value.(map[string]any)
	input := chatFrontInt(usage["input_tokens"])
	output := chatFrontInt(usage["output_tokens"])
	result := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": chatFrontInt(details["cached_tokens"])}
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": chatFrontInt(details["reasoning_tokens"])}
	}
	return result
}

// chatFrontCompletion renders a finished Responses object as a chat.completion.
func chatFrontCompletion(response map[string]any, id string, created int64, model string) map[string]any {
	var text, refusal, reasoning strings.Builder
	var calls []any
	appendReasoning := func(s string) {
		if s == "" {
			return
		}
		if reasoning.Len() > 0 {
			reasoning.WriteString("\n\n")
		}
		reasoning.WriteString(s)
	}
	output, _ := response["output"].([]any)
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if value, _ := part["text"].(string); value != "" {
					text.WriteString(value)
				} else if value, _ := part["refusal"].(string); value != "" {
					refusal.WriteString(value)
				}
			}
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, rawPart := range summary {
				part, _ := rawPart.(map[string]any)
				appendReasoning(stringValue(part["text"]))
			}
			if len(summary) == 0 {
				content, _ := item["content"].([]any)
				for _, rawPart := range content {
					part, _ := rawPart.(map[string]any)
					appendReasoning(stringValue(part["text"]))
				}
			}
		case "function_call":
			calls = append(calls, map[string]any{
				"id": item["call_id"], "type": "function",
				"function": map[string]any{"name": item["name"], "arguments": item["arguments"]},
			})
		case "custom_tool_call":
			encoded, _ := json.Marshal(map[string]any{"input": stringValue(item["input"])})
			calls = append(calls, map[string]any{
				"id": item["call_id"], "type": "function",
				"function": map[string]any{"name": item["name"], "arguments": string(encoded)},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if refusal.Len() > 0 {
		message["refusal"] = refusal.String()
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": chatFrontFinishReason(response, len(calls) > 0),
		}},
		"usage": chatFrontUsage(response["usage"]),
	}
}
