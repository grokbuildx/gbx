package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

func testPNGDataURI(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testGIFBase64(t *testing.T) string {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestBatch3ChatTopologyAndMidDeveloperInPlace(t *testing.T) {
	// 1. Pure-text user/assistant flattened to strings, mid-conversation developer kept in place,
	// empty user turn preserved, empty assistant turn dropped, assistant image stripped, refusal preserved.
	reqBody := `{
		"model":"gpt-4o",
		"instructions":"lead sys",
		"input":[
			{"type":"message","role":"system","content":"lead sys 2"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":"world"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"},{"type":"input_image","image_url":"https://example.com/a.png"}]},
			{"type":"message","role":"developer","content":"mid dev rule"},
			{"type":"message","role":"user","content":""},
			{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]},
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"inspect"},
				{"type":"input_file","filename":"doc.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"},
				{"type":"input_audio","data":"UklGRg==","format":"wav"}
			]}
		]
	}`
	converted, err := convertResponsesToChatCompletions([]byte(reqBody))
	if err != nil {
		t.Fatalf("convertResponsesToChatCompletions failed: %v", err)
	}
	parsed := gjson.ParseBytes(converted.body)
	msgs := parsed.Get("messages").Array()
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages (system + 6 turns), got %d: %s", len(msgs), converted.body)
	}
	if msgs[0].Get("role").String() != "system" || !strings.Contains(msgs[0].Get("content").String(), "lead sys 2") || strings.Contains(msgs[0].Get("content").String(), "mid dev rule") {
		t.Fatalf("leading system wrong or hoisted mid dev: %s", msgs[0].Raw)
	}
	if msgs[1].Get("role").String() != "user" || msgs[1].Get("content").Type != gjson.String || msgs[1].Get("content").String() != "hello\nworld" {
		t.Fatalf("pure-text user must flatten to string, got: %s", msgs[1].Raw)
	}
	if msgs[2].Get("role").String() != "assistant" || msgs[2].Get("content").Type != gjson.String || msgs[2].Get("content").String() != "hi" {
		t.Fatalf("assistant must flatten to string and strip image, got: %s", msgs[2].Raw)
	}
	if msgs[3].Get("role").String() != "developer" || msgs[3].Get("content").String() != "mid dev rule" {
		t.Fatalf("mid-conversation developer must stay in place, got: %s", msgs[3].Raw)
	}
	if msgs[4].Get("role").String() != "user" || msgs[4].Get("content").Type != gjson.String {
		t.Fatalf("empty user turn boundary must be preserved, got: %s", msgs[4].Raw)
	}
	if msgs[5].Get("role").String() != "assistant" || msgs[5].Get("content").String() != "cannot comply" {
		t.Fatalf("assistant refusal must be preserved, got: %s", msgs[5].Raw)
	}
	if msgs[6].Get("content").IsArray() == false || msgs[6].Get("content.1.type").String() != "file" || msgs[6].Get("content.2.type").String() != "input_audio" {
		t.Fatalf("multimodal user must emit file and input_audio array parts: %s", msgs[6].Raw)
	}

	// 2. Image file_id is explicitly rejected on OpenAI Chat Completions.
	if _, err := convertResponsesToChatCompletions([]byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_123"}]}]}`)); err == nil {
		t.Fatal("expected error for input_image file_id on Chat Completions")
	}

	// 3. Kimi merges adjacent user messages created when an empty assistant turn is dropped.
	kimiRaw := []byte(`{"model":"kimi-for-coding","messages":[{"role":"user","content":"part 1"},{"role":"assistant","content":""},{"role":"user","content":"part 2"}]}`)
	kimiNorm, err := normalizeKimiBody(kimiRaw)
	if err != nil {
		t.Fatalf("normalizeKimiBody: %v", err)
	}
	kimiMsgs := gjson.GetBytes(kimiNorm, "messages").Array()
	if len(kimiMsgs) != 1 || !strings.Contains(kimiMsgs[0].Get("content").String(), "part 1") || !strings.Contains(kimiMsgs[0].Get("content").String(), "part 2") {
		t.Fatalf("expected Kimi to merge adjacent user messages after dropping empty assistant: %s", kimiNorm)
	}

	// 4. WorkBuddy maps only leading developer->system and mid-conversation developer->user.
	wbRaw := []byte(`{"model":"m","messages":[{"role":"developer","content":"lead"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"},{"role":"developer","content":"mid"},{"role":"user","content":"u2"}]}`)
	wbNorm, err := sanitizeWorkBuddyPayload(wbRaw)
	if err != nil {
		t.Fatalf("sanitizeWorkBuddyPayload: %v", err)
	}
	wbMsgs := gjson.GetBytes(wbNorm, "messages").Array()
	if len(wbMsgs) != 5 || wbMsgs[0].Get("role").String() != "system" || wbMsgs[3].Get("role").String() != "user" {
		t.Fatalf("expected leading developer->system and mid developer->user in WorkBuddy: %s", wbNorm)
	}
}

func TestBatch3CodexOAuthPassthroughInstructionsAndImageNormalization(t *testing.T) {
	pngB64 := strings.TrimPrefix(testPNGDataURI(t), "data:image/png;base64,")
	var capturedBody []byte
	var capturedSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		capturedSession = r.Header.Get("Session-Id")
		if capturedSession == "" {
			capturedSession = r.Header.Get("Session_id")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer upstream.Close()

	svc := newCodexResponsesService("k", upstream.URL, []runtimeModelRoute{{Name: "gpt-5", Alias: "gpt-5"}}, &codexStaticAuthorizer{token: "tok"}, NewUsageTracker())
	// Exercise Codex OAuth passthrough normalization helper directly as well.
	payload := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"message","role":"system","content":"top sys"},
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"DATA:IMAGE/JPG;BASE64,` + pngB64 + `"}]},
			{"type":"message","role":"system","content":"mid sys"}
		],
		"client_metadata":{"session_id":"client-keep-1"}
	}`)
	var payloadMap map[string]any
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		t.Fatal(err)
	}
	normalizePassthroughCodexOAuthPayload(payloadMap)
	normalizePassthroughResponsesImages(payloadMap)
	out, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatal(err)
	}
	parsed := gjson.ParseBytes(out)
	if parsed.Get("instructions").String() != "top sys" {
		t.Fatalf("expected leading system hoisted to instructions: %s", out)
	}
	if parsed.Get("input.#").Int() != 2 || parsed.Get("input.1.role").String() != "developer" {
		t.Fatalf("expected mid system rewritten to developer: %s", out)
	}
	imgURL := parsed.Get("input.0.content.0.image_url").String()
	if !strings.HasPrefix(imgURL, "data:image/jpeg;base64,") {
		t.Fatalf("expected DATA:IMAGE/JPG normalized to data:image/jpeg;base64, got %q", imgURL)
	}
	metaOut, err := svc.addClientMetadata(out, "fallback-sess")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(metaOut, "client_metadata.session_id").String() != "client-keep-1" {
		t.Fatalf("addClientMetadata overwrote existing session_id: %s", metaOut)
	}

	// Verify responsesNeedFlattening catches hosted items and non-standard message images.
	if !responsesNeedFlattening(map[string]any{"input": []any{map[string]any{"type": "web_search_call", "id": "ws_1"}}}) {
		t.Fatal("expected hosted web_search_call to trigger flattening check")
	}
	if !responsesNeedFlattening(map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "DATA:image/png;base64,abc"}}}}}) {
		t.Fatal("expected uppercase DATA: image in message.content to trigger flattening check")
	}

	// Verify extractClientSessionID reads unified headers.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("X-Grok-Conv-Id", "conv-grok-99")
	if got := extractClientSessionID(req); got != "conv-grok-99" {
		t.Fatalf("extractClientSessionID = %q, want conv-grok-99", got)
	}
	rr := httptest.NewRecorder()
	svc.handleResponses(rr, req)
	if rr.Code != http.StatusOK || capturedSession != "conv-grok-99" || len(capturedBody) == 0 {
		t.Fatalf("expected 200 and forwarded session conv-grok-99, got status=%d session=%q", rr.Code, capturedSession)
	}
}

func TestBatch3AnthropicTopologyMultimodalAndCitations(t *testing.T) {
	pngURI := testPNGDataURI(t)
	// 1. Mid-conversation developer, trailing assistant, whitespace text skip, PDF + text document.
	raw := `{
		"model":"claude-sonnet-4-5",
		"instructions":"top",
		"reasoning":{"effort":"high"},
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"   "},
				{"type":"input_text","text":"read this"},
				{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"},
				{"type":"input_file","filename":"notes.txt","file_data":"data:text/plain;base64,` + base64.StdEncoding.EncodeToString([]byte("hello text")) + `"},
				{"type":"input_image","image_url":"` + pngURI + `"}
			]},
			{"type":"message","role":"developer","content":"be brief"},
			{"type":"message","role":"assistant","content":"draft answer"}
		]
	}`
	converted, err := convertResponsesToAnthropicMessages([]byte(raw))
	if err != nil {
		t.Fatalf("convertResponsesToAnthropicMessages failed: %v", err)
	}
	g := gjson.ParseBytes(converted.body)
	msgs := g.Get("messages").Array()
	// user (merged with mid-dev) -> assistant -> synthetic user("Continue.")
	if len(msgs) != 3 || msgs[2].Get("role").String() != "user" || msgs[2].Get("content.0.text").String() != "Continue." {
		t.Fatalf("expected trailing assistant followed by synthetic Continue., got: %s", converted.body)
	}
	u0 := msgs[0].Get("content").Array()
	hasPDF, hasPlainDoc, hasMidDev, hasWhitespace := false, false, false, false
	for _, b := range u0 {
		if b.Get("type").String() == "document" && b.Get("source.media_type").String() == "application/pdf" {
			hasPDF = true
		}
		if b.Get("type").String() == "document" && b.Get("source.type").String() == "text" && b.Get("source.data").String() == "hello text" {
			hasPlainDoc = true
		}
		if b.Get("type").String() == "text" && strings.Contains(b.Get("text").String(), "[Developer instruction]") {
			hasMidDev = true
		}
		if b.Get("type").String() == "text" && strings.TrimSpace(b.Get("text").String()) == "" {
			hasWhitespace = true
		}
	}
	if !hasPDF || !hasPlainDoc || !hasMidDev || hasWhitespace {
		t.Fatalf("unexpected Anthropic content blocks (pdf=%v plain=%v midDev=%v ws=%v): %s", hasPDF, hasPlainDoc, hasMidDev, hasWhitespace, msgs[0].Raw)
	}

	// 2. Reject audio, image file_id, and unsupported BMP image on Anthropic.
	if _, err := convertResponsesToAnthropicMessages([]byte(`{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_audio","data":"UklGRg==","format":"wav"}]}]}`)); err == nil {
		t.Fatal("expected Anthropic converter to reject input_audio")
	}
	if _, err := convertResponsesToAnthropicMessages([]byte(`{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`)); err == nil {
		t.Fatal("expected Anthropic converter to reject image file_id")
	}
	bmpB64 := base64.StdEncoding.EncodeToString([]byte("BM\x00\x00\x00\x00"))
	if _, err := convertResponsesToAnthropicMessages([]byte(`{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/bmp;base64,` + bmpB64 + `"}]}]}`)); err == nil {
		t.Fatal("expected Anthropic converter to reject BMP image")
	}

	// 3. anthropicThinkingReplayable scans multi-step tool loops (including user turns with tool_result + text).
	loopMsgs := []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "go"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "c1", "content": "r1"}, {"type": "text", "text": "note"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "thinking", "thinking": "t", "signature": "s"}, {"type": "tool_use", "id": "c2", "name": "f", "input": map[string]any{}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "c2", "content": "r2"}}},
	}
	if anthropicThinkingReplayable(loopMsgs) {
		t.Fatal("expected unsigned earlier assistant in same tool loop to disable thinking replay")
	}

	// 4. Anthropic citations_delta converts to Responses annotations + annotation.added event.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Cited fact."}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com","title":"Example","cited_text":"Cited fact."}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	assembler := newResponseAssembler(&adapterRequest{clientModel: "claude-sonnet-4-5", stream: true}, rr)
	if err := processAnthropicMessagesStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatalf("processAnthropicMessagesStream: %v", err)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "response.output_text.annotation.added") || !strings.Contains(body, `"url":"https://example.com"`) {
		t.Fatalf("expected citation converted to Responses annotation event, got:\n%s", body)
	}
}

func TestBatch3KiroAndQoderTopologyAndMultimodal(t *testing.T) {
	// 1. Kiro GIF decoding works via _ "image/gif" and corrects misdeclared format.
	gifB64 := testGIFBase64(t)
	gifObj := map[string]any{"format": "png", "source": map[string]any{"bytes": gifB64}}
	_, corrected := processKiroImage(gifObj)
	if !corrected || gifObj["format"] != "gif" {
		t.Fatalf("expected GIF to be decoded and format corrected, got corrected=%v format=%v", corrected, gifObj["format"])
	}

	// 2. Kiro normal-size request cleans empty assistant turns, handles leading assistant with systemText,
	// preserves trailing assistant with "Continue.", and binds session fallback.
	kiroReq := `{
		"model":"claude-sonnet-4-5",
		"instructions":"sys rules",
		"input":[
			{"type":"message","role":"assistant","content":"leading assistant"},
			{"type":"message","role":"user","content":"next user"},
			{"type":"message","role":"developer","content":"mid dev"},
			{"type":"message","role":"assistant","content":"trailing assistant"}
		]
	}`
	kiroConv, err := convertResponsesToKiroWithSession([]byte(kiroReq), "sess-http-1")
	if err != nil {
		t.Fatalf("convertResponsesToKiroWithSession failed: %v", err)
	}
	kiroBytes, _ := json.Marshal(kiroConv.body)
	kg := gjson.ParseBytes(kiroBytes)
	kiroConv2, err := convertResponsesToKiroWithSession([]byte(kiroReq), "sess-http-2")
	if err != nil {
		t.Fatal(err)
	}
	kiroBytes2, _ := json.Marshal(kiroConv2.body)
	convID1 := kg.Get("conversationState.conversationId").String()
	convID2 := gjson.GetBytes(kiroBytes2, "conversationState.conversationId").String()
	if convID1 == "" || convID1 != kiroConversationID("sess-http-1") || convID1 == convID2 {
		t.Fatalf("expected deterministic HTTP session conversationId, got id1=%q id2=%q", convID1, convID2)
	}
	if kg.Get("conversationState.currentMessage.userInputMessage.content").String() != "Continue." {
		t.Fatalf("expected trailing assistant preserved with Continue. currentMessage: %s", kiroBytes)
	}
	if !strings.Contains(string(kiroBytes), "trailing assistant") || !strings.Contains(string(kiroBytes), "mid dev") {
		t.Fatalf("expected trailing assistant and mid dev retained in Kiro history: %s", kiroBytes)
	}

	// 3. Kiro explicitly rejects remote image URL, input_file, and input_audio with 400.
	for name, payload := range map[string]string{
		"remote_image": `{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`,
		"input_file":   `{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}]}]}`,
		"input_audio":  `{"model":"claude-sonnet-4-5","input":[{"type":"message","role":"user","content":[{"type":"input_audio","data":"UklGRg==","format":"wav"}]}]}`,
	} {
		if _, err := convertResponsesToKiro([]byte(payload)); err == nil {
			t.Fatalf("expected Kiro converter to reject %s", name)
		}
	}

	// 4. Qoder flattens multi-part pure-text user to string, skips empty reasoning, keeps mid developer,
	// rejects input_file/input_audio, and inherits HTTP session ID.
	qCatalog := []qoderModel{{ID: "qoder-model", Name: "qoder-model", ContextWindow: 128000, MaxTokens: 8192}}
	qoderReq := `{
		"model":"qoder-model",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"line 1"},{"type":"input_text","text":"line 2"}]},
			{"type":"reasoning","id":"rs_empty","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"},{"type":"output_text","text":"a2"}]},
			{"type":"message","role":"developer","content":"mid dev"}
		]
	}`
	qConv, err := convertResponsesToQoderWithSession([]byte(qoderReq), qCatalog, "qoder-sess-42")
	if err != nil {
		t.Fatalf("convertResponsesToQoderWithSession failed: %v", err)
	}
	qBytes, _ := json.Marshal(qConv.body)
	qg := gjson.ParseBytes(qBytes)
	if !strings.HasSuffix(qg.Get("session_id").String(), "-qoder-sess-42") {
		t.Fatalf("expected Qoder session fallback suffix -qoder-sess-42, got %q (body=%s)", qg.Get("session_id").String(), qBytes)
	}
	qMsgs := qg.Get("messages").Array()
	if qMsgs[0].Get("content").Type != gjson.String || qMsgs[0].Get("content").String() != "line 1\nline 2" {
		t.Fatalf("expected Qoder pure-text multi-part user flattened to string: %s", qMsgs[0].Raw)
	}
	if strings.Contains(qMsgs[1].Get("content").String(), "<thinking></thinking>") || qMsgs[1].Get("content").String() != "a1\na2" {
		t.Fatalf("expected Qoder empty reasoning omitted and assistant joined with newline: %s", qMsgs[1].Raw)
	}
	if _, err := convertResponsesToQoder([]byte(`{"model":"qoder-model","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}]}]}`), qCatalog); err == nil {
		t.Fatal("expected Qoder converter to reject input_file")
	}
}

func TestBatch3CursorParkedAbandonmentHistoryOrderAndSessionIsolation(t *testing.T) {
	pngURI := testPNGDataURI(t)
	// 1. History order puts TOOL_RESULT before USER, and retains historical user images up to 4.
	reqBody := `{
		"model":"cursor-default",
		"instructions":"sys A",
		"tools":[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"v":{"type":"string"}}}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"},{"type":"input_image","image_url":"` + pngURI + `"}]},
			{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"v\":\"1\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"tool-out"},
			{"type":"message","role":"user","content":"user-after-tool"}
		]
	}`
	conv, err := convertResponsesToCursor([]byte(reqBody))
	if err != nil {
		t.Fatalf("convertResponsesToCursor failed: %v", err)
	}
	if len(conv.images) != 1 {
		t.Fatalf("expected historical user image retained, got %d images", len(conv.images))
	}
	if !strings.Contains(conv.current, "user-after-tool") {
		t.Fatalf("expected current user text captured for parked-stream abandonment check, got %q", conv.current)
	}
	toolIdx := strings.Index(conv.userText, "TOOL_RESULT:")
	userIdx := strings.Index(conv.userText, "user-after-tool")
	if toolIdx < 0 || userIdx < 0 || toolIdx > userIdx {
		t.Fatalf("expected TOOL_RESULT before user-after-tool in Cursor userText, got:\n%s", conv.userText)
	}

	// 2. Concurrent headerless conversations produce distinct anonSessionKey values.
	convA, err := convertResponsesToCursor([]byte(`{"model":"cursor-default","input":"alpha conversation"}`))
	if err != nil {
		t.Fatal(err)
	}
	convB, err := convertResponsesToCursor([]byte(`{"model":"cursor-default","input":"beta conversation"}`))
	if err != nil {
		t.Fatal(err)
	}
	if convA.anonSessionKey == "" || convB.anonSessionKey == "" || convA.anonSessionKey == convB.anonSessionKey {
		t.Fatalf("expected distinct non-empty anonymous session keys, got %q and %q", convA.anonSessionKey, convB.anonSessionKey)
	}

	// 3. System prompt hash changes when instructions change.
	convSysB, err := convertResponsesToCursor([]byte(`{"model":"cursor-default","instructions":"sys B","input":"alpha conversation"}`))
	if err != nil {
		t.Fatal(err)
	}
	if conv.systemHash == convSysB.systemHash {
		t.Fatal("expected different systemHash when instructions change")
	}
}

func TestBatch3MultimodalDataURIHardeningAndAnnotationsChatFront(t *testing.T) {
	// 1. Data URI hardening: reject empty base64 and file:// URLs; accept uppercase DATA:.
	if _, err := parseResponsesRequest([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]}]}`)); err == nil {
		t.Fatal("expected empty base64 data URI to be rejected")
	}
	if _, err := parseResponsesRequest([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"file:///etc/passwd"}]}]}`)); err == nil {
		t.Fatal("expected file:// image URL to be rejected")
	}
	pngB64 := strings.TrimPrefix(testPNGDataURI(t), "data:image/png;base64,")
	req, err := parseResponsesRequest([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"DATA:IMAGE/PNG;BASE64,` + pngB64 + `"},{"type":"input_image","file_id":"file_img_1"}]}]}`))
	if err != nil {
		t.Fatalf("expected uppercase DATA: and file_id to parse, got %v", err)
	}
	if len(req.Turns) != 1 || len(req.Turns[0].Parts) != 2 || req.Turns[0].Parts[0].MediaType != "image/png" || req.Turns[0].Parts[1].FileID != "file_img_1" {
		t.Fatalf("unexpected parsed image parts: %+v", req.Turns)
	}

	// 2. Chat Completions stream & non-stream annotations propagate to Responses and chat_front.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"See source.\",\"annotations\":[{\"type\":\"url_citation\",\"url_citation\":{\"url\":\"https://docs.example.com\",\"title\":\"Docs\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"See source.","annotations":[{"type":"url_citation","url_citation":{"url":"https://docs.example.com","title":"Docs"}}]},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	rt, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	// Non-stream via /v1/chat/completions preserves annotations on message.annotations.
	chatReq, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/chat/completions", strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	chatReq.Header.Set("Authorization", "Bearer "+rt.APIKey())
	chatResp, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatBytes, _ := io.ReadAll(chatResp.Body)
	chatResp.Body.Close()
	if chatResp.StatusCode != http.StatusOK || gjson.GetBytes(chatBytes, "choices.0.message.annotations.0.url_citation.url").String() != "https://docs.example.com" {
		t.Fatalf("expected chat_front non-stream annotations, status=%d body=%s", chatResp.StatusCode, chatBytes)
	}
}

func TestBatch3LocalCompactionStoreFalseItemReferenceAndSessions(t *testing.T) {
	var mu sync.Mutex
	var upstreamPaths []string
	var lastMessages string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		lastMessages = string(b)
		mu.Unlock()
		if gjson.GetBytes(b, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"summary-text\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"summary-text"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	rt, err := StartOpenAIChatAPI(context.Background(), upstream.URL+"/v1", "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(path, payload string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// 1. store:false skips caching response ID.
	resp, b := post("/responses", `{"model":"m","stream":false,"store":false,"input":"ephemeral"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	ephemeralID := gjson.GetBytes(b, "id").String()
	resp, b = post("/responses", `{"model":"m","stream":false,"previous_response_id":"`+ephemeralID+`","input":"follow"}`)
	if resp.StatusCode != http.StatusBadRequest || gjson.GetBytes(b, "error.code").String() != "previous_response_not_found" {
		t.Fatalf("store:false response must not be cached, got status=%d body=%s", resp.StatusCode, b)
	}

	// 2. Stored response supports item_reference expansion and /responses/compact local summary.
	resp, b = post("/responses", `{"model":"m","stream":false,"input":"remember goal alpha"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	storedID := gjson.GetBytes(b, "id").String()
	msgItemID := gjson.GetBytes(b, "output.0.id").String()
	if msgItemID == "" {
		t.Fatalf("expected output item id in response: %s", b)
	}
	resp, b = post("/responses", `{"model":"m","stream":false,"input":[{"type":"item_reference","id":"`+msgItemID+`"},{"type":"message","role":"user","content":"next"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected item_reference expansion to succeed, got %d: %s", resp.StatusCode, b)
	}
	mu.Lock()
	sawExpanded := strings.Contains(lastMessages, "summary-text")
	mu.Unlock()
	if !sawExpanded {
		t.Fatalf("expected expanded item_reference content in upstream request, got: %s", lastMessages)
	}

	// Unknown item_reference returns 400 item_not_found.
	resp, b = post("/responses", `{"model":"m","stream":false,"input":[{"type":"item_reference","id":"msg_missing"}]}`)
	if resp.StatusCode != http.StatusBadRequest || gjson.GetBytes(b, "error.code").String() != "item_not_found" {
		t.Fatalf("expected 400 item_not_found, got status=%d body=%s", resp.StatusCode, b)
	}

	// /responses/compact returns object: "response.compaction" via local summary and expands previous_response_id.
	resp, b = post("/responses/compact", `{"model":"m","previous_response_id":"`+storedID+`","input":"wrap up"}`)
	if resp.StatusCode != http.StatusOK || gjson.GetBytes(b, "object").String() != "response.compaction" {
		t.Fatalf("expected local compaction response.compaction, got status=%d body=%s", resp.StatusCode, b)
	}
	var compactObj map[string]any
	if err := json.Unmarshal(b, &compactObj); err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(b, "output.0.type").String() != "message" || !strings.Contains(gjson.GetBytes(b, "output.0.content.0.text").String(), "summary-text") {
		t.Fatalf("unexpected compaction output: %s", b)
	}
}
