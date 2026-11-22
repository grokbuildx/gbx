package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/tidwall/gjson"
)

func TestBatch2OpenAIChatMaxCompletionTokensFallbacksAndReasoning(t *testing.T) {
	// 1. OpenAI o-series / GPT-5 use max_completion_tokens, independent temp+top_p, effort="none", json_object.
	raw, err := convertResponsesToChatCompletions([]byte(`{
		"model":"o3-mini",
		"max_output_tokens":2048,
		"temperature":0.7,
		"top_p":0.9,
		"reasoning":{"effort":"none"},
		"service_tier":"Scale",
		"safety_identifier":"user-safe-1",
		"prompt_cache_key":"cache-k1",
		"prompt_cache_retention":"24h",
		"text":{"format":{"type":"json_object"}},
		"input":"hi"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	parsed := gjson.ParseBytes(raw.body)
	if parsed.Get("max_completion_tokens").Int() != 2048 || parsed.Get("max_tokens").Exists() {
		t.Fatalf("o3-mini must use max_completion_tokens, got: %s", raw.body)
	}
	if parsed.Get("temperature").Float() != 0.7 || parsed.Get("top_p").Float() != 0.9 {
		t.Fatalf("temperature and top_p must both be forwarded: %s", raw.body)
	}
	if parsed.Get("reasoning_effort").String() != "none" {
		t.Fatalf("effort=none must pass reasoning_effort=none: %s", raw.body)
	}
	if parsed.Get("response_format.type").String() != "json_object" {
		t.Fatalf("json_object lost: %s", raw.body)
	}
	if parsed.Get("service_tier").String() != "scale" || parsed.Get("safety_identifier").String() != "user-safe-1" ||
		parsed.Get("prompt_cache_key").String() != "cache-k1" || parsed.Get("prompt_cache_retention").String() != "24h" {
		t.Fatalf("optional Chat params lost: %s", raw.body)
	}

	// 2. 400 fallback loop: max_tokens -> max_completion_tokens + stream_options strip + reasoning_content strip + caching.
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		g := gjson.ParseBytes(b)
		if g.Get("max_tokens").Exists() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`)
			return
		}
		if g.Get("stream_options").Exists() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: stream_options"}}`)
			return
		}
		if g.Get(`messages.#(reasoning_content!="")`).Exists() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized field reasoning_content in messages"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	svc := &chatCompletionsService{
		apiKey:     "local-key",
		endpoint:   upstream.URL,
		authorizer: &chatStaticAuthorizer{token: "up-key"},
		client:     upstream.Client(),
	}
	reqPayload := `{"model":"custom-model","stream":true,"max_output_tokens":100,"input":[
		{"role":"user","content":"q1"},
		{"type":"reasoning","id":"rs_gbx_1","summary":[{"type":"summary_text","text":"old thought"}]},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"q2"}
	]}`
	for turn := 0; turn < 2; turn++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqPayload))
		req.Header.Set("Authorization", "Bearer local-key")
		rec := httptest.NewRecorder()
		svc.handleResponses(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d status = %d; body=%s", turn, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// Turn 1: 4 attempts (max_tokens 400 -> stream_options 400 -> reasoning_content 400 -> 200).
	// Turn 2: learned max_completion_tokens & noStreamOptions immediately, so only reasoning_content 400 -> 200 (2 calls).
	if len(bodies) != 6 {
		t.Fatalf("expected 4 + 2 = 6 upstream calls with cached compat quirks, got %d", len(bodies))
	}
	turn2First := gjson.Parse(bodies[4])
	if !turn2First.Get("max_completion_tokens").Exists() || turn2First.Get("stream_options").Exists() {
		t.Fatalf("turn 2 did not reuse learned compat quirks: %s", bodies[4])
	}

	// 3. Toggle/budget-only reasoning model (claude-sonnet-4-5-20250929 in models.dev has budget_tokens & empty efforts)
	// must NOT have reasoning_effort stripped, AND max_tokens must clamp to MaxOutputTokens (64000)!
	seedBatch2ModelsDevCache(t, `{
		"anthropic": {
			"id": "anthropic",
			"name": "Anthropic",
			"api": "https://api.anthropic.com/v1",
			"models": {
				"claude-sonnet-4-5-20250929": {
					"id": "claude-sonnet-4-5-20250929",
					"name": "Claude Sonnet 4.5",
					"reasoning": true,
					"limit": {"context": 200000, "output": 64000},
					"reasoning_options": [{"type": "budget_tokens"}]
				},
				"claude-opus-4-7": {
					"id": "claude-opus-4-7",
					"name": "Claude Opus 4.7",
					"reasoning": true,
					"limit": {"context": 1000000, "output": 128000},
					"reasoning_options": [{"type": "effort", "values": ["low", "medium", "high", "xhigh", "max"]}]
				}
			}
		}
	}`)
	svc2 := &chatCompletionsService{modelsDevProvider: "anthropic"}
	convToggle, err := svc2.convertRequest([]byte(`{"model":"claude-sonnet-4-5-20250929","reasoning":{"effort":"high"},"max_output_tokens":999999,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	gToggle := gjson.ParseBytes(convToggle.body)
	if gToggle.Get("reasoning_effort").String() != "high" {
		t.Fatalf("toggle/budget reasoning model had reasoning_effort stripped: %s", convToggle.body)
	}
	if gToggle.Get("max_tokens").Int() != 64000 {
		t.Fatalf("max_tokens = %d, want clamped 64000: %s", gToggle.Get("max_tokens").Int(), convToggle.body)
	}
}

func seedBatch2ModelsDevCache(t *testing.T, catalogJSON string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cachePath, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(catalogJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBatch2AnthropicAdaptiveThinkingFallbackClampingAndScope(t *testing.T) {
	// 1. Adaptive model (claude-opus-4-7) emits thinking.type="adaptive" + output_config.effort.
	svc := newAnthropicMessagesService("local-key", "https://api.anthropic.com/v1", nil, nil, "up-key", nil)
	svc.modelsDevProvider = "anthropic"
	conv, err := svc.convertRequest([]byte(`{
		"model":"claude-opus-4-7",
		"reasoning":{"effort":"high"},
		"safety_identifier":"uid-9",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"instructions":"Be concise.",
		"input":[
			{"role":"user","content":"u1"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"u2"},
			{"role":"assistant","content":"a2"},
			{"role":"user","content":"u3"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(conv.body)
	if g.Get("thinking.type").String() != "adaptive" || g.Get("output_config.effort").String() != "high" {
		t.Fatalf("adaptive model did not emit adaptive thinking + output_config: %s", conv.body)
	}
	if g.Get("metadata.user_id").String() != "uid-9" {
		t.Fatalf("metadata.user_id missing: %s", conv.body)
	}
	// Verify 4 cache breakpoints: tools[0], system[0], and last 2 user turns (messages[2] & messages[4]).
	if !g.Get("tools.0.cache_control").Exists() || !g.Get("system.0.cache_control").Exists() ||
		!g.Get("messages.2.content.0.cache_control").Exists() || !g.Get("messages.4.content.0.cache_control").Exists() ||
		g.Get("messages.0.content.0.cache_control").Exists() {
		t.Fatalf("expected exactly 4 cache breakpoints (tools, system, last 2 user turns): %s", conv.body)
	}

	// 2. effort="none" on thinking model emits thinking.type="disabled" and clamps temperature to [0, 1].
	convOff, err := svc.convertRequest([]byte(`{"model":"claude-opus-4-7","reasoning":{"effort":"none"},"temperature":1.8,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	gOff := gjson.ParseBytes(convOff.body)
	if gOff.Get("thinking.type").String() != "disabled" || gOff.Get("temperature").Float() != 1.0 {
		t.Fatalf("effort=none / temperature clamping wrong: %s", convOff.body)
	}

	// 3. Enabled thinking fallback: prefers adaptive downgrade first, then full strip restoring max_tokens + temp + stripping historical thinking.
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, string(b))
		g := gjson.ParseBytes(b)
		if g.Get("thinking.type").String() == "enabled" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"budget_tokens is not supported, use adaptive thinking"}}`)
			return
		}
		if g.Get("thinking.type").String() == "adaptive" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid signature in thinking block"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE(
			`{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"recovered"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer upstream.Close()

	fbSvc := newAnthropicMessagesService("local-key", upstream.URL, nil, nil, "up-key", nil)
	fbSvc.client = upstream.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-custom",
		"reasoning":{"effort":"max"},
		"temperature":0.4,
		"input":[
			{"role":"user","content":"u1"},
			{"type":"reasoning","id":"rs_gbx_1","summary":[{"type":"summary_text","text":"old"}],"encrypted_content":"gbx-anthropic:stale-sig"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"u2"}
		]
	}`))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	fbSvc.handleResponses(rec, req)
	if rec.Code != http.StatusOK || len(calls) != 3 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(calls), rec.Body.String())
	}
	// Attempt 2 must try adaptive with original max_tokens (8192, not inflated 20480).
	call2 := gjson.Parse(calls[1])
	if call2.Get("thinking.type").String() != "adaptive" || call2.Get("max_tokens").Int() != anthropicDefaultMaxTokens {
		t.Fatalf("attempt 2 did not downgrade to adaptive with restored max_tokens: %s", calls[1])
	}
	// Attempt 3 must strip thinking, output_config, historical thinking block, and restore temperature=0.4 & max_tokens=8192.
	call3 := gjson.Parse(calls[2])
	if call3.Get("thinking").Exists() || call3.Get("output_config").Exists() ||
		call3.Get("temperature").Float() != 0.4 || call3.Get("max_tokens").Int() != anthropicDefaultMaxTokens ||
		strings.Contains(calls[2], "stale-sig") {
		t.Fatalf("attempt 3 did not cleanly restore params and strip thinking history: %s", calls[2])
	}

	// 4. Cross-model Anthropic signature scope isolation.
	sonnetScope := fbSvc.reasoningScope("claude-sonnet-4-6", "claude-sonnet-4-6")
	protocol.RecordReasoningScope(anthropicSignaturePrefix+"sig-from-sonnet", sonnetScope)
	convOpus, err := fbSvc.convertRequest([]byte(`{
		"model":"claude-opus-4-6",
		"reasoning":{"effort":"high"},
		"input":[
			{"role":"user","content":"u1"},
			{"type":"reasoning","id":"rs_gbx_1","summary":[{"type":"summary_text","text":"sonnet thought"}],"encrypted_content":"gbx-anthropic:sig-from-sonnet"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"u2"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(convOpus.body), "sig-from-sonnet") {
		t.Fatalf("Sonnet signature leaked into Opus request: %s", convOpus.body)
	}
}

func TestBatch2ResponsesSignatureScopeXaiNoneAndPassthroughControls(t *testing.T) {
	// 1. Scoped gbx-responses-reasoning-v1 drops cross-scope items and drops empty encrypted_content.
	sigA, err := codexReasoningSignatureScoped(map[string]any{
		"id": "rs_1", "type": "reasoning", "summary": []any{}, "encrypted_content": "cipher-A",
	}, "responses:https://a.test:m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := codexReasoningFromSignatureScoped(sigA, "responses:https://b.test:m2"); err != nil || matched {
		t.Fatalf("cross-scope signature should be dropped cleanly, got matched=%v err=%v", matched, err)
	}
	if _, matched, err := codexReasoningFromSignatureScoped(sigA, "responses:https://a.test:m1"); err != nil || !matched {
		t.Fatalf("same-scope signature should match, got matched=%v err=%v", matched, err)
	}
	summaryOnlySig, err := codexReasoningSignatureScoped(map[string]any{
		"id": "rs_2", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "only summary"}},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := codexReasoningFromSignatureScoped(summaryOnlySig, ""); err != nil || matched {
		t.Fatalf("summary-only reasoning without encrypted_content must be dropped under store=false, got matched=%v err=%v", matched, err)
	}

	// 2. normalizeXaiReasoning must NEVER clamp effort="none" up to "low", and must work for xAI API-key too.
	xaiSvc := &codexResponsesService{
		modelsDevProvider: "xai",
		modelDetails: map[string]protocol.ModelInfo{
			"grok-4.20": {
				ID:              "grok-4.20",
				ReasoningKnown:  true,
				ReasoningLevels: []protocol.ReasoningLevel{{Effort: "low"}, {Effort: "high"}},
			},
		},
	}
	payloadNone := map[string]any{"reasoning": map[string]any{"effort": "none"}}
	if enabled := xaiSvc.normalizeXaiReasoning("grok-4.20", payloadNone); enabled {
		t.Fatalf("effort=none returned enabled=true: %+v", payloadNone)
	}
	if _, exists := payloadNone["reasoning"]; exists {
		t.Fatalf("effort=none was clamped instead of disabling reasoning: %+v", payloadNone)
	}

	// 3. servePassthrough strips stop, merges include, isolates foreign native reasoning, and retries 400 effort error.
	protocol.RecordReasoningScope("native-openai-cipher-xyz", "openai-native")
	var captured []string
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = append(captured, string(b))
		if calls.Add(1) == 1 && gjson.GetBytes(b, "reasoning.effort").Exists() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported value for reasoning.effort"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_xai\",\"encrypted_content\":\"xai-cipher-123\",\"summary\":[]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()

	ptSvc := &codexResponsesService{
		apiKey:            "local-key",
		endpoint:          upstream.URL,
		models:            []string{"grok-4.20"},
		modelRoute:        map[string]string{"grok-4.20": "grok-4.20"},
		modelsDevProvider: "xai",
		authorizer:        &codexStaticAuthorizer{token: "up-key"},
		client:            upstream.Client(),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"grok-4.20",
		"stream":false,
		"stop":["END"],
		"include":["message.input_image.image_url"],
		"prompt_cache_key":"",
		"reasoning":{"effort":"high"},
		"input":[
			{"type":"reasoning","id":"rs_openai_1","encrypted_content":"native-openai-cipher-xyz","summary":[]},
			{"role":"user","content":"hi"}
		]
	}`))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	ptSvc.handleResponses(rec, req)
	if rec.Code != http.StatusOK || len(captured) != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(captured), rec.Body.String())
	}
	first := gjson.Parse(captured[0])
	if first.Get("stop").Exists() {
		t.Fatalf("stop leaked to Responses passthrough: %s", captured[0])
	}
	if !strings.Contains(first.Get("include").Raw, "message.input_image.image_url") ||
		!strings.Contains(first.Get("include").Raw, "reasoning.encrypted_content") {
		t.Fatalf("include did not merge existing + encrypted_content: %s", first.Get("include").Raw)
	}
	if strings.Contains(captured[0], "native-openai-cipher-xyz") {
		t.Fatalf("foreign native OpenAI encrypted_content leaked into xAI passthrough: %s", captured[0])
	}
	if first.Get("prompt_cache_key").String() == "" {
		t.Fatalf("empty prompt_cache_key was not populated with sessionID: %s", captured[0])
	}
	// Verify xAI emitted reasoning was recorded under xAI scope so native OpenAI will reject it.
	if protocol.ReasoningScopeMatches("xai-cipher-123", "openai-native") {
		t.Fatal("xAI passthrough encrypted_content must NOT match openai-native scope")
	}

	// 4. convertResponsesForUpstream preserves reasoning.summary="concise" & "none", text.verbosity, json_object, service_tier="scale".
	convResp, err := convertResponsesForUpstream([]byte(`{
		"model":"gpt-5",
		"reasoning":{"effort":"high","summary":"concise"},
		"text":{"verbosity":"low","format":{"type":"json_object"}},
		"service_tier":"Scale",
		"metadata":{"trace":"t1"},
		"safety_identifier":"safe-1",
		"prompt_cache_retention":"24h",
		"include":["file_search_call.results"],
		"input":"hi"
	}`), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	gr := gjson.ParseBytes(convResp.body)
	if gr.Get("reasoning.summary").String() != "concise" || gr.Get("text.verbosity").String() != "low" ||
		gr.Get("text.format.type").String() != "json_object" || gr.Get("service_tier").String() != "scale" ||
		gr.Get("metadata.trace").String() != "t1" || gr.Get("safety_identifier").String() != "safe-1" ||
		gr.Get("prompt_cache_retention").String() != "24h" {
		t.Fatalf("converted Responses params lost: %s", convResp.body)
	}
}

func TestBatch2InlineThinkingStructuredOutputsAndBackends(t *testing.T) {
	// 1. Start-anchored <think> extraction across 1-byte chunks even with thinkingEnabled=false.
	a := newResponseAssembler(&adapterRequest{clientModel: "m", thinkingEnabled: false}, nil)
	for _, b := range []byte("  <think>step 1</think>final answer") {
		if err := a.addAssistantContent(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.finish(); err != nil {
		t.Fatal(err)
	}
	blocks := a.contentBlocks()
	if len(blocks) != 2 || blocks[0].Type != blockThinking || blocks[0].Thinking != "step 1" || blocks[1].Type != blockText || blocks[1].Text != "final answer" {
		t.Fatalf("1-byte <think> extraction failed: %+v", blocks)
	}

	// 2. Mid-message <think> inside code/prose MUST NOT hijack text mode.
	aCode := newResponseAssembler(&adapterRequest{clientModel: "m", thinkingEnabled: true}, nil)
	if err := aCode.addAssistantContent("Use <think>tags</think> in XML."); err != nil {
		t.Fatal(err)
	}
	if err := aCode.finish(); err != nil {
		t.Fatal(err)
	}
	codeBlocks := aCode.contentBlocks()
	if len(codeBlocks) != 1 || codeBlocks[0].Type != blockText || codeBlocks[0].Text != "Use <think>tags</think> in XML." {
		t.Fatalf("mid-message <think> hijacked text stream: %+v", codeBlocks)
	}

	// 3. chat_front: max_completion_tokens overrides max_tokens; json_schema defaults strict:false; json_object preserved; assistant.reasoning preserved.
	chatReqBytes, _, err := chatToResponsesRequest([]byte(`{
		"model":"m",
		"max_tokens":4096,
		"max_completion_tokens":512,
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}}},
		"messages":[
			{"role":"user","content":"u1"},
			{"role":"assistant","reasoning":"thought-via-reasoning","content":"a1"},
			{"role":"user","content":"u2"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	gc := gjson.ParseBytes(chatReqBytes)
	if gc.Get("max_output_tokens").Int() != 512 {
		t.Fatalf("max_completion_tokens must override max_tokens, got %d", gc.Get("max_output_tokens").Int())
	}
	if gc.Get("text.format.strict").Type != gjson.False {
		t.Fatalf("omitted Chat json_schema.strict must default to false, got: %s", gc.Get("text.format").Raw)
	}
	if !strings.Contains(string(chatReqBytes), "thought-via-reasoning") {
		t.Fatalf("assistant.reasoning lost in chat_front: %s", chatReqBytes)
	}

	// 4. schemaSupportsStrictOutput catches forbidden keywords, non-object root, type array, and extra required keys.
	badSchemas := []string{
		`{"type":"array","items":{"type":"string"}}`,
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["a","missing"],"additionalProperties":false}`,
		`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}},"required":["a"],"additionalProperties":false}`,
		`{"type":["object","null"],"properties":{"a":{"type":"string"}},"required":["a"]}`,
	}
	for _, s := range badSchemas {
		if schemaSupportsStrictOutput(gjson.Parse(s)) {
			t.Fatalf("schemaSupportsStrictOutput wrongly accepted %s", s)
		}
	}
	withMeta := &conversationFormat{
		Type:   "json_schema",
		Name:   "clean",
		Schema: json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`),
	}
	outSchema := structuredOutputSchema(withMeta)
	if bytes.Contains(outSchema["schema"].(json.RawMessage), []byte("$schema")) {
		t.Fatalf("$schema was not stripped: %s", outSchema["schema"])
	}

	// 5. Kiro & Cursor structured output prompt injection + Kiro adaptive 4.7/5 + gbx-kiro: prefix.
	structuredReq := `{"model":"claude-opus-4-7","reasoning":{"effort":"high"},"text":{"format":{"type":"json_object"}},"input":"hi"}`
	kiroConv, err := convertResponsesToKiro([]byte(structuredReq))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustJSON(kiroConv.body)), "valid JSON object") {
		t.Fatalf("Kiro dropped text.format json_object: %+v", kiroConv.body)
	}
	if fields := kiroReasoningFields(&conversationRequest{Effort: "high"}, "claude-opus-4.7"); fields == nil {
		t.Fatal("kiroReasoningFields missed claude-opus-4.7")
	}
	cursorConv, err := convertResponsesToCursor([]byte(structuredReq))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cursorConv.system, "valid JSON object") {
		t.Fatalf("Cursor dropped text.format json_object: %q", cursorConv.system)
	}

	// 6. Qoder: enable_thinking:false on effort=none, strips temp/top_p when thinking on, qoderMatchEffort clamps medium/ultra, stateful split tag strip.
	qCatalog := []qoderModel{{
		ID:        "q1",
		Reasoning: true,
		Config: map[string]any{
			"thinking_config": map[string]any{
				"enabled": map[string]any{
					"efforts": map[string]any{
						"low":  map[string]any{},
						"high": map[string]any{},
					},
				},
			},
		},
	}}
	qOff, err := convertResponsesToQoder([]byte(`{"model":"q1","reasoning":{"effort":"none"},"temperature":0.5,"input":"hi"}`), qCatalog)
	if err != nil {
		t.Fatal(err)
	}
	qOffParams := qOff.body["parameters"].(map[string]any)
	if qOffParams["enable_thinking"] != false || qOffParams["temperature"] != 0.5 {
		t.Fatalf("Qoder effort=none params = %+v", qOffParams)
	}
	qOn, err := convertResponsesToQoder([]byte(`{"model":"q1","reasoning":{"effort":"medium"},"temperature":0.5,"top_p":0.8,"input":"hi"}`), qCatalog)
	if err != nil {
		t.Fatal(err)
	}
	qOnParams := qOn.body["parameters"].(map[string]any)
	if qOnParams["enable_thinking"] != true || qOnParams["reasoning_effort"] != "low" {
		t.Fatalf("Qoder effort=medium should clamp to low, got %+v", qOnParams)
	}
	if _, hasT := qOnParams["temperature"]; hasT {
		t.Fatalf("Qoder thinking=true must strip temperature: %+v", qOnParams)
	}
	if _, hasP := qOnParams["top_p"]; hasP {
		t.Fatalf("Qoder thinking=true must strip top_p: %+v", qOnParams)
	}
	// Split-chunk Qoder tag stripping across chunks.
	var stripper qoderThinkingTagStripper
	stripped := stripper.push("hello <thi") + stripper.push("nking>world</thi") + stripper.push("nking>!") + stripper.flush()
	if stripped != "hello world!" {
		t.Fatalf("stateful Qoder tag stripper = %q, want %q", stripped, "hello world!")
	}
	_ = context.Background()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
