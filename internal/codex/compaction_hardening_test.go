package codex

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayNativeNonCodexRoutesCompactionTriggerThroughBridge(t *testing.T) {
	var sawTrigger atomic.Bool
	var upstreamCalls atomic.Int32
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "compaction_trigger") {
			sawTrigger.Store(true)
			http.Error(w, `{"error":{"message":"Invalid value: compaction_trigger"}}`, http.StatusBadRequest)
			return
		}
		if strings.Contains(string(raw), `"reasoning"`) {
			t.Errorf("compaction leaked reasoning field: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_sum","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"third-party-responses-summary"}]}]}`)
	}, "openai_responses", Options{Desktop: true})

	resp := postResponses(t, g, map[string]any{
		"model":     "gbx/chosen/model-a",
		"reasoning": map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "remember-this-goal"},
			map[string]any{"type": "compaction_trigger"},
		},
	})
	result := responseObject(t, resp)
	if sawTrigger.Load() {
		t.Fatal("compaction_trigger leaked to non-Codex-native openai_responses upstream")
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls.Load())
	}
	output, _ := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 compaction item, got %+v", result)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "compaction" {
		t.Fatalf("item = %+v", item)
	}
	summary, ok := decodeCompactionSummary(textValue(item["encrypted_content"]))
	if !ok || summary != "third-party-responses-summary" {
		t.Fatalf("decoded summary = (%q, %v)", summary, ok)
	}
}

func TestGatewayBridgeCompactionRetriesOverflowAcceptsIncompleteAndDecodesPriorSummary(t *testing.T) {
	var calls atomic.Int32
	var payloadSizes []int
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		payloadSizes = append(payloadSizes, len(raw))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, hasReasoning := body["reasoning_effort"]; hasReasoning {
			t.Errorf("compaction leaked reasoning_effort: %s", raw)
		}
		if strings.Contains(string(raw), "opaque-reasoning-blob") {
			t.Error("compaction leaked reasoning input item")
		}
		if strings.Contains(string(raw), "gbx1:") {
			t.Error("compaction failed to decode prior gbx1 summary")
		}
		if !strings.Contains(string(raw), "prior-round-summary") {
			t.Errorf("compaction dropped prior decoded summary: %s", raw)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context window exceeded"}}`)
			return
		}
		// Finish with finish_reason:"length" (mapped to status:"incomplete") but non-empty summary.
		writeChatFixture(w, r, map[string]any{"content": "compact-summary-after-overflow"}, "length", 10)
	}, "openai", Options{Desktop: true})

	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "initial-user-goal"},
		map[string]any{"type": "reasoning", "id": "rs_old", "encrypted_content": "opaque-reasoning-blob"},
		map[string]any{"type": "compaction", "encrypted_content": encodeCompactionSummary("prior-round-summary")},
	}
	for i := 0; i < 18; i++ {
		items = append(items, map[string]any{
			"type":    "function_call_output",
			"call_id": "call_log",
			"output":  strings.Repeat("logline\n", 1500),
		})
	}
	items = append(items, map[string]any{"type": "compaction_trigger"})

	resp := postResponses(t, g, map[string]any{
		"model":     "gbx/chosen/model-a",
		"reasoning": map[string]any{"effort": "xhigh"},
		"input":     items,
	})
	result := responseObject(t, resp)
	if calls.Load() != 2 || len(payloadSizes) != 2 || payloadSizes[1] >= payloadSizes[0] {
		t.Fatalf("expected shrinking overflow retry: calls=%d sizes=%v", calls.Load(), payloadSizes)
	}
	output, _ := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", result)
	}
	item, _ := output[0].(map[string]any)
	summary, ok := decodeCompactionSummary(textValue(item["encrypted_content"]))
	if !ok || summary != "compact-summary-after-overflow" {
		t.Fatalf("decoded summary = (%q, %v)", summary, ok)
	}
}

func TestGatewayBridgeCompactionSucceedsAgainstStreamOnlyChatUpstream(t *testing.T) {
	var nonStreamRejects, streamCalls atomic.Int32
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		isStream, _ := body["stream"].(bool)
		if !isStream {
			nonStreamRejects.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11101,"msg":"Non-stream chat request is currently not supported","retry_policy":{"retry":false}}`)
			return
		}
		streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_s\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"codex-stream-only-summary\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_s\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, "openai", Options{Desktop: true})

	resp := postResponses(t, g, map[string]any{
		"model": "gbx/chosen/model-a",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "keep this task"},
			map[string]any{"type": "compaction_trigger"},
		},
	})
	result := responseObject(t, resp)
	output, _ := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", result)
	}
	item, _ := output[0].(map[string]any)
	summary, ok := decodeCompactionSummary(textValue(item["encrypted_content"]))
	if !ok || summary != "codex-stream-only-summary" {
		t.Fatalf("decoded summary = (%q, %v)", summary, ok)
	}
	if nonStreamRejects.Load() != 1 || streamCalls.Load() != 1 {
		t.Fatalf("nonStreamRejects=%d streamCalls=%d, want 1 and 1", nonStreamRejects.Load(), streamCalls.Load())
	}
}
