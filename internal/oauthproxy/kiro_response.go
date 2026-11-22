package oauthproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (a *responseAssembler) process(frame *kiroEventFrame) error {
	messageType := frame.headers[":message-type"]
	switch messageType {
	case "", "event":
		return a.processEvent(frame.headers[":event-type"], frame.payload)
	case "error":
		code := frame.headers[":error-code"]
		msg := strings.TrimSpace(string(frame.payload))
		errBody, _ := json.Marshal(map[string]any{"code": code, "message": msg})
		return buildStreamUpstreamError("kiro", string(errBody), fmt.Sprintf("Kiro event stream error %s: %s", code, msg))
	case "exception":
		kind := frame.headers[":exception-type"]
		if kind == "ContentLengthExceededException" {
			// If generation already produced output or an in-flight tool call,
			// treat the limit as output truncation (response.incomplete). If zero
			// output was produced, the prompt itself overflowed the context window.
			if len(a.blocks) > 0 || len(a.tools) > 0 {
				a.stopReason = stopMaxTokens
				return nil
			}
			msg := firstNonEmptyString(kiroEventMessage(frame.payload), "input exceeds Kiro context length")
			return &streamUpstreamError{
				dialect: "kiro",
				status:  http.StatusBadRequest,
				errType: "invalid_request_error",
				code:    "context_length_exceeded",
				message: fmt.Sprintf("Kiro event stream exception %s: %s", kind, msg),
				body:    strings.TrimSpace(string(frame.payload)),
			}
		}
		msg := kiroEventMessage(frame.payload)
		errBody, _ := json.Marshal(map[string]any{"code": kind, "message": msg})
		return buildStreamUpstreamError("kiro", string(errBody), fmt.Sprintf("Kiro event stream exception %s: %s", kind, msg))
	default:
		return fmt.Errorf("unsupported Kiro EventStream message type %q", messageType)
	}
}

func (a *responseAssembler) processEvent(eventType string, payload []byte) error {
	switch eventType {
	case "assistantResponseEvent":
		var event struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.Content != "" {
			return a.addAssistantContent(event.Content)
		}
	case "reasoningContentEvent":
		var event struct {
			Text            string `json:"text"`
			Signature       string `json:"signature"`
			RedactedContent string `json:"redactedContent"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		a.nativeReasoning = true
		if err := a.flushInlineContent(); err != nil {
			return err
		}
		if event.RedactedContent != "" {
			if err := a.addOpaqueReasoning(event.RedactedContent); err != nil {
				return err
			}
		}
		if event.Text != "" {
			if err := a.addThinking(event.Text); err != nil {
				return err
			}
		}
		if event.Signature != "" && a.activeType == blockThinking && a.activeIndex >= 0 {
			a.blocks[a.activeIndex].Signature = event.Signature
		}
	case "toolUseEvent":
		var event struct {
			Name      string `json:"name"`
			ToolUseID string `json:"toolUseId"`
			Input     string `json:"input"`
			Stop      bool   `json:"stop"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.ToolUseID == "" {
			return fmt.Errorf("Kiro toolUseEvent is missing toolUseId")
		}
		accumulator := a.tools[event.ToolUseID]
		if accumulator == nil {
			if len(a.tools) >= assemblerMaxToolCalls {
				return fmt.Errorf("Kiro tool use stream exceeds %d concurrent tool calls", assemblerMaxToolCalls)
			}
			accumulator = &assemblerToolAccumulator{name: event.Name}
			a.tools[event.ToolUseID] = accumulator
		}
		if event.Name != "" {
			accumulator.name = event.Name
		}
		if err := a.retain(len(event.Input)); err != nil {
			return err
		}
		accumulator.input.WriteString(event.Input)
		if event.Stop {
			delete(a.tools, event.ToolUseID)
			if n := accumulator.input.Len(); n > 0 && a.retainedBytes >= n {
				a.retainedBytes -= n
			}
			if err := a.flushInlineContent(); err != nil {
				return err
			}
			return a.addToolUse(event.ToolUseID, accumulator.name, accumulator.input.String())
		}
	case "contextUsageEvent":
		var event struct {
			ContextUsagePercentage float64 `json:"contextUsagePercentage"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if event.ContextUsagePercentage > 0 {
			window := 0
			if a.request != nil {
				window = a.request.contextWindow
			}
			if window <= 0 {
				window = kiroContextWindow(a.request.upstreamModel)
			}
			a.inputUsageKnown = true
			a.contextTokens = int(event.ContextUsagePercentage / 100 * float64(window))
			if event.ContextUsagePercentage >= 100 {
				a.stopReason = stopMaxTokens
			}
		}
	case "meteringEvent":
		var event struct {
			Unit       string  `json:"unit"`
			UnitPlural string  `json:"unitPlural"`
			Usage      float64 `json:"usage"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		a.creditUsage = event.Usage
		a.creditUnit = event.Unit
		a.creditPlural = event.UnitPlural
	}
	return nil
}

func processKiroEventStream(reader io.Reader, assembler *responseAssembler) error {
	// Frames are read in two small chunks (prelude, then remainder), so reading
	// straight from the network body would cost two syscalls per frame.
	buffered := bufio.NewReaderSize(reader, kiroEventReadBufferSize)
	sawFrame := false
	for {
		frame, err := readKiroEventFrame(buffered)
		if err == io.EOF {
			if !sawFrame {
				return fmt.Errorf("Kiro event stream returned an empty response: %w", io.ErrUnexpectedEOF)
			}
			return assembler.finish()
		}
		if err != nil {
			return err
		}
		sawFrame = true
		if err := assembler.process(frame); err != nil {
			return err
		}
	}
}

func kiroEventMessage(payload []byte) string {
	var value struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &value) == nil && value.Message != "" {
		return value.Message
	}
	return strings.TrimSpace(string(payload))
}

func kiroContextWindow(model string) int {
	switch model {
	case "claude-opus-5", "claude-sonnet-5",
		"claude-opus-4.8", "claude-opus-4.7",
		"claude-opus-4.6", "claude-sonnet-4.6":
		return 1_000_000
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return 272_000
	default:
		return 200_000
	}
}
