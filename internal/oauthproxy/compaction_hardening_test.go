package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestCodexResponsesCompactStripsForbiddenFieldsAndDecodesGBXCompaction(t *testing.T) {
	var seenBody map[string]any
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp_up","object":"response.compaction","output":[]}`)
	}))
	defer upstream.Close()

	rt, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	payload := map[string]any{
		"model":             "gpt-5",
		"stream":            true,
		"store":             false,
		"tools":             []any{map[string]any{"type": "function", "name": "exec"}},
		"reasoning":         map[string]any{"effort": "xhigh"},
		"max_output_tokens": 8192,
		"prompt_cache_key":  "sess-1",
		"input": []any{
			map[string]any{"type": "compaction", "encrypted_content": EncodeCompactionSummary("prior goal: fix compact")},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses/compact", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+rt.APIKey())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if seenPath != "/v1/responses/compact" {
		t.Fatalf("seenPath=%q", seenPath)
	}
	for _, forbidden := range []string{"stream", "store", "tools", "reasoning", "max_output_tokens", "prompt_cache_key"} {
		if _, exists := seenBody[forbidden]; exists {
			t.Fatalf("forbidden field %q leaked to /responses/compact: %+v", forbidden, seenBody)
		}
	}
	encodedSeen, _ := json.Marshal(seenBody["input"])
	if strings.Contains(string(encodedSeen), CompactionSummaryPrefix) || !strings.Contains(string(encodedSeen), "prior goal: fix compact") {
		t.Fatalf("gbx1 compaction item was not decoded before passthrough: %s", encodedSeen)
	}
}

func TestCodexResponsesCompactFallsBackToLocalOnUnsupportedAndOverflow(t *testing.T) {
	t.Run("404 unsupported falls back to local compaction", func(t *testing.T) {
		var compactHits, responsesHits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/compact") {
				compactHits.Add(1)
				http.Error(w, `{"error":{"message":"unknown endpoint /responses/compact"}}`, http.StatusNotFound)
				return
			}
			responsesHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_sum","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"local-fallback-summary"}]}]}`)
		}))
		defer upstream.Close()

		rt, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "gpt-5")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses/compact", strings.NewReader(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"remember alpha"}]}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"response.compaction"`) || !strings.Contains(string(body), "local-fallback-summary") {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if compactHits.Load() != 1 || responsesHits.Load() != 1 {
			t.Fatalf("compactHits=%d responsesHits=%d", compactHits.Load(), responsesHits.Load())
		}
	})

	t.Run("401 unauthorized does not fall back", func(t *testing.T) {
		var responsesHits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/compact") {
				http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
				return
			}
			responsesHits.Add(1)
		}))
		defer upstream.Close()

		rt, err := StartOpenAIResponsesAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "gpt-5")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses/compact", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || responsesHits.Load() != 0 {
			t.Fatalf("status=%d responsesHits=%d", resp.StatusCode, responsesHits.Load())
		}
	})
}

func TestLocalCompactionDropsReasoningAcceptsIncompleteAndRetriesOverflow(t *testing.T) {
	// Verify multibyte UTF-8 truncation preserves valid rune boundaries.
	multibyte := strings.Repeat("\u4e2d\u6587\u538b\u7f29\u6d4b\u8bd5", 5000)
	excerpted := ExcerptCompactionText(multibyte, 1024)
	if !utf8.ValidString(excerpted) || !strings.Contains(excerpted, "truncated for compaction") || len(excerpted) > 1024 {
		t.Fatalf("invalid UTF-8 excerpt: valid=%v len=%d", utf8.ValidString(excerpted), len(excerpted))
	}

	var calls atomic.Int32
	var attemptSizes []int
	handler := localCompactionHandler("local-key", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		attemptSizes = append(attemptSizes, len(raw))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if _, hasReasoning := body["reasoning"]; hasReasoning {
			t.Error("local compaction leaked reasoning parameter")
		}
		if strings.Contains(string(raw), `"type":"reasoning"`) {
			t.Error("local compaction leaked reasoning input item")
		}
		if strings.Contains(string(raw), CompactionSummaryPrefix) {
			t.Error("local compaction failed to decode prior gbx1 compaction item")
		}
		if calls.Add(1) == 1 {
			writeProxyErrorDetail(w, http.StatusBadRequest, "invalid_request_error", "context_length_exceeded", "", "maximum context length exceeded")
			return
		}
		// Return status:"incomplete" with a non-empty summary to verify it is accepted.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_inc","object":"response","status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"rescued-summary"}]}]}`)
	})

	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "initial-goal"},
		map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "opaque"},
		map[string]any{"type": "compaction", "encrypted_content": EncodeCompactionSummary("earlier-summary")},
	}
	for i := 0; i < 20; i++ {
		items = append(items, map[string]any{
			"type":    "function_call_output",
			"call_id": "call_big",
			"output":  strings.Repeat("x", 12000),
		})
	}
	items = append(items, map[string]any{"type": "message", "role": "user", "content": "latest-task"})

	rawReq, _ := json.Marshal(map[string]any{
		"model":     "m",
		"reasoning": map[string]any{"effort": "max"},
		"input":     items,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(rawReq))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rescued-summary") {
		t.Fatalf("missing rescued summary: %s", rec.Body.String())
	}
	if calls.Load() != 2 || len(attemptSizes) != 2 || attemptSizes[1] >= attemptSizes[0] {
		t.Fatalf("expected shrinking overflow retry, calls=%d sizes=%v", calls.Load(), attemptSizes)
	}
}

func TestOpenAIChatStreamOnlyFallbackAndCompaction(t *testing.T) {
	t.Run("non-stream 400 upgrades to SSE caches requireStream and compacts", func(t *testing.T) {
		var nonStreamRejects, streamCalls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"stream-rescued-summary\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()

		rt, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "chat-model")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		// 1. Compaction triggers non-stream request -> upgrades to stream=true and succeeds.
		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses/compact", strings.NewReader(`{"model":"chat-model","input":[{"type":"message","role":"user","content":"remember alpha"}]}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"response.compaction"`) || !strings.Contains(string(body), "stream-rescued-summary") {
			t.Fatalf("compact status=%d body=%s", resp.StatusCode, body)
		}
		if nonStreamRejects.Load() != 1 || streamCalls.Load() != 1 {
			t.Fatalf("first call nonStreamRejects=%d streamCalls=%d", nonStreamRejects.Load(), streamCalls.Load())
		}

		// 2. Subsequent non-stream request uses cached requireStream directly (0 additional 400s).
		req2, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(`{"model":"chat-model","stream":false,"input":[{"type":"message","role":"user","content":"hi"}]}`))
		req2.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req2.Header.Set("Content-Type", "application/json")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		body2, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body2), `"object":"response"`) || !strings.Contains(string(body2), "stream-rescued-summary") {
			t.Fatalf("second status=%d body=%s", resp2.StatusCode, body2)
		}
		if nonStreamRejects.Load() != 1 || streamCalls.Load() != 2 {
			t.Fatalf("second call nonStreamRejects=%d streamCalls=%d", nonStreamRejects.Load(), streamCalls.Load())
		}
	})

	t.Run("stream-only plus rejected stream_options composes cleanly", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			isStream, _ := body["stream"].(bool)
			if !isStream {
				http.Error(w, `{"error":{"message":"stream must be true"}}`, http.StatusBadRequest)
				return
			}
			if _, hasOpts := body["stream_options"]; hasOpts {
				http.Error(w, `{"error":{"message":"unsupported parameter: stream_options"}}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"composed-summary\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()

		rt, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "chat-model")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses/compact", strings.NewReader(`{"model":"chat-model","input":[{"type":"message","role":"user","content":"remember beta"}]}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "composed-summary") {
			t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
		}
		if calls.Load() != 3 {
			t.Fatalf("calls=%d, want 3 (non-stream 400 -> stream_options 400 -> clean SSE)", calls.Load())
		}
	})

	t.Run("upstream returning SSE despite stream=false still assembles JSON", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_3\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"always-sse\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()

		rt, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "upstream-key", "chat-model")
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Stop()

		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(`{"model":"chat-model","stream":false,"input":[{"type":"message","role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"response"`) || !strings.Contains(string(body), "always-sse") {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
	})
}
