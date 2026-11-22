package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// chatToolCall accumulates one streamed OpenAI tool_call across its argument
// deltas, mirroring codexFunctionCall. Arguments arrive as partial JSON
// fragments and are joined before emission.
type chatToolCall struct {
	index     int
	order     int
	id        string
	name      string
	arguments strings.Builder
	emitted   bool
}

type chatCompletionsStreamState struct {
	assembler    *responseAssembler
	tools        map[string]*chatToolCall
	activeSlot   map[int]string
	nextSlot     int
	usage        map[string]any
	retained     int
	terminalSeen bool
}

// processChatCompletionsStream converts an OpenAI Chat Completions SSE stream
// into OpenAI Responses SSE, feeding the shared assembler.
func processChatCompletionsStream(reader io.Reader, assembler *responseAssembler) error {
	state := &chatCompletionsStreamState{
		assembler: assembler,
		tools:     make(map[string]*chatToolCall),
	}
	if err := readSSEPayload(reader, "openai_chat", state.process); err != nil {
		return err
	}
	if !state.terminalSeen {
		return fmt.Errorf("OpenAI Chat stream ended before finish_reason: %w", io.ErrUnexpectedEOF)
	}
	if err := state.emitTools(); err != nil {
		return err
	}
	// The upstream usage is authoritative and arrives in the final chunk, so it
	// must be applied after tool emission (which still counts tokens locally).
	chatApplyUsage(assembler, state.usage)
	return assembler.finish()
}

// readChatCompletionsSSE reads OpenAI `data:` lines and hands each payload to
// consume. `[DONE]` and blank payloads are skipped.
func readChatCompletionsSSE(reader io.Reader, consume func([]byte) error) error {
	return readSSEPayload(reader, "openai_chat", consume)
}

// readSSEPayload reads standard SSE `data:` frames and also captures non-SSE
// HTTP 200 JSON error envelopes (common on OpenRouter/OneAPI/WorkBuddy) so
// pre-stream gates can surface the real status and error.code instead of EOF.
func readSSEPayload(reader io.Reader, dialect string, consume func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), codexResponsesMaxSSEEventBytes)
	var data strings.Builder
	var plain strings.Builder
	sawData := false
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		return consume([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			sawData = true
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > codexResponsesMaxSSEEventBytes {
				return fmt.Errorf("OpenAI Chat Completions SSE event exceeds %d bytes", codexResponsesMaxSSEEventBytes)
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		} else if !sawData && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") &&
			!strings.HasPrefix(line, "retry:") && !strings.HasPrefix(line, ":") {
			if int64(plain.Len()+len(line)+1) <= chatMaxErrorBytes {
				plain.WriteString(line)
				plain.WriteByte('\n')
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read OpenAI Chat Completions event stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if !sawData {
		if trimmed := strings.TrimSpace(plain.String()); trimmed != "" {
			if envErr := detectJSONErrorEnvelope([]byte(trimmed), dialect); envErr != nil {
				return envErr
			}
		}
	}
	return nil
}

func (s *chatCompletionsStreamState) process(payload []byte) error {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode OpenAI Chat Completions chunk: %w", err)
	}
	if upstreamError := event["error"]; upstreamError != nil {
		return buildStreamUpstreamError("openai_chat", string(payload), "")
	}
	if usage := mapValue(event["usage"]); len(usage) > 0 {
		s.usage = chatMergeUsage(s.usage, usage)
	}
	choices := sliceValue(event["choices"])
	if len(choices) == 0 {
		// The final chunk can carry only usage when stream_options.include_usage
		// is set, with an empty choices array.
		return nil
	}
	choice := mapValue(choices[0])
	if delta := mapValue(choice["delta"]); delta != nil {
		// Reasoning precedes assistant content, matching Responses item order.
		if reasoning := chatExtractReasoningText(delta); reasoning != "" {
			s.assembler.nativeReasoning = true
			if err := s.assembler.addThinking(reasoning); err != nil {
				return err
			}
		}
		if refusal := stringValue(delta["refusal"]); refusal != "" {
			if s.assembler.stopReason == "" {
				s.assembler.stopReason = stopRefusal
			}
			if err := s.assembler.addRefusal(refusal); err != nil {
				return err
			}
		}
		if text := stringValue(delta["content"]); text != "" {
			if err := s.assembler.addText(text); err != nil {
				return err
			}
		}
		for _, rawCall := range sliceValue(delta["tool_calls"]) {
			if err := s.accumulateToolCall(mapValue(rawCall)); err != nil {
				return err
			}
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish != "" {
		s.terminalSeen = true
		if finish != "stop" || s.assembler.stopReason != stopRefusal {
			s.assembler.stopReason = chatStopReason(finish)
		}
	}
	return nil
}

// chatExtractReasoningText reads reasoning text from OpenAI-compatible Chat
// delta/message objects across provider dialects without double-counting when
// gateways like OpenRouter emit both `reasoning` and `reasoning_details`.
func chatExtractReasoningText(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if text := stringValue(obj["reasoning_content"]); text != "" {
		return text
	}
	if text := stringValue(obj["reasoning"]); text != "" {
		return text
	}
	if details := sliceValue(obj["reasoning_details"]); len(details) > 0 {
		var out strings.Builder
		for _, raw := range details {
			part := mapValue(raw)
			if part == nil {
				continue
			}
			switch stringValue(part["type"]) {
			case "reasoning.text":
				out.WriteString(stringValue(part["text"]))
			case "reasoning.summary":
				out.WriteString(stringValue(part["summary"]))
			default:
				if text := stringValue(part["text"]); text != "" {
					out.WriteString(text)
				} else if summary := stringValue(part["summary"]); summary != "" {
					out.WriteString(summary)
				}
			}
		}
		return out.String()
	}
	return ""
}

// chatMergeUsage merges multi-frame Chat usage chunks so a completion-only or
// partial final chunk does not erase earlier prompt_tokens or cache details.
func chatMergeUsage(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for key, val := range src {
		if val == nil {
			continue
		}
		if nestedSrc, ok := val.(map[string]any); ok {
			if len(nestedSrc) == 0 {
				continue
			}
			existing, _ := dst[key].(map[string]any)
			merged := make(map[string]any, len(existing)+len(nestedSrc))
			for nk, nv := range existing {
				merged[nk] = nv
			}
			for nk, nv := range nestedSrc {
				if nv != nil {
					merged[nk] = nv
				}
			}
			dst[key] = merged
			continue
		}
		if key == "prompt_tokens" && intValue(val) == 0 && intValue(dst[key]) > 0 {
			continue
		}
		dst[key] = val
	}
	return dst
}

func (s *chatCompletionsStreamState) accumulateToolCall(raw map[string]any) error {
	if s.tools == nil {
		s.tools = make(map[string]*chatToolCall)
	}
	if s.activeSlot == nil {
		s.activeSlot = make(map[int]string)
	}
	index := intValue(raw["index"])
	id := stringValue(raw["id"])
	key := s.activeSlot[index]
	if key == "" {
		key = fmt.Sprintf("%d", index)
	}
	call := s.tools[key]
	// Some OpenAI-compatible gateways omit index (defaulting every parallel call
	// to 0) and distinguish concurrent calls only by id. Open a new slot instead
	// of concatenating two distinct calls into one corrupted entry.
	if call != nil && id != "" && call.id != "" && call.id != id {
		s.nextSlot++
		key = fmt.Sprintf("%d#%d", index, s.nextSlot)
		call = nil
	}
	if call == nil {
		if len(s.tools) >= assemblerMaxToolCalls {
			return fmt.Errorf("OpenAI Chat tool call stream exceeds %d concurrent tool calls", assemblerMaxToolCalls)
		}
		call = &chatToolCall{index: index, order: s.nextSlot}
		s.tools[key] = call
		s.activeSlot[index] = key
	}
	if id != "" {
		call.id = id
	}
	if function := mapValue(raw["function"]); function != nil {
		if name := stringValue(function["name"]); name != "" {
			merged, err := chatToolNameDelta(call.name, name, s.assembler.request.declaredToolNames)
			if err != nil {
				return err
			}
			if added := len(merged) - len(call.name); added > 0 {
				if s.retained+added > assemblerMaxRetainedBytes {
					return fmt.Errorf("OpenAI Chat tool call names exceed %d bytes", assemblerMaxRetainedBytes)
				}
				s.retained += added
			}
			call.name = merged
		}
		if arguments := stringValue(function["arguments"]); arguments != "" {
			if s.retained+len(arguments) > assemblerMaxRetainedBytes {
				return fmt.Errorf("OpenAI Chat tool call arguments exceed %d bytes", assemblerMaxRetainedBytes)
			}
			s.retained += len(arguments)
			call.arguments.WriteString(arguments)
		}
	}
	return nil
}

// Some gateways stream name fragments, others repeat the complete name. Only
// assemble exact prefixes of declared names; never strip reasoning markers or
// guess a nearby function. Ambiguous interpretations fail before tool emission.
func chatToolNameDelta(current, delta string, declared map[string]struct{}) (string, error) {
	maxBytes := 128
	if declared != nil {
		maxBytes = 0
		for name := range declared {
			maxBytes = max(maxBytes, len(name))
		}
	}
	validPrefix := func(name string) bool {
		if len(name) == 0 || len(name) > maxBytes {
			return false
		}
		if declared == nil {
			return true
		}
		for candidate := range declared {
			if strings.HasPrefix(candidate, name) {
				return true
			}
		}
		return false
	}
	var candidates []string
	if len(current)+len(delta) <= maxBytes && validPrefix(current+delta) {
		candidates = append(candidates, current+delta)
	}
	if current != "" && strings.HasPrefix(delta, current) && validPrefix(delta) {
		if len(candidates) == 0 || candidates[0] != delta {
			candidates = append(candidates, delta)
		}
	}
	if declared == nil && current == delta {
		return current, nil
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("OpenAI Chat provider returned a malformed, ambiguous or undeclared tool name (payload omitted)")
	}
	return candidates[0], nil
}

func chatValidateToolName(name string, declared map[string]struct{}) error {
	if declared != nil {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("OpenAI Chat provider returned a malformed or undeclared tool name (payload omitted)")
		}
	}
	return nil
}

// emitTools flushes accumulated calls in index order. With a request-bound
// catalog, every name must be a complete declaration before any call is emitted.
func (s *chatCompletionsStreamState) emitTools() error {
	type indexedCall struct {
		key  string
		call *chatToolCall
	}
	ordered := make([]indexedCall, 0, len(s.tools))
	for key, call := range s.tools {
		if call != nil {
			ordered = append(ordered, indexedCall{key: key, call: call})
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].call.index != ordered[j].call.index {
			return ordered[i].call.index < ordered[j].call.index
		}
		if ordered[i].call.order != ordered[j].call.order {
			return ordered[i].call.order < ordered[j].call.order
		}
		return ordered[i].key < ordered[j].key
	})
	truncated := s.assembler.isTruncatedStop()
	if !truncated {
		// Validate the whole batch before exposing even its first function call.
		for _, entry := range ordered {
			if err := chatValidateToolName(entry.call.name, s.assembler.request.declaredToolNames); err != nil {
				return err
			}
		}
	}
	for _, entry := range ordered {
		call := entry.call
		if call == nil || call.emitted {
			continue
		}
		call.emitted = true
		id := call.id
		if strings.TrimSpace(id) == "" {
			id = "call_" + strings.ReplaceAll(uuidString(), "-", "")
		}
		name := call.name
		if strings.TrimSpace(name) == "" {
			if truncated {
				continue
			}
			name = fmt.Sprintf("tool_%d", call.index)
		}
		arguments := call.arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if truncated {
			if err := chatValidateToolName(name, s.assembler.request.declaredToolNames); err != nil {
				continue
			}
			if _, _, _, err := s.assembler.validateToolUse(name, arguments); err != nil {
				continue
			}
		}
		if err := s.assembler.addToolUse(id, name, arguments); err != nil {
			return err
		}
	}
	return nil
}

// processChatCompletionsNonStream converts a complete OpenAI Chat Completions
// JSON body into an OpenAI Responses response via the shared assembler.
func processChatCompletionsNonStream(body []byte, assembler *responseAssembler) error {
	if envErr := detectJSONErrorEnvelope(body, "openai_chat"); envErr != nil {
		return envErr
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode OpenAI Chat Completions response: %w", err)
	}
	choices := sliceValue(payload["choices"])
	if len(choices) == 0 {
		return fmt.Errorf("OpenAI Chat Completions response has no choices")
	}
	choice := mapValue(choices[0])
	message := mapValue(choice["message"])
	// reasoning_content precedes content, matching Responses item order.
	if reasoning := chatExtractReasoningText(message); reasoning != "" {
		assembler.nativeReasoning = true
		if err := assembler.addThinking(reasoning); err != nil {
			return err
		}
	}
	if err := chatAddContent(assembler, message["content"]); err != nil {
		return err
	}
	if refusal := stringValue(message["refusal"]); refusal != "" {
		if assembler.stopReason == "" {
			assembler.stopReason = stopRefusal
		}
		if err := assembler.addRefusal(refusal); err != nil {
			return err
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish != "" {
		if finish != "stop" || assembler.stopReason != stopRefusal {
			assembler.stopReason = chatStopReason(finish)
		}
	}
	truncated := assembler.isTruncatedStop()
	rawCalls := sliceValue(message["tool_calls"])
	if !truncated {
		// Validate the whole batch before exposing even its first function call.
		for _, rawCall := range rawCalls {
			call := mapValue(rawCall)
			function := mapValue(call["function"])
			if err := chatValidateToolName(stringValue(function["name"]), assembler.request.declaredToolNames); err != nil {
				return err
			}
		}
	}
	for index, rawCall := range rawCalls {
		call := mapValue(rawCall)
		function := mapValue(call["function"])
		name := stringValue(function["name"])
		if strings.TrimSpace(name) == "" {
			if truncated {
				continue
			}
			name = fmt.Sprintf("tool_%d", index)
		}
		id := stringValue(call["id"])
		if strings.TrimSpace(id) == "" {
			id = "call_" + strings.ReplaceAll(uuidString(), "-", "")
		}
		arguments := stringValue(function["arguments"])
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if truncated {
			if err := chatValidateToolName(name, assembler.request.declaredToolNames); err != nil {
				continue
			}
			if _, _, _, err := assembler.validateToolUse(name, arguments); err != nil {
				continue
			}
		}
		if err := assembler.addToolUse(id, name, arguments); err != nil {
			return err
		}
	}
	chatApplyUsage(assembler, mapValue(payload["usage"]))
	return assembler.finish()
}

// chatAddContent feeds an OpenAI assistant content field, which is a plain
// string for text-only responses or an array of text/refusal parts for
// structured ones.
func chatAddContent(assembler *responseAssembler, content any) error {
	switch typed := content.(type) {
	case string:
		if typed != "" {
			return assembler.addText(typed)
		}
	case []any:
		for _, part := range typed {
			block := mapValue(part)
			if block == nil {
				continue
			}
			if refusal := stringValue(block["refusal"]); refusal != "" {
				if assembler.stopReason == "" {
					assembler.stopReason = stopRefusal
				}
				if err := assembler.addRefusal(refusal); err != nil {
					return err
				}
				continue
			}
			if text := stringValue(block["text"]); text != "" {
				if err := assembler.addText(text); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// chatApplyUsage records the OpenAI usage object onto the assembler, mapping
// prompt_tokens/completion_tokens, cached/written prompt tokens, and reasoning
// tokens to the assembler's usage fields.
func chatApplyUsage(assembler *responseAssembler, usage map[string]any) {
	if usage == nil {
		return
	}
	in := intValue(usage["prompt_tokens"])
	if value, ok := usage["completion_tokens"]; ok && value != nil {
		assembler.outputUsageKnown = true
		assembler.outputTokens = intValue(value)
	}
	cacheRead := 0
	cacheWrite := 0
	if details := mapValue(usage["prompt_tokens_details"]); details != nil {
		cacheRead = intValue(details["cached_tokens"])
		cacheWrite = intValue(details["cache_write_tokens"])
		if cacheWrite == 0 {
			cacheWrite = intValue(details["cache_creation_input_tokens"])
		}
	}
	if cacheRead == 0 {
		for _, key := range []string{"cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"} {
			if v := intValue(usage[key]); v > 0 {
				cacheRead = v
				break
			}
		}
	}
	if cacheWrite == 0 {
		for _, key := range []string{"cache_write_tokens", "cache_creation_input_tokens"} {
			if v := intValue(usage[key]); v > 0 {
				cacheWrite = v
				break
			}
		}
	}
	assembler.cacheReadTokens = cacheRead
	assembler.cacheWriteTokens = cacheWrite
	if details := mapValue(usage["completion_tokens_details"]); details != nil {
		if reasoning := intValue(details["reasoning_tokens"]); reasoning > 0 {
			assembler.reasoningTokens = reasoning
		}
	}
	// OpenAI prompt_tokens already includes cached/written tokens; report only
	// fresh input in contextTokens (see codex_responses_stream).
	if value, ok := usage["prompt_tokens"]; ok && value != nil {
		assembler.inputUsageKnown = true
		assembler.contextTokens = max(0, in-assembler.cacheReadTokens-assembler.cacheWriteTokens)
	}
}

// chatStopReason maps an OpenAI finish_reason onto the assembler stop_reason.
func chatStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length":
		return stopMaxTokens
	case "tool_calls", "function_call":
		return stopToolUse
	case "content_filter":
		return stopContentFilter
	default:
		return stopEndTurn
	}
}
