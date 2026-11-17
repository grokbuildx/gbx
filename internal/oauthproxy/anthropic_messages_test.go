package oauthproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/tidwall/gjson"
)

// anthropicSSE renders Messages stream events.
func anthropicSSE(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		kind := gjson.Get(event, "type").String()
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", kind, event)
	}
	return b.String()
}

func anthropicThinkingToolStream() string {
	return anthropicSSE(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":120,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Need the file list."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Listing files."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"shell","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	)
}

type anthropicUpstreamCall struct {
	path, apiKey, version, auth string
	body                        []byte
}

func TestMixedRuntimeRoutesAnthropicModelsThroughMessages(t *testing.T) {
	var mu sync.Mutex
	var calls []anthropicUpstreamCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, anthropicUpstreamCall{path: r.URL.Path, apiKey: r.Header.Get("x-api-key"),
			version: r.Header.Get("anthropic-version"), auth: r.Header.Get("Authorization"), body: body})
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, anthropicThinkingToolStream())
			return
		}
		_, _ = io.WriteString(w, anthropicSSE(
			`{"type":"message_start","message":{"usage":{"input_tokens":200,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer upstream.Close()

	runtime, err := StartMixedProtocolAPIKeyRuntime(context.Background(), upstream.URL+"/openai/v1", "upstream-key",
		"chat-model,claude-sonnet",
		map[string]string{"chat-model": "openai", "claude-sonnet": "anthropic"},
		map[string]string{"claude-sonnet": upstream.URL + "/anthropic/v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	if models := runtime.Models(); len(models) != 2 {
		t.Fatalf("runtime models = %v, want chat-model and claude-sonnet", models)
	}

	post := func(body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, runtime.Endpoint()+"/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Turn 1: non-streaming client, thinking on, one tool.
	resp := post(`{"model":"claude-sonnet","instructions":"You are terse.","reasoning":{"effort":"high"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"list files"}]}],
		"tools":[{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status %d: %s", resp.StatusCode, raw)
	}

	first := calls[0]
	if first.path != "/anthropic/v1/messages" {
		t.Fatalf("upstream path = %q, want the model's own /anthropic/v1/messages", first.path)
	}
	if first.apiKey != "upstream-key" || first.version != protocol.AnthropicVersion || first.auth != "" {
		t.Fatalf("headers: x-api-key=%q anthropic-version=%q Authorization=%q", first.apiKey, first.version, first.auth)
	}
	req := gjson.ParseBytes(first.body)
	for path, want := range map[string]string{
		"model":                     "claude-sonnet",
		"stream":                    "true",
		"system.0.text":             "You are terse.",
		"messages.0.role":           "user",
		"messages.0.content.0.text": "list files",
		"tools.0.name":              "shell",
		"tools.0.input_schema.type": "object",
		"thinking.type":             "enabled",
		"thinking.budget_tokens":    "4096",
	} {
		if got := req.Get(path).String(); got != want {
			t.Fatalf("upstream %s = %q, want %q\n%s", path, got, want, first.body)
		}
	}
	if req.Get("max_tokens").Int() <= req.Get("thinking.budget_tokens").Int() {
		t.Fatalf("max_tokens %d must exceed the thinking budget", req.Get("max_tokens").Int())
	}

	out := gjson.ParseBytes(raw)
	if out.Get("status").String() != "completed" {
		t.Fatalf("response status = %q: %s", out.Get("status"), raw)
	}
	reasoning := out.Get(`output.#(type=="reasoning")`)
	if reasoning.Get("summary.0.text").String() != "Need the file list." ||
		reasoning.Get("encrypted_content").String() != anthropicSignaturePrefix+"sig-abc" {
		t.Fatalf("reasoning item = %s", reasoning.Raw)
	}
	if text := out.Get(`output.#(type=="message").content.0.text`).String(); text != "Listing files." {
		t.Fatalf("message text = %q", text)
	}
	call := out.Get(`output.#(type=="function_call")`)
	if call.Get("name").String() != "shell" || call.Get("call_id").String() != "toolu_1" ||
		gjson.Get(call.Get("arguments").String(), "command").String() != "ls" {
		t.Fatalf("function call = %s", call.Raw)
	}
	if out.Get("usage.input_tokens").Int() == 0 || out.Get("usage.output_tokens").Int() != 42 {
		t.Fatalf("usage = %s", out.Get("usage").Raw)
	}

	// Turn 2: replay the output plus the tool result, streaming.
	var history []json.RawMessage
	history = append(history, json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"list files"}]}`))
	for _, item := range out.Get("output").Array() {
		history = append(history, json.RawMessage(item.Raw))
	}
	history = append(history, json.RawMessage(`{"type":"function_call_output","call_id":"toolu_1","output":"a.txt\nb.txt"}`))
	input, _ := json.Marshal(history)
	resp = post(`{"model":"claude-sonnet","stream":true,"reasoning":{"effort":"high"},"input":` + string(input) + `,
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`)
	stream, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(stream), "response.completed") ||
		!strings.Contains(string(stream), `"delta":"Done."`) {
		t.Fatalf("turn 2 stream (%d):\n%s", resp.StatusCode, stream)
	}

	second := gjson.ParseBytes(calls[1].body)
	assistant := second.Get(`messages.#(role=="assistant")`)
	if assistant.Get("content.0.type").String() != "thinking" ||
		assistant.Get("content.0.signature").String() != "sig-abc" ||
		assistant.Get("content.0.thinking").String() != "Need the file list." {
		t.Fatalf("assistant replay must start with the signed thinking block: %s", assistant.Raw)
	}
	if assistant.Get(`content.#(type=="tool_use").input.command`).String() != "ls" {
		t.Fatalf("tool_use not replayed: %s", assistant.Raw)
	}
	result := second.Get("messages.2.content.0")
	if result.Get("type").String() != "tool_result" || result.Get("tool_use_id").String() != "toolu_1" ||
		result.Get("content.0.text").String() != "a.txt\nb.txt" {
		t.Fatalf("tool result = %s", result.Raw)
	}
	if second.Get("thinking.type").String() != "enabled" {
		t.Fatalf("thinking was dropped although the history is signed: %s", calls[1].body)
	}
}

func TestMixedRuntimeServesAnAnthropicOnlyGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE(
			`{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer upstream.Close()
	runtime, err := StartMixedProtocolAPIKeyRuntime(context.Background(), upstream.URL+"/v1", "k",
		"claude-a,claude-b", map[string]string{"claude-a": "anthropic", "claude-b": "anthropic"}, nil)
	if err != nil {
		t.Fatalf("Anthropic-only gateway: %v", err)
	}
	defer runtime.Stop()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, runtime.Endpoint()+"/responses",
		strings.NewReader(`{"model":"claude-b","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := gjson.ParseBytes(raw)
	if resp.StatusCode != http.StatusOK || out.Get("status").String() != "incomplete" ||
		out.Get(`output.0.content.0.text`).String() != "hi" {
		t.Fatalf("max_tokens turn = %d %s", resp.StatusCode, raw)
	}
}

func TestAnthropicUpstreamErrorsKeepTheirStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad thing"}}`)
	}))
	defer upstream.Close()
	runtime, err := StartMixedProtocolAPIKeyRuntime(context.Background(), upstream.URL+"/v1", "k",
		"claude-a", map[string]string{"claude-a": "anthropic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, runtime.Endpoint()+"/responses",
		strings.NewReader(`{"model":"claude-a","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "bad thing") {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
}

func TestAnthropicConversionRules(t *testing.T) {
	// Reasoning from another backend is not replayed, and without signed
	// thinking on the tool turn, thinking is turned off for the request.
	body := `{"model":"claude-a","reasoning":{"effort":"medium"},"input":[
		{"role":"user","content":"go"},
		{"type":"reasoning","id":"` + gbxReasoningIDPrefix + `x","summary":[{"type":"summary_text","text":"gemini thoughts"}],"encrypted_content":"gemini:opaque"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"ls\"}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"role":"user","content":"and now?"}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":"required","parallel_tool_calls":false,"temperature":0.2}`
	converted, err := convertResponsesToAnthropicMessages([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	req := gjson.ParseBytes(converted.body)
	if req.Get("thinking").Exists() {
		t.Fatalf("thinking must be off for a forced tool choice / unsigned history: %s", converted.body)
	}
	if req.Get("tool_choice.type").String() != "any" || !req.Get("tool_choice.disable_parallel_tool_use").Bool() {
		t.Fatalf("tool_choice = %s", req.Get("tool_choice").Raw)
	}
	if req.Get("temperature").Float() != 0.2 {
		t.Fatalf("temperature dropped without thinking: %s", converted.body)
	}
	assistant := req.Get(`messages.#(role=="assistant")`)
	if strings.Contains(assistant.Raw, "gemini") || assistant.Get("content.0.type").String() != "tool_use" {
		t.Fatalf("foreign reasoning leaked into Messages: %s", assistant.Raw)
	}
	// Tool results lead the user message that follows the tool call, and the
	// newest user content carries the cache breakpoint.
	user := req.Get("messages.2")
	if user.Get("content.0.type").String() != "tool_result" || user.Get("content.1.text").String() != "and now?" {
		t.Fatalf("user turn ordering = %s", user.Raw)
	}
	if user.Get("content.1.cache_control.type").String() != "ephemeral" {
		t.Fatalf("missing cache breakpoint: %s", user.Raw)
	}
}
