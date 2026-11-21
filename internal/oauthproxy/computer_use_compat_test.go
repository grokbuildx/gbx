package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestImageDetailPreservedAcrossConverters(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"model": "vision-model",
		"tools": []any{
			map[string]any{"type": "computer", "display_width_px": 1440, "display_height_px": 900, "environment": "mac"},
		},
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Inspect this screen"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64, "detail": "original"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + onePixelPNGBase64, "detail": "low"}},
				},
			},
			map[string]any{"type": "computer_call", "call_id": "cu_1", "action": map[string]any{"type": "screenshot"}},
			map[string]any{
				"type":        "computer_call_output",
				"call_id":     "cu_1",
				"current_url": "https://example.test/dashboard",
				"output": map[string]any{
					"type":      "computer_screenshot",
					"image_url": "data:image/png;base64," + onePixelPNGBase64,
					"detail":    "original",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := parseResponsesRequest(raw)
	if err != nil {
		t.Fatalf("parseResponsesRequest failed: %v", err)
	}
	if len(parsed.Turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(parsed.Turns))
	}
	if got := parsed.Turns[0].Parts[1].Detail; got != "original" {
		t.Fatalf("first image detail = %q, want original", got)
	}
	if got := parsed.Turns[0].Parts[2].Detail; got != "low" {
		t.Fatalf("second image detail = %q, want low", got)
	}
	toolResult := parsed.Turns[2].Parts[0]
	if !strings.Contains(toolResult.outputText(), "Current URL: https://example.test/dashboard") {
		t.Fatalf("computer_call_output lost current_url: %q", toolResult.outputText())
	}
	if imgs := toolResult.outputImages(); len(imgs) != 1 || imgs[0].Detail != "original" {
		t.Fatalf("computer_call_output screenshot detail = %+v, want original", imgs)
	}

	// 1. OpenAI Chat Completions maps "original" -> "high" and keeps "low".
	chatConverted, err := convertResponsesToChatCompletions(raw)
	if err != nil {
		t.Fatalf("convertResponsesToChatCompletions failed: %v", err)
	}
	var chatBody map[string]any
	if err := json.Unmarshal(chatConverted.body, &chatBody); err != nil {
		t.Fatal(err)
	}
	chatEncoded := string(chatConverted.body)
	if strings.Contains(chatEncoded, `"detail":"original"`) {
		t.Fatalf("chat converter emitted non-Chat detail=original: %s", chatEncoded)
	}
	if strings.Count(chatEncoded, `"detail":"high"`) != 2 || strings.Count(chatEncoded, `"detail":"low"`) != 1 {
		t.Fatalf("unexpected detail counts in chat body: %s", chatEncoded)
	}

	// 2. Qoder converter maps "original" -> "high" and keeps "low".
	qoderConverted, err := convertResponsesToQoder(raw, []qoderModel{{ID: "vision-model", Name: "vision-model", Vision: true}})
	if err != nil {
		t.Fatalf("convertResponsesToQoder failed: %v", err)
	}
	qoderJSON, _ := json.Marshal(qoderConverted.body["messages"])
	qoderStr := string(qoderJSON)
	if strings.Contains(qoderStr, `"detail":"original"`) {
		t.Fatalf("qoder converter emitted detail=original: %s", qoderStr)
	}
	if strings.Count(qoderStr, `"detail":"high"`) != 2 || strings.Count(qoderStr, `"detail":"low"`) != 1 {
		t.Fatalf("unexpected detail counts in qoder messages: %s", qoderStr)
	}

	// 3. Responses converter keeps exact "original", and normalizeXaiImageDetail maps "original" -> "high".
	respConverted, err := convertResponsesForUpstream(raw, "sess_1")
	if err != nil {
		t.Fatalf("convertResponsesForUpstream failed: %v", err)
	}
	if strings.Count(string(respConverted.body), `"detail":"original"`) != 2 {
		t.Fatalf("convertResponsesForUpstream did not preserve detail=original: %s", respConverted.body)
	}
	var xaiBody map[string]any
	if err := json.Unmarshal(respConverted.body, &xaiBody); err != nil {
		t.Fatal(err)
	}
	normalizeXaiImageDetail(xaiBody)
	xaiEncoded, _ := json.Marshal(xaiBody)
	if strings.Contains(string(xaiEncoded), `"detail":"original"`) {
		t.Fatalf("normalizeXaiImageDetail left detail=original: %s", xaiEncoded)
	}
	if strings.Count(string(xaiEncoded), `"detail":"high"`) != 2 || strings.Count(string(xaiEncoded), `"detail":"low"`) != 1 {
		t.Fatalf("unexpected xai detail mapping: %s", xaiEncoded)
	}
}

func TestBuiltinToolChoiceWithoutName(t *testing.T) {
	tools := []any{
		map[string]any{"type": "computer_use_preview", "display_width": 1280, "display_height": 720, "environment": "linux"},
		map[string]any{"type": "shell"},
		map[string]any{"type": "local_shell"},
		map[string]any{"type": "apply_patch"},
		map[string]any{"type": "tool_search", "execution": "client"},
	}

	cases := []struct {
		choice   map[string]any
		wantName string
	}{
		{map[string]any{"type": "computer"}, "computer"},
		{map[string]any{"type": "computer_use_preview"}, "computer"},
		{map[string]any{"type": "shell"}, "shell"},
		{map[string]any{"type": "local_shell"}, "local_shell"},
		{map[string]any{"type": "apply_patch"}, "apply_patch"},
		{map[string]any{"type": "tool_search"}, "tool_search"},
	}
	for _, tc := range cases {
		raw, _ := json.Marshal(map[string]any{
			"model":       "test-model",
			"tools":       tools,
			"tool_choice": tc.choice,
			"input":       "go",
		})
		parsed, err := parseResponsesRequest(raw)
		if err != nil {
			t.Fatalf("tool_choice=%v failed: %v", tc.choice, err)
		}
		if parsed.ToolChoice.Mode != toolChoiceTool || parsed.ToolChoice.Name != tc.wantName {
			t.Fatalf("tool_choice=%v -> %+v, want tool:%s", tc.choice, parsed.ToolChoice, tc.wantName)
		}
	}

	undeclaredRaw, _ := json.Marshal(map[string]any{
		"model":       "test-model",
		"tools":       []any{map[string]any{"type": "shell"}},
		"tool_choice": map[string]any{"type": "computer"},
		"input":       "go",
	})
	if _, err := parseResponsesRequest(undeclaredRaw); err == nil || !strings.Contains(err.Error(), "tool_choice is not in tools") {
		t.Fatalf("expected undeclared computer tool_choice error, got: %v", err)
	}
}

func TestComputerToolSchemaBatchActionsValidationAndTruncation(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model":      "test-model",
		"truncation": "auto",
		"tools": []any{
			map[string]any{"type": "computer", "display_width_px": 1920, "display_height_px": 1080, "environment": "browser"},
		},
		"input": "batch click and type",
	})

	parsed, err := parseResponsesRequest(raw)
	if err != nil {
		t.Fatalf("parseResponsesRequest failed: %v", err)
	}
	if parsed.Truncation != "auto" {
		t.Fatalf("Truncation = %q, want auto", parsed.Truncation)
	}
	if len(parsed.Tools) != 1 || !strings.Contains(parsed.Tools[0].Description, "display=1920x1080") {
		t.Fatalf("computer description missed display_*_px: %+v", parsed.Tools)
	}
	var schema map[string]any
	if err := json.Unmarshal(parsed.Tools[0].Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if _, hasRequired := schema["required"]; hasRequired {
		t.Fatalf("computer schema should allow batched actions without top-level required type: %+v", schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if props["actions"] == nil || props["ms"] == nil {
		t.Fatalf("computer schema missing actions or ms properties: %+v", props)
	}

	converted, err := convertResponsesForUpstream(raw, "sess_trunc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted.body), `"truncation":"auto"`) {
		t.Fatalf("convertResponsesForUpstream dropped truncation: %s", converted.body)
	}

	adapter := parsed.adapter("test-model")
	// 1. Batched actions with extra empty type field still emits top-level actions.
	assembler := newResponseAssembler(&adapter, nil)
	if err := assembler.addToolUse("batch_1", "computer", `{"actions":[{"type":"click","x":10,"y":20},{"type":"type","text":"hello"}],"type":""}`); err != nil {
		t.Fatalf("valid batched computer_call rejected: %v", err)
	}
	items := assembler.outputItems()
	if len(items) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(items))
	}
	if _, hasAction := items[0]["action"]; hasAction {
		t.Fatalf("batched computer_call should not emit singular action: %+v", items[0])
	}
	actions, ok := items[0]["actions"].([]any)
	if !ok || len(actions) != 2 {
		t.Fatalf("expected 2 batched actions, got: %+v", items[0])
	}

	// 2. Empty or malformed computer_call payloads are rejected instead of emitting broken action: {}.
	for _, invalid := range []string{`{}`, `{"actions":[]}`, `{"actions":[{"x":10}]}`, `{"action":{}}`} {
		badAssembler := newResponseAssembler(&adapter, nil)
		if err := badAssembler.addToolUse("bad_1", "computer", invalid); err == nil {
			t.Fatalf("expected invalid computer payload %s to be rejected", invalid)
		}
	}
}

func TestResponsesNeedFlatteningDetectsFunctionCallOutputScreenshots(t *testing.T) {
	need := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "fn_1",
				"output": []any{
					map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;base64," + onePixelPNGBase64},
				},
			},
		},
	}
	if !responsesNeedFlattening(need) {
		t.Fatal("expected computer_screenshot inside function_call_output to trigger flattening")
	}

	mimeLess := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "fn_2",
				"output": []any{
					map[string]any{"type": "input_image", "image_url": "data:;base64," + onePixelPNGBase64},
				},
			},
		},
	}
	if !responsesNeedFlattening(mimeLess) {
		t.Fatal("expected MIME-less data URL inside function_call_output to trigger flattening")
	}

	clean := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "fn_3",
				"output": []any{
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64},
				},
			},
		},
	}
	if responsesNeedFlattening(clean) {
		t.Fatal("standard input_image in function_call_output should not force flattening")
	}
}

func TestPruneHistoricalToolScreenshotsBoundsMultiTurnLoop(t *testing.T) {
	items := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "Reference screenshot"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64},
			},
		},
	}
	for step := 1; step <= 7; step++ {
		callID := fmt.Sprintf("cu_%d", step)
		items = append(items,
			map[string]any{"type": "computer_call", "call_id": callID, "action": map[string]any{"type": "screenshot"}},
			map[string]any{
				"type":    "computer_call_output",
				"call_id": callID,
				"output": map[string]any{
					"type":      "computer_screenshot",
					"image_url": "data:image/png;base64," + onePixelPNGBase64,
				},
			},
		)
	}
	raw, _ := json.Marshal(map[string]any{
		"model": "vision-model",
		"tools": []any{map[string]any{"type": "computer"}},
		"input": items,
	})

	parsed, err := parseResponsesRequest(raw)
	if err != nil {
		t.Fatalf("parseResponsesRequest failed: %v", err)
	}

	// Direct user-message image must remain intact.
	if len(parsed.Turns[0].Parts) < 2 || parsed.Turns[0].Parts[1].Kind != partImage {
		t.Fatalf("direct user attachment was pruned: %+v", parsed.Turns[0].Parts)
	}

	keptToolImages := 0
	omittedPlaceholders := 0
	for turnIdx, turn := range parsed.Turns {
		for _, part := range turn.Parts {
			if part.Kind != partToolResult {
				continue
			}
			imgs := part.outputImages()
			keptToolImages += len(imgs)
			if strings.Contains(part.outputText(), "[earlier screenshot omitted]") {
				omittedPlaceholders++
			}
			// Latest turn (step 7) must always keep its screenshot.
			if turnIdx == len(parsed.Turns)-1 && len(imgs) != 1 {
				t.Fatalf("latest turn tool screenshot was pruned: %+v", part)
			}
		}
	}
	// 1 latest turn + 4 newest historical turns = 5 retained; 2 oldest pruned.
	if keptToolImages != maxHistoricalToolScreenshots+1 || omittedPlaceholders != 2 {
		t.Fatalf("keptToolImages=%d omittedPlaceholders=%d, want 5 and 2", keptToolImages, omittedPlaceholders)
	}
}

func TestResponsesHistoryCachePrunesOldScreenshotsAndDemotesComputerCalls(t *testing.T) {
	bigShot := "data:image/png;base64," + strings.Repeat("A", 1024)
	items := make([]any, 0, 16)
	for step := 1; step <= 8; step++ {
		callID := fmt.Sprintf("cu_%d", step)
		items = append(items,
			map[string]any{"type": "computer_call", "call_id": callID, "action": map[string]any{"type": "click", "x": step, "y": step}},
			map[string]any{
				"type":        "computer_call_output",
				"call_id":     callID,
				"current_url": fmt.Sprintf("https://app.test/step/%d", step),
				"output":      map[string]any{"type": "computer_screenshot", "image_url": bigShot},
			},
		)
	}

	cache := newResponsesHistoryCache()
	now := time.Now()
	cache.put("resp_multi", items, now)

	got, ok := cache.get("resp_multi", now)
	if !ok {
		t.Fatal("expected multi-screenshot history to be cached")
	}
	// Caller input must never be mutated in place.
	if firstOut, _ := items[1].(map[string]any); stringValue(firstOut["type"]) != "computer_call_output" {
		t.Fatalf("caller slice was mutated in place: %+v", firstOut)
	}

	encoded, _ := json.Marshal(got)
	text := string(encoded)
	if strings.Count(text, bigShot) != responsesHistoryMaxRetainedImages {
		t.Fatalf("retained screenshots = %d, want %d", strings.Count(text, bigShot), responsesHistoryMaxRetainedImages)
	}
	if strings.Count(text, "[historical screenshot omitted]") != 2 {
		t.Fatalf("expected 2 omitted historical screenshots, got text=%s", text)
	}
	// Oldest 2 computer_call + computer_call_output pairs must both be demoted to plain messages.
	if firstItem, _ := got[0].(map[string]any); stringValue(firstItem["type"]) != "message" || stringValue(firstItem["role"]) != "assistant" {
		t.Fatalf("paired computer_call was not demoted to assistant message: %+v", firstItem)
	}
	if secondItem, _ := got[1].(map[string]any); stringValue(secondItem["type"]) != "message" || stringValue(secondItem["role"]) != "user" {
		t.Fatalf("pruned computer_call_output was not demoted to user message: %+v", secondItem)
	}
	if !strings.Contains(text, "Current URL: https://app.test/step/1") {
		t.Fatalf("demoted computer_call_output lost current_url: %s", text)
	}

	// Under a tight byte limit, progressive fallback prunes further instead of silently dropping the entry.
	tight, ok := marshalBoundedResponsesHistory(items, 4096)
	if !ok || len(tight) > 4096 {
		t.Fatalf("expected tight bounded history to succeed <= 4096 bytes, ok=%v len=%d", ok, len(tight))
	}
}

func TestXaiAPIKeyRuntimeFlattensToolsAndMapsOriginalDetail(t *testing.T) {
	var seenBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &seenBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_xai_1\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_exec_1\",\"name\":\"exec\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"item_id\":\"fc_1\",\"arguments\":\"{\\\"input\\\":\\\"await tools.screenshot({})\\\"}\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_exec_1\",\"name\":\"exec\",\"arguments\":\"{\\\"input\\\":\\\"await tools.screenshot({})\\\"}\",\"status\":\"completed\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_xai_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_exec_1\",\"name\":\"exec\",\"arguments\":\"{\\\"input\\\":\\\"await tools.screenshot({})\\\"}\",\"status\":\"completed\"}]}}\n\n")
	}))
	defer upstream.Close()

	rt, err := startOpenAIResponsesAPIWithProvider(context.Background(), upstream.URL+"/v1", "fake-xai-key", "grok-4-20", "xai")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	payload, _ := json.Marshal(map[string]any{
		"model":  "grok-4-20",
		"stream": false,
		"tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "JS REPL", "format": map[string]any{"type": "text", "syntax": "javascript", "definition": "js"}},
		},
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "click ok"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64, "detail": "original"},
				},
			},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+rt.APIKey())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, respBytes)
	}
	if _, hasClientMeta := seenBody["client_metadata"]; hasClientMeta {
		t.Fatalf("xAI API-key runtime should omit Codex client_metadata: %+v", seenBody)
	}
	seenJSON, _ := json.Marshal(seenBody)
	if strings.Contains(string(seenJSON), `"detail":"original"`) || !strings.Contains(string(seenJSON), `"detail":"high"`) {
		t.Fatalf("xAI API-key runtime did not map image detail original->high: %s", seenJSON)
	}
	if !strings.Contains(string(seenJSON), `"type":"function","name":"exec"`) && !strings.Contains(string(seenJSON), `"name":"exec"`) {
		t.Fatalf("xAI API-key runtime did not flatten custom exec tool: %s", seenJSON)
	}
	if !strings.Contains(string(respBytes), `"type":"custom_tool_call"`) {
		t.Fatalf("expected restored custom_tool_call in client response, got: %s", respBytes)
	}
}
