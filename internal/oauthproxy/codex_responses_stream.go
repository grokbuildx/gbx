package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const codexResponsesMaxSSEEventBytes = 16 << 20

type codexFunctionCall struct {
	callID    string
	name      string
	arguments strings.Builder
	emitted   bool
}

type codexResponsesStreamState struct {
	assembler          *responseAssembler
	functions          map[string]*codexFunctionCall
	functionCount      int
	retained           int
	textPartsSeen      map[string]bool
	annotationsSeen    map[string]bool
	outputAliases      map[string]string
	reasoningPartsSeen map[string]bool
	reasoningDone      map[string]bool
	terminalSeen       bool
	metrics            codexResponsesStreamMetrics
}

type codexResponsesStreamMetrics struct {
	events               int
	textDeltaEvents      int
	reasoningDeltaEvents int
	textBytes            int
	reasoningBytes       int
	firstEventAt         time.Time
	firstContentAt       time.Time
	terminalType         string
}

func processCodexResponsesStream(reader io.Reader, assembler *responseAssembler) error {
	_, err := processCodexResponsesStreamObserved(reader, assembler)
	return err
}

func processCodexResponsesStreamObserved(reader io.Reader, assembler *responseAssembler) (codexResponsesStreamMetrics, error) {
	state := &codexResponsesStreamState{
		assembler: assembler,
		functions: make(map[string]*codexFunctionCall),
	}
	if err := readCodexSSE(reader, state.process); err != nil {
		return state.metrics, err
	}
	if !state.terminalSeen {
		return state.metrics, fmt.Errorf("Responses stream ended before terminal event: %w", io.ErrUnexpectedEOF)
	}
	return state.metrics, nil
}

func readCodexSSE(reader io.Reader, consume func([]byte) error) error {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	// Detect non-SSE JSON bodies up front so single-line responses up to 64 MiB
	// bypass the 16 MiB SSE line scanner limit, and HTTP 200 JSON error/failed
	// payloads are never wrapped as response.completed.
	for {
		b, err := buffered.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Codex Responses event stream: %w", err)
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		_ = buffered.UnreadByte()
		if b == '{' {
			raw, readErr := io.ReadAll(io.LimitReader(buffered, chatMaxResponseBytes+1))
			if readErr != nil {
				return fmt.Errorf("read non-streaming Codex Responses payload: %w", readErr)
			}
			if int64(len(raw)) > chatMaxResponseBytes {
				return fmt.Errorf("Codex Responses non-streaming payload exceeds %d bytes", chatMaxResponseBytes)
			}
			return consumeNonStreamCodexJSON(raw, consume)
		}
		break
	}

	scanner := bufio.NewScanner(buffered)
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
				return fmt.Errorf("Codex Responses SSE event exceeds %d bytes", codexResponsesMaxSSEEventBytes)
			}
			data.WriteString(strings.TrimPrefix(value, " "))
		} else if !sawData && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") &&
			!strings.HasPrefix(line, "retry:") && !strings.HasPrefix(line, ":") {
			if int64(plain.Len()+len(line)+1) > chatMaxResponseBytes {
				return fmt.Errorf("Codex Responses non-streaming payload exceeds %d bytes", chatMaxResponseBytes)
			}
			plain.WriteString(line)
			plain.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex Responses event stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if sawData || strings.TrimSpace(plain.String()) == "" {
		return nil
	}
	return consumeNonStreamCodexJSON([]byte(strings.TrimSpace(plain.String())), consume)
}

func consumeNonStreamCodexJSON(raw []byte, consume func([]byte) error) error {
	if envErr := detectJSONErrorEnvelope(raw, "openai_responses"); envErr != nil {
		return envErr
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode non-streaming Codex Responses payload: %w", err)
	}
	eventType := stringValue(response["type"])
	if eventType == "" || (!strings.HasPrefix(eventType, "response.") && eventType != "error") {
		if response["error"] != nil {
			return buildStreamUpstreamError("openai_responses", string(raw), "")
		}
		status := strings.ToLower(strings.TrimSpace(stringValue(response["status"])))
		switch status {
		case "failed":
			response = map[string]any{"type": "response.failed", "response": response}
		case "incomplete":
			response = map[string]any{"type": "response.incomplete", "response": response}
		case "cancelled":
			return buildStreamUpstreamError("openai_responses", string(raw), "Codex Responses request was cancelled")
		default:
			response = map[string]any{"type": "response.completed", "response": response}
		}
	}
	payload, _ := json.Marshal(response)
	return consume(payload)
}

func (s *codexResponsesStreamState) process(payload []byte) error {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode Codex Responses event: %w", err)
	}
	if !s.assembler.started {
		if response := mapValue(event["response"]); response != nil {
			if id := stringValue(response["id"]); id != "" {
				s.assembler.responseID = id
			}
		}
	}
	eventType := stringValue(event["type"])
	now := time.Now()
	s.metrics.events++
	if s.metrics.firstEventAt.IsZero() {
		s.metrics.firstEventAt = now
	}
	switch eventType {
	case "response.output_text.delta", "response.refusal.delta":
		delta := stringValue(event["delta"])
		s.metrics.textDeltaEvents++
		s.metrics.textBytes += len(delta)
		if s.metrics.firstContentAt.IsZero() && delta != "" {
			s.metrics.firstContentAt = now
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := stringValue(event["delta"])
		s.metrics.reasoningDeltaEvents++
		s.metrics.reasoningBytes += len(delta)
		if s.metrics.firstContentAt.IsZero() && delta != "" {
			s.metrics.firstContentAt = now
		}
	case "response.completed", "response.incomplete", "response.failed", "error":
		s.metrics.terminalType = eventType
	}
	switch eventType {
	case "response.created", "response.in_progress":
		return nil
	case "response.reasoning_summary_part.added":
		return nil
	case "response.reasoning_summary_text.delta":
		return s.addReasoningPart(event, nil, "summary", intValue(event["summary_index"]), stringValue(event["delta"]), true)
	case "response.reasoning_summary_text.done":
		return s.addReasoningPart(event, nil, "summary", intValue(event["summary_index"]), stringValue(event["text"]), false)
	case "response.reasoning_summary_part.done":
		return s.addReasoningPart(event, nil, "summary", intValue(event["summary_index"]), stringValue(mapValue(event["part"])["text"]), false)
	case "response.reasoning_text.delta":
		return s.addReasoningPart(event, nil, "content", intValue(event["content_index"]), stringValue(event["delta"]), true)
	case "response.reasoning_text.done":
		return s.addReasoningPart(event, nil, "content", intValue(event["content_index"]), stringValue(event["text"]), false)
	case "response.output_text.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		s.markTextPart(event, nil, intValue(event["content_index"]))
		return s.assembler.addText(delta)
	case "response.output_text.done":
		text := stringValue(event["text"])
		if text == "" || s.markTextPart(event, nil, intValue(event["content_index"])) {
			return nil
		}
		return s.assembler.addText(text)
	case "response.refusal.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			return nil
		}
		s.markTextPart(event, nil, intValue(event["content_index"]))
		if s.assembler.stopReason == "" {
			s.assembler.stopReason = stopRefusal
		}
		return s.assembler.addRefusal(delta)
	case "response.refusal.done":
		refusal := stringValue(event["refusal"])
		if refusal == "" || s.markTextPart(event, nil, intValue(event["content_index"])) {
			return nil
		}
		if s.assembler.stopReason == "" {
			s.assembler.stopReason = stopRefusal
		}
		return s.assembler.addRefusal(refusal)
	case "response.output_text.annotation.added":
		if ann := mapValue(event["annotation"]); ann != nil {
			s.markAnnotationPart(event, nil, intValue(event["content_index"]))
			return s.assembler.addAnnotation(ann)
		}
		return nil
	case "response.content_part.done":
		part := mapValue(event["part"])
		if part == nil {
			return nil
		}
		kind := stringValue(part["type"])
		if kind == "output_text" {
			text := stringValue(part["text"])
			if text != "" && !s.markTextPart(event, nil, intValue(event["content_index"])) {
				if err := s.assembler.addText(text); err != nil {
					return err
				}
			}
			if anns := sliceValue(part["annotations"]); len(anns) > 0 && !s.markAnnotationPart(event, nil, intValue(event["content_index"])) {
				for _, rawAnn := range anns {
					if ann := mapValue(rawAnn); ann != nil {
						if err := s.assembler.addAnnotation(ann); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}
		if kind == "refusal" {
			refusal := stringValue(part["refusal"])
			if refusal == "" || s.markTextPart(event, nil, intValue(event["content_index"])) {
				return nil
			}
			if s.assembler.stopReason == "" {
				s.assembler.stopReason = stopRefusal
			}
			return s.assembler.addRefusal(refusal)
		}
	case "response.function_call_arguments.delta":
		call, err := s.resolveFunction(event, nil)
		if err != nil {
			return err
		}
		if delta := stringValue(event["delta"]); delta != "" {
			if err := s.retainArguments(len(delta)); err != nil {
				return err
			}
			call.arguments.WriteString(delta)
		}
	case "response.function_call_arguments.done":
		call, err := s.resolveFunction(event, nil)
		if err != nil {
			return err
		}
		if arguments := stringValue(event["arguments"]); arguments != "" {
			if err := s.retainArguments(len(arguments)); err != nil {
				return err
			}
			call.arguments.Reset()
			call.arguments.WriteString(arguments)
		}
	case "response.output_item.added":
		item := mapValue(event["item"])
		s.textItemKey(event, item)
		if stringValue(item["type"]) == "function_call" {
			if _, err := s.updateFunction(event, item); err != nil {
				return err
			}
		}
	case "response.output_item.done":
		return s.processOutputItem(event, false)
	case "response.completed", "response.incomplete", "response.failed":
		return s.processTerminal(eventType, mapValue(event["response"]))
	case "error":
		fallback := fmt.Sprintf("Codex Responses stream error: %s", codexEventErrorMessage(event))
		return buildStreamUpstreamError("openai_responses", string(payload), fallback)
	}
	return nil
}

func (s *codexResponsesStreamState) markAnnotationPart(event, item map[string]any, contentIndex int) bool {
	if s.annotationsSeen == nil {
		s.annotationsSeen = make(map[string]bool)
	}
	key := fmt.Sprintf("%s:%d", s.textItemKey(event, item), contentIndex)
	if s.annotationsSeen[key] {
		return true
	}
	s.annotationsSeen[key] = true
	return false
}

func (s *codexResponsesStreamState) processOutputItem(event map[string]any, terminal bool) error {
	item := mapValue(event["item"])
	switch stringValue(item["type"]) {
	case "message":
		for index, part := range sliceValue(item["content"]) {
			content := mapValue(part)
			kind := stringValue(content["type"])
			if kind != "output_text" && kind != "refusal" {
				continue
			}
			if kind == "refusal" {
				if s.markTextPart(event, item, index) {
					continue
				}
				refusal := stringValue(content["refusal"])
				if refusal != "" {
					if s.assembler.stopReason == "" {
						s.assembler.stopReason = stopRefusal
					}
					if err := s.assembler.addRefusal(refusal); err != nil {
						return err
					}
				}
				continue
			}
			if !s.markTextPart(event, item, index) {
				if text := stringValue(content["text"]); text != "" {
					if err := s.assembler.addText(text); err != nil {
						return err
					}
				}
			}
			if anns := sliceValue(content["annotations"]); len(anns) > 0 && !s.markAnnotationPart(event, item, index) {
				for _, rawAnn := range anns {
					if ann := mapValue(rawAnn); ann != nil {
						if err := s.assembler.addAnnotation(ann); err != nil {
							return err
						}
					}
				}
			}
		}
	case "reasoning":
		key := codexFunctionKey(event, item)
		if s.reasoningDone[key] {
			return nil
		}
		if s.reasoningDone == nil {
			s.reasoningDone = make(map[string]bool)
		}
		s.reasoningDone[key] = true
		scope := ""
		if s.assembler.request != nil {
			scope = s.assembler.request.reasoningScope
		}
		signature, err := codexReasoningSignatureScoped(item, scope)
		if err != nil {
			return err
		}
		if err := s.assembler.retain(len(signature)); err != nil {
			return err
		}
		hadText := false
		for index, raw := range sliceValue(item["summary"]) {
			text := stringValue(mapValue(raw)["text"])
			if text != "" {
				hadText = true
			}
			if err := s.addReasoningPart(event, item, "summary", index, text, false); err != nil {
				return err
			}
		}
		for index, raw := range sliceValue(item["content"]) {
			part := mapValue(raw)
			text := stringValue(part["text"])
			if text != "" {
				hadText = true
			}
			if err := s.addReasoningPart(event, item, "content", index, text, false); err != nil {
				return err
			}
		}
		if s.assembler.activeType == blockThinking && s.assembler.activeIndex >= 0 {
			s.assembler.blocks[s.assembler.activeIndex].Signature = signature
			return s.assembler.closeActive()
		}
		if hadText {
			// Text was already streamed and closed into an earlier thinking block
			// (for example when output_item.done was omitted and text followed).
			// Attach the signature to the most recent unsigned thinking block.
			for i := len(s.assembler.blocks) - 1; i >= 0; i-- {
				if s.assembler.blocks[i].Type == blockThinking && s.assembler.blocks[i].Signature == "" {
					s.assembler.blocks[i].Signature = signature
					return nil
				}
			}
		}
		// No visible summary/content text: emit an opaque reasoning item with
		// summary: [] instead of fabricating an empty summary_text part.
		if signature != "" {
			if n := len(signature); s.assembler.retainedBytes >= n {
				s.assembler.retainedBytes -= n
			}
			return s.assembler.addOpaqueReasoning(signature)
		}
		return nil
	case "function_call":
		call, err := s.updateFunction(event, item)
		if err != nil {
			return err
		}
		// Defer an incomplete function_call item until the terminal event tells
		// us whether max_output_tokens truncated its JSON arguments.
		if !terminal && stringValue(item["status"]) == "incomplete" {
			return nil
		}
		return s.emitFunction(call)
	}
	return nil
}

// Track each content part independently; terminal output repeats streamed
// parts but may also contain additional messages or unstreamed parts.
func (s *codexResponsesStreamState) textItemKey(event, item map[string]any) string {
	if s.outputAliases == nil {
		s.outputAliases = make(map[string]string)
	}
	id := stringValue(item["id"])
	if id == "" {
		id = stringValue(event["item_id"])
	}
	if _, ok := event["output_index"]; ok {
		key := fmt.Sprintf("index:%d", intValue(event["output_index"]))
		if id != "" {
			s.outputAliases[id] = key
		}
		return key
	}
	if key := s.outputAliases[id]; key != "" {
		return key
	}
	if id != "" {
		return "id:" + id
	}
	return "index:0"
}

func (s *codexResponsesStreamState) markTextPart(event, item map[string]any, index int) bool {
	return s.markSeenPart(&s.textPartsSeen, event, item, "text", index)
}

func (s *codexResponsesStreamState) addReasoningPart(event, item map[string]any, namespace string, index int, text string, delta bool) error {
	if text == "" {
		return nil
	}
	seen := s.markSeenPart(&s.reasoningPartsSeen, event, item, namespace, index)
	if seen && !delta {
		return nil
	}
	if !seen && s.assembler.activeType == blockThinking && s.assembler.buffers[s.assembler.activeIndex].thinking.Len() > 0 {
		if err := s.assembler.addThinking("\n\n"); err != nil {
			return err
		}
	}
	return s.assembler.addThinking(text)
}

func (s *codexResponsesStreamState) markSeenPart(parts *map[string]bool, event, item map[string]any, namespace string, index int) bool {
	if *parts == nil {
		*parts = make(map[string]bool)
	}
	key := fmt.Sprintf("%s/%s:%d", s.textItemKey(event, item), namespace, index)
	seen := (*parts)[key]
	id := stringValue(item["id"])
	if id == "" {
		id = stringValue(event["item_id"])
	}
	if id != "" {
		idKey := fmt.Sprintf("id:%s/%s:%d", id, namespace, index)
		seen = seen || (*parts)[idKey]
		(*parts)[idKey] = true
	}
	(*parts)[key] = true
	return seen
}

func (s *codexResponsesStreamState) processTerminal(eventType string, response map[string]any) error {
	if eventType == "response.failed" || strings.EqualFold(stringValue(response["status"]), "failed") || response["error"] != nil {
		rawErr, _ := json.Marshal(response)
		fallback := "Codex Responses failed"
		if message := codexEventErrorMessage(response); message != "" {
			fallback = fmt.Sprintf("Codex Responses failed: %s", message)
		}
		return buildStreamUpstreamError("openai_responses", string(rawErr), fallback)
	}
	// Resolve incomplete stop_reason BEFORE emitting terminal output items so
	// truncated function calls on max_output_tokens/content_filter are skipped cleanly.
	if eventType == "response.incomplete" || strings.EqualFold(stringValue(response["status"]), "incomplete") {
		details := mapValue(response["incomplete_details"])
		switch stringValue(details["reason"]) {
		case "content_filter":
			s.assembler.stopReason = stopContentFilter
		default:
			s.assembler.stopReason = stopMaxTokens
		}
	}
	// Some compatible gateways omit output_item.done. Recover any terminal text,
	// reasoning signature, or function call exactly once from response.output.
	for index, rawItem := range sliceValue(response["output"]) {
		item := mapValue(rawItem)
		event := map[string]any{"output_index": float64(index), "item": item}
		if err := s.processOutputItem(event, true); err != nil {
			return err
		}
	}
	usage := mapValue(response["usage"])
	in := intValue(usage["input_tokens"])
	if value, ok := usage["output_tokens"]; ok && value != nil {
		s.assembler.outputUsageKnown = true
		s.assembler.outputTokens = intValue(value)
	}
	if details := mapValue(usage["output_tokens_details"]); details != nil {
		if reasoning := intValue(details["reasoning_tokens"]); reasoning > 0 {
			s.assembler.reasoningTokens = reasoning
		}
	}
	if details := mapValue(usage["input_tokens_details"]); details != nil {
		s.assembler.cacheReadTokens = intValue(details["cached_tokens"])
		s.assembler.cacheWriteTokens = intValue(details["cache_write_tokens"])
	}
	if s.assembler.cacheWriteTokens == 0 {
		s.assembler.cacheWriteTokens = intValue(usage["cache_write_tokens"])
	}
	// Responses input_tokens already includes cached/written tokens; subtract
	// them so contextTokens holds only fresh input, matching qoder and keeping
	// cache tokens out of the fresh input total.
	if value, ok := usage["input_tokens"]; ok && value != nil {
		s.assembler.inputUsageKnown = true
		s.assembler.contextTokens = max(0, in-s.assembler.cacheReadTokens-s.assembler.cacheWriteTokens)
	}
	s.terminalSeen = true
	return s.assembler.finish()
}

func (s *codexResponsesStreamState) retainArguments(size int) error {
	if s.retained+size > assemblerMaxRetainedBytes {
		return fmt.Errorf("Codex Responses function call arguments exceed %d bytes", assemblerMaxRetainedBytes)
	}
	s.retained += size
	return nil
}

func (s *codexResponsesStreamState) resolveFunction(event, item map[string]any) (*codexFunctionCall, error) {
	keys := codexFunctionKeys(event, item)
	var call *codexFunctionCall
	for _, key := range keys {
		if existing := s.functions[key]; existing != nil {
			call = existing
			break
		}
	}
	if call == nil {
		if s.functionCount >= assemblerMaxToolCalls {
			return nil, fmt.Errorf("Codex Responses function call stream exceeds %d concurrent calls", assemblerMaxToolCalls)
		}
		call = &codexFunctionCall{}
		s.functionCount++
	}
	for _, key := range keys {
		s.functions[key] = call
	}
	return call, nil
}

func (s *codexResponsesStreamState) updateFunction(event, item map[string]any) (*codexFunctionCall, error) {
	call, err := s.resolveFunction(event, item)
	if err != nil {
		return nil, err
	}
	if value := stringValue(item["call_id"]); value != "" {
		call.callID = value
	}
	if value := stringValue(item["name"]); value != "" {
		call.name = value
	}
	if arguments := stringValue(item["arguments"]); arguments != "" {
		if err := s.retainArguments(len(arguments)); err != nil {
			return nil, err
		}
		call.arguments.Reset()
		call.arguments.WriteString(arguments)
	}
	return call, nil
}

func (s *codexResponsesStreamState) emitFunction(call *codexFunctionCall) error {
	if call == nil || call.emitted {
		return nil
	}
	call.emitted = true
	if call.callID == "" {
		call.callID = "call_" + strings.ReplaceAll(uuidString(), "-", "")
	}
	arguments := call.arguments.String()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	if s.assembler.isTruncatedStop() {
		if _, _, _, err := s.assembler.validateToolUse(call.name, arguments); err != nil {
			return nil
		}
	}
	return s.assembler.addToolUse(call.callID, call.name, arguments)
}

func codexFunctionKeys(event, item map[string]any) []string {
	keys := make([]string, 0, 4)
	add := func(key string) {
		for _, existing := range keys {
			if existing == key {
				return
			}
		}
		keys = append(keys, key)
	}
	if item != nil {
		if value := stringValue(item["id"]); value != "" {
			add("id:" + value)
		}
		if value := stringValue(item["call_id"]); value != "" {
			add("call_id:" + value)
		}
	}
	if value := stringValue(event["item_id"]); value != "" {
		add("id:" + value)
	}
	if _, ok := event["output_index"]; ok {
		add(fmt.Sprintf("index:%d", intValue(event["output_index"])))
	}
	if len(keys) == 0 {
		add("index:0")
	}
	return keys
}

func codexFunctionKey(event, item map[string]any) string {
	return codexFunctionKeys(event, item)[0]
}

func codexEventErrorMessage(value map[string]any) string {
	if value == nil {
		return ""
	}
	if message := stringValue(value["message"]); message != "" {
		return message
	}
	if nested := mapValue(value["error"]); nested != nil {
		if message := stringValue(nested["message"]); message != "" {
			return message
		}
	}
	return ""
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func sliceValue(value any) []any {
	result, _ := value.([]any)
	return result
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}
