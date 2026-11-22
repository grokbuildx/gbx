package oauthproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/grokbuildx/gbx/internal/protocol"
)

const anthropicMaxActiveBlocks = 256

// anthropicStreamBlock is one Messages content block under decoding.
type anthropicStreamBlock struct {
	kind      string
	id        string
	name      string
	input     strings.Builder
	signature strings.Builder
	thinking  bool // whether thinking text reached the assembler
}

type anthropicPendingTool struct {
	id    string
	name  string
	input string
}

type anthropicStreamState struct {
	assembler    *responseAssembler
	blocks       map[int]*anthropicStreamBlock
	pendingTools []anthropicPendingTool
	finalUsage   map[string]any
	terminalSeen bool
}

// processAnthropicMessagesStream converts an Anthropic Messages SSE stream
// into Responses events through the shared assembler.
func processAnthropicMessagesStream(reader io.Reader, assembler *responseAssembler) error {
	state := &anthropicStreamState{assembler: assembler, blocks: make(map[int]*anthropicStreamBlock)}
	// Messages SSE frames carry "event:" lines too, but every data payload
	// repeats its type, so only the data lines are needed.
	if err := readSSEPayload(reader, "anthropic", state.process); err != nil {
		return err
	}
	if !state.terminalSeen {
		return fmt.Errorf("Anthropic Messages stream ended before message_stop: %w", io.ErrUnexpectedEOF)
	}
	if err := state.flushPendingTools(assembler.isTruncatedStop()); err != nil {
		return err
	}
	// Apply authoritative upstream usage after deferred tool emission so local
	// tool-argument token estimates do not inflate reported output_tokens.
	anthropicApplyUsage(assembler, state.finalUsage, true)
	return assembler.finish()
}

func (s *anthropicStreamState) flushPendingTools(allowTruncated bool) error {
	if len(s.pendingTools) == 0 {
		return nil
	}
	pending := s.pendingTools
	s.pendingTools = nil
	if !allowTruncated {
		for _, tool := range pending {
			if err := s.assembler.validateToolUseName(tool.name); err != nil {
				return err
			}
		}
	}
	for _, tool := range pending {
		if allowTruncated {
			if _, _, _, err := s.assembler.validateToolUse(tool.name, tool.input); err != nil {
				continue
			}
		}
		if err := s.assembler.addToolUse(tool.id, tool.name, tool.input); err != nil {
			return err
		}
	}
	return nil
}

func (s *anthropicStreamState) process(payload []byte) error {
	var event struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		Message      json.RawMessage `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
		Usage        json.RawMessage `json:"usage"`
		Error        *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode Anthropic Messages event: %w", err)
	}
	a := s.assembler
	switch event.Type {
	case "message_start":
		// Defer assembler.start() until the first content block or finish() so
		// pre-content overloaded_error/rate_limit_error events still map to HTTP
		// error statuses instead of HTTP 200 SSE response.failed.
		var message struct {
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(event.Message, &message); err == nil {
			anthropicApplyUsage(a, message.Usage, false)
		}
	case "content_block_start":
		var block struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			Text      string `json:"text"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Data      string `json:"data"`
		}
		if err := json.Unmarshal(event.ContentBlock, &block); err != nil {
			return fmt.Errorf("decode Anthropic content block: %w", err)
		}
		if _, exists := s.blocks[event.Index]; !exists && len(s.blocks) >= anthropicMaxActiveBlocks {
			return fmt.Errorf("Anthropic Messages stream exceeds %d active content blocks", anthropicMaxActiveBlocks)
		}
		if block.Type != "tool_use" && len(s.pendingTools) > 0 {
			if err := s.flushPendingTools(false); err != nil {
				return err
			}
		}
		state := &anthropicStreamBlock{kind: block.Type, id: block.ID, name: block.Name}
		s.blocks[event.Index] = state
		switch block.Type {
		case "thinking":
			// Each thinking block carries its own signature: never merge two.
			if err := a.closeActive(); err != nil {
				return err
			}
			if block.Signature != "" {
				if err := a.retain(len(block.Signature)); err != nil {
					return err
				}
				state.signature.WriteString(block.Signature)
			}
			if block.Thinking != "" {
				state.thinking = true
				return a.addThinking(block.Thinking)
			}
		case "redacted_thinking":
			delete(s.blocks, event.Index)
			if strings.TrimSpace(block.Data) == "" {
				return nil
			}
			prefixed := anthropicRedactedPrefix + block.Data
			if a.request != nil && a.request.reasoningScope != "" {
				protocol.RecordReasoningScope(prefixed, a.request.reasoningScope)
			}
			return a.addOpaqueReasoning(prefixed)
		case "text":
			return a.addText(block.Text)
		}
	case "content_block_delta":
		state := s.blocks[event.Index]
		if state == nil {
			return fmt.Errorf("Anthropic delta for unknown content block %d", event.Index)
		}
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
		}
		if err := json.Unmarshal(event.Delta, &delta); err != nil {
			return fmt.Errorf("decode Anthropic delta: %w", err)
		}
		switch delta.Type {
		case "text_delta":
			return a.addText(delta.Text)
		case "thinking_delta":
			if delta.Thinking != "" {
				state.thinking = true
			}
			return a.addThinking(delta.Thinking)
		case "signature_delta":
			if err := a.retain(len(delta.Signature)); err != nil {
				return err
			}
			state.signature.WriteString(delta.Signature)
		case "input_json_delta":
			if err := a.retain(len(delta.PartialJSON)); err != nil {
				return err
			}
			state.input.WriteString(delta.PartialJSON)
		}
	case "content_block_stop":
		state := s.blocks[event.Index]
		delete(s.blocks, event.Index)
		if state == nil {
			return nil
		}
		switch state.kind {
		case "tool_use":
			// Tool JSON bytes were already retained on input_json_delta; credit back
			// before addToolUse retains the final payload so the cap is not halved.
			// Defer emission until message_stop so message_delta(max_tokens) can
			// cleanly drop a truncated tool call instead of failing the stream.
			if n := state.input.Len(); n > 0 && a.retainedBytes >= n {
				a.retainedBytes -= n
			}
			if len(s.pendingTools) >= assemblerMaxToolCalls {
				return fmt.Errorf("Anthropic Messages stream exceeds %d concurrent tool calls", assemblerMaxToolCalls)
			}
			s.pendingTools = append(s.pendingTools, anthropicPendingTool{
				id: state.id, name: state.name, input: state.input.String(),
			})
			return nil
		case "thinking":
			sig := state.signature.String()
			if sig == "" {
				return nil
			}
			prefixed := anthropicSignaturePrefix + sig
			if a.request != nil && a.request.reasoningScope != "" {
				protocol.RecordReasoningScope(prefixed, a.request.reasoningScope)
			}
			if state.thinking && a.activeType == blockThinking && a.activeIndex >= 0 {
				a.blocks[a.activeIndex].Signature = prefixed
				return a.closeActive()
			}
			// Signature bytes were already retained on signature_delta; credit
			// back before addOpaqueReasoning retains the final payload.
			if n := state.signature.Len(); n > 0 && a.retainedBytes >= n {
				a.retainedBytes -= n
			}
			// Thinking with no visible text still has to be replayed.
			return a.addOpaqueReasoning(prefixed)
		}
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(event.Delta, &delta); err == nil && delta.StopReason != "" {
			a.stopReason = anthropicStopReason(delta.StopReason)
		}
		var usage map[string]any
		if err := json.Unmarshal(event.Usage, &usage); err == nil && usage != nil {
			s.finalUsage = usage
			anthropicApplyUsage(a, usage, true)
		}
	case "message_stop":
		s.terminalSeen = true
	case "error":
		fallback := strings.TrimSpace(string(payload))
		if event.Error != nil {
			fallback = fmt.Sprintf("Anthropic Messages upstream error %s: %s", event.Error.Type, event.Error.Message)
		}
		return buildStreamUpstreamError("anthropic", string(payload), fallback)
	}
	return nil
}

// anthropicApplyUsage records Messages usage. input_tokens excludes cached
// tokens, which arrive separately as cache reads and writes. When fromDelta is
// true, all-zero input/cache fields from compatibility proxies do not clobber
// positive values already received in message_start.
func anthropicApplyUsage(a *responseAssembler, usage map[string]any, fromDelta bool) {
	if usage == nil {
		return
	}
	inVal, hasIn := usage["input_tokens"]
	hasIn = hasIn && inVal != nil
	readVal, hasRead := usage["cache_read_input_tokens"]
	hasRead = hasRead && readVal != nil
	writeVal, hasWrite := usage["cache_creation_input_tokens"]
	hasWrite = hasWrite && writeVal != nil

	cacheWrite := intValue(writeVal)
	if cacheCreation := mapValue(usage["cache_creation"]); cacheCreation != nil {
		ephemeralSum := intValue(cacheCreation["ephemeral_5m_input_tokens"]) + intValue(cacheCreation["ephemeral_1h_input_tokens"])
		if ephemeralSum > 0 && cacheWrite == 0 {
			cacheWrite = ephemeralSum
			hasWrite = true
		}
	}

	deltaInputTotal := intValue(inVal) + intValue(readVal) + cacheWrite
	preserveStartInput := fromDelta && deltaInputTotal == 0 && a.inputUsageKnown &&
		(a.contextTokens > 0 || a.cacheReadTokens > 0 || a.cacheWriteTokens > 0)

	if !preserveStartInput {
		if hasIn {
			a.inputUsageKnown = true
			a.contextTokens = intValue(inVal)
		}
		if hasRead {
			a.cacheReadTokens = intValue(readVal)
		}
		if hasWrite {
			a.cacheWriteTokens = cacheWrite
		}
	}
	if value, ok := usage["output_tokens"]; ok && value != nil {
		a.outputUsageKnown = true
		a.outputTokens = intValue(value)
	}
}

// anthropicStopReason maps a Messages stop_reason onto the assembler's.
func anthropicStopReason(reason string) string {
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return stopMaxTokens
	case "tool_use":
		return stopToolUse
	case "refusal":
		return stopContentFilter
	default:
		return stopEndTurn
	}
}
