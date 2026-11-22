package oauthproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestToolCallMaxTokensTruncationEmitsIncompleteAcrossBackends(t *testing.T) {
	reqRaw, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"tools": []any{map[string]any{"type": "function", "name": "write_file", "parameters": map[string]any{"type": "object"}}},
		"input": "write a large file",
	})
	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatal(err)
	}

	assertIncomplete := func(t *testing.T, label string, assembler *responseAssembler) {
		t.Helper()
		resp := assembler.response()
		if resp["status"] != "incomplete" {
			t.Fatalf("%s: status = %v, want incomplete (resp=%+v)", label, resp["status"], resp)
		}
		details, _ := resp["incomplete_details"].(map[string]any)
		if details["reason"] != "max_output_tokens" {
			t.Fatalf("%s: incomplete_details = %+v, want max_output_tokens", label, details)
		}
		if assembler.hasToolUse {
			t.Fatalf("%s: truncated partial tool call should not be emitted", label)
		}
	}

	// 1. OpenAI Chat streaming truncation mid-JSON.
	t.Run("chat_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"Starting write...","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.go\",\"content\":\"unterminated"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		if err := processChatCompletionsStream(strings.NewReader(stream), assembler); err != nil {
			t.Fatalf("chat stream max_tokens failed: %v", err)
		}
		assertIncomplete(t, "chat_stream", assembler)
	})

	// 2. OpenAI Chat non-stream truncation mid-JSON.
	t.Run("chat_non_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		body := `{"choices":[{"message":{"content":"partial","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.go"}}]},"finish_reason":"length"}]}`
		if err := processChatCompletionsNonStream([]byte(body), assembler); err != nil {
			t.Fatalf("chat non-stream max_tokens failed: %v", err)
		}
		assertIncomplete(t, "chat_non_stream", assembler)
	})

	// 3. Qoder streaming truncation mid-JSON.
	t.Run("qoder_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		stream := strings.Join([]string{
			`data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_q1","function":{"name":"write_file","arguments":"{\"path\":\"a.go"}}]},"finish_reason":"length"}]}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		if _, err := processQoderEventStream(context.Background(), strings.NewReader(stream), assembler); err != nil {
			t.Fatalf("qoder stream max_tokens failed: %v", err)
		}
		assertIncomplete(t, "qoder_stream", assembler)
	})

	// 4. Anthropic Messages streaming truncation mid-JSON (content_block_stop precedes message_delta).
	t.Run("anthropic_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		stream := strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"write_file"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\",\"body\":\"cut"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":99}}`,
			`data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"
		if err := processAnthropicMessagesStream(strings.NewReader(stream), assembler); err != nil {
			t.Fatalf("anthropic stream max_tokens failed: %v", err)
		}
		assertIncomplete(t, "anthropic_stream", assembler)
	})

	// 5. Responses streaming truncation (output_item.done status=incomplete + response.incomplete).
	t.Run("responses_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		stream := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_inc","status":"in_progress","output":[]}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_r1","name":"write_file","arguments":"","status":"in_progress"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"path\":\"a.go"}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_r1","name":"write_file","arguments":"{\"path\":\"a.go","status":"incomplete"}}`,
			`data: {"type":"response.incomplete","response":{"id":"resp_inc","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"fc_1","type":"function_call","call_id":"call_r1","name":"write_file","arguments":"{\"path\":\"a.go","status":"incomplete"}]}}`,
		}, "\n\n") + "\n\n"
		if err := processCodexResponsesStream(strings.NewReader(stream), assembler); err != nil {
			t.Fatalf("responses stream max_tokens failed: %v", err)
		}
		assertIncomplete(t, "responses_stream", assembler)
	})

	// 6. Kiro ContentLengthExceededException with an unclosed toolUseEvent accumulator.
	t.Run("kiro_stream", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		if err := assembler.process(&kiroEventFrame{
			headers: map[string]string{":message-type": "event", ":event-type": "toolUseEvent"},
			payload: []byte(`{"name":"write_file","toolUseId":"toolu_k1","input":"{\"path\":\"a.go"}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := assembler.process(&kiroEventFrame{
			headers: map[string]string{":message-type": "exception", ":exception-type": "ContentLengthExceededException"},
			payload: []byte(`{"message":"context length exceeded"}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := assembler.finish(); err != nil {
			t.Fatalf("kiro finish after ContentLengthExceededException failed: %v", err)
		}
		assertIncomplete(t, "kiro_stream", assembler)
	})

	// 7. Normal completion (not max_tokens) with broken JSON MUST still fail closed.
	t.Run("normal_completion_still_rejects_invalid_json", func(t *testing.T) {
		adapter := parsed.adapter("test-model")
		assembler := newResponseAssembler(&adapter, nil)
		stream := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"broken"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
		if err := processChatCompletionsStream(strings.NewReader(stream), assembler); err == nil {
			t.Fatal("expected invalid JSON on normal tool_calls finish_reason to fail closed")
		}
	})
}

func TestChatAndQoderSeparateParallelToolCallsReusingZeroIndex(t *testing.T) {
	reqRaw, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"tools": []any{
			map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "function", "name": "list_dir", "parameters": map[string]any{"type": "object"}},
		},
		"input": "read and list",
	})
	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatal(err)
	}

	// OpenAI Chat stream where both calls omit index (defaulting to 0) and differ only by id.
	chatStream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"list_dir","arguments":"{\"dir\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\".\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	adapter := parsed.adapter("test-model")
	assembler := newResponseAssembler(&adapter, nil)
	if err := processChatCompletionsStream(strings.NewReader(chatStream), assembler); err != nil {
		t.Fatalf("chat parallel zero-index stream failed: %v", err)
	}
	blocks := assembler.contentBlocks()
	if len(blocks) != 2 || blocks[0].ID != "call_a" || blocks[0].Name != "read_file" || blocks[0].Arguments != `{"path":"a.go"}` ||
		blocks[1].ID != "call_b" || blocks[1].Name != "list_dir" || blocks[1].Arguments != `{"dir":"."}` {
		t.Fatalf("unexpected chat blocks: %+v", blocks)
	}

	// Qoder stream with the same omitted/reused index=0 pattern.
	qoderStream := strings.Join([]string{
		`data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"tool_calls":[{"id":"q_1","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}}]}}]}}`,
		`data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"tool_calls":[{"id":"q_2","function":{"name":"list_dir","arguments":"{\"dir\":\"y\"}"}}]},"finish_reason":"tool_calls"}]}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	qAdapter := parsed.adapter("test-model")
	qAssembler := newResponseAssembler(&qAdapter, nil)
	if _, err := processQoderEventStream(context.Background(), strings.NewReader(qoderStream), qAssembler); err != nil {
		t.Fatalf("qoder parallel zero-index stream failed: %v", err)
	}
	qBlocks := qAssembler.contentBlocks()
	if len(qBlocks) != 2 || qBlocks[0].ID != "q_1" || qBlocks[0].Name != "read_file" || qBlocks[1].ID != "q_2" || qBlocks[1].Name != "list_dir" {
		t.Fatalf("unexpected qoder blocks: %+v", qBlocks)
	}
}

func TestCodexResponsesStreamAliasesItemIDAndOutputIndex(t *testing.T) {
	reqRaw, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
		"input": "search",
	})
	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := parsed.adapter("test-model")
	assembler := newResponseAssembler(&adapter, nil)

	// output_item.added omits item.id (only call_id + output_index); subsequent
	// delta introduces item_id alongside output_index.
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_alias","status":"in_progress","output":[]}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"","status":"in_progress"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_a","delta":"{\"q\":\"hello\"}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","status":"completed"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_alias","status":"completed","output":[{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","status":"completed"}]}}`,
	}, "\n\n") + "\n\n"
	if err := processCodexResponsesStream(strings.NewReader(stream), assembler); err != nil {
		t.Fatalf("processCodexResponsesStream failed: %v", err)
	}
	blocks := assembler.contentBlocks()
	if len(blocks) != 1 || blocks[0].ID != "call_a" || blocks[0].Name != "lookup" || blocks[0].Arguments != `{"q":"hello"}` {
		t.Fatalf("arguments lost across item_id/output_index alias: %+v", blocks)
	}
}

func TestChatFrontToolBridgeFixes(t *testing.T) {
	// 1. chatToResponsesRequest handles flat tool_choice, non-string tool content, and image detail.
	chatReq, _ := json.Marshal(map[string]any{
		"model": "gpt-test",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
		"tool_choice": map[string]any{
			"type": "function",
			"name": "lookup",
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "inspect"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + onePixelPNGBase64, "detail": "high"}},
			}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": map[string]any{"status": "ok", "count": 3}},
		},
	})
	respBody, _, err := chatToResponsesRequest(chatReq)
	if err != nil {
		t.Fatalf("chatToResponsesRequest failed: %v", err)
	}
	if got := gjson.GetBytes(respBody, "tool_choice.name").String(); got != "lookup" {
		t.Fatalf("flat tool_choice lost name: %s", respBody)
	}
	if got := gjson.GetBytes(respBody, "input.0.content.1.detail").String(); got != "high" {
		t.Fatalf("chatFrontContent dropped image detail: %s", respBody)
	}
	if got := gjson.GetBytes(respBody, "input.2.output").String(); !strings.Contains(got, `"status":"ok"`) {
		t.Fatalf("non-string role:tool content dropped: %s", respBody)
	}

	// Missing function name in tool_choice must fail validation cleanly.
	badChoice, _ := json.Marshal(map[string]any{
		"model":       "gpt-test",
		"tool_choice": map[string]any{"type": "function"},
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, _, err := chatToResponsesRequest(badChoice); err == nil {
		t.Fatal("expected missing tool_choice.function.name to fail validation")
	}

	// 2. chatFrontWriter recovers arguments from function_call_arguments.done and output_item.done without prior .delta.
	rec := httptest.NewRecorder()
	writer := newChatFrontWriter(rec, "gpt-test", false)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_done_only","name":"lookup","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"q\":\"from-done\"}"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_item_done","name":"lookup","arguments":"{\"q\":\"from-item-done\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	}, "\n\n") + "\n\n"
	if _, err := writer.Write([]byte(sse)); err != nil {
		t.Fatal(err)
	}
	writer.finish()
	out := rec.Body.String()
	if !strings.Contains(out, `\"q\":\"from-done\"`) || !strings.Contains(out, `\"q\":\"from-item-done\"`) {
		t.Fatalf("chatFrontWriter dropped .done/output_item.done arguments: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("chatFrontWriter missed finish_reason=tool_calls: %s", out)
	}
}

func TestCursorPreValidatesExecMCPBeforeOpeningGate(t *testing.T) {
	reqRaw, _ := json.Marshal(map[string]any{
		"model": "cursor-model",
		"tools": []any{map[string]any{"type": "function", "name": "allowed_tool", "parameters": map[string]any{"type": "object"}}},
		"input": "run tool",
	})
	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatal(err)
	}

	// Build a Connect frame carrying one valid exec_mcp and one undeclared exec_mcp.
	var payload []byte
	payload = append(payload, cursorTestExecMCP(1, "e1", "allowed_tool", "c1", nil)...)
	payload = append(payload, cursorTestExecMCP(2, "e2", "undeclared_tool", "c2", nil)...)
	frame := cursorFrameConnect(payload, 0)

	rec := httptest.NewRecorder()
	gate := &cursorGateWriter{response: rec}
	adapter := parsed.adapter("cursor-model")
	assembler := newResponseAssembler(&adapter, gate)
	pr, pw := io.Pipe()
	defer pw.Close()
	session := &cursorSession{
		reader:  bufio.NewReader(bytes.NewReader(frame)),
		writer:  pw,
		pending: make(map[string]cursorPendingCall),
		cancel:  func() {},
	}
	defer pr.Close()

	svc := &cursorService{}
	err = svc.continueTurn(context.Background(), session, &cursorConvertedRequest{adapterRequest: adapter}, assembler, gate, false)
	if err == nil || !strings.Contains(err.Error(), "outside the declared tool list") {
		t.Fatalf("expected undeclared tool rejection, got err=%v", err)
	}
	if gate.opened() || rec.Body.Len() != 0 {
		t.Fatalf("gate opened before validating exec_mcp batch: opened=%v body=%q", gate.opened(), rec.Body.String())
	}
	if assembler.hasToolUse {
		t.Fatal("first exec_mcp escaped before second exec_mcp in the same frame was validated")
	}
}

func TestNormalizeToolCallPairingDemotesOrphansAcrossConverters(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model": "test-model",
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
		"input": []any{
			map[string]any{"role": "user", "content": "start"},
			// Interrupted tool call with NO matching output in the next user turn.
			map[string]any{"type": "function_call", "call_id": "call_interrupted", "name": "lookup", "arguments": `{"q":"cancelled"}`},
			map[string]any{"role": "user", "content": "actually do something else"},
			// Orphan tool result with NO preceding matching call, carrying text + image.
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_orphan",
				"output": []any{
					map[string]any{"type": "input_text", "text": "orphan output text"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + onePixelPNGBase64},
				},
			},
			// Properly paired tool call + output.
			map[string]any{"type": "function_call", "call_id": "call_ok", "name": "lookup", "arguments": `{"q":"good"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_ok", "output": "matched result"},
		},
	})

	// 1. OpenAI Chat Completions: only call_ok appears as tool_calls / role:tool.
	chatReq, err := convertResponsesToChatCompletions(raw)
	if err != nil {
		t.Fatalf("convertResponsesToChatCompletions failed: %v", err)
	}
	chatStr := string(chatReq.body)
	if strings.Contains(chatStr, `"id":"call_interrupted"`) || strings.Contains(chatStr, `"tool_call_id":"call_orphan"`) {
		t.Fatalf("orphan tool call/result leaked into Chat wire messages: %s", chatStr)
	}
	if !strings.Contains(chatStr, `"id":"call_ok"`) || !strings.Contains(chatStr, `"tool_call_id":"call_ok"`) {
		t.Fatalf("valid paired tool call was lost in Chat messages: %s", chatStr)
	}
	if !strings.Contains(chatStr, "Interrupted tool call lookup (call_interrupted)") || !strings.Contains(chatStr, "orphan output text") || !strings.Contains(chatStr, onePixelPNGBase64) {
		t.Fatalf("demoted context or image lost in Chat messages: %s", chatStr)
	}

	// 2. Anthropic Messages: only call_ok appears as tool_use / tool_result.
	anthReq, err := convertResponsesToAnthropicMessages(raw)
	if err != nil {
		t.Fatalf("convertResponsesToAnthropicMessages failed: %v", err)
	}
	anthStr := string(anthReq.body)
	if strings.Contains(anthStr, `"id":"call_interrupted"`) || strings.Contains(anthStr, `"tool_use_id":"call_orphan"`) {
		t.Fatalf("orphan tool call/result leaked into Anthropic messages: %s", anthStr)
	}
	if !strings.Contains(anthStr, `"id":"call_ok"`) || !strings.Contains(anthStr, `"tool_use_id":"call_ok"`) {
		t.Fatalf("valid paired tool call was lost in Anthropic messages: %s", anthStr)
	}

	// 3. Qoder: only call_ok appears as tool_calls / role:tool.
	qoderReq, err := convertResponsesToQoder(raw, []qoderModel{{ID: "test-model", Name: "test-model", Vision: true}})
	if err != nil {
		t.Fatalf("convertResponsesToQoder failed: %v", err)
	}
	qoderBytes, _ := json.Marshal(qoderReq.body["messages"])
	qoderStr := string(qoderBytes)
	if strings.Contains(qoderStr, `"id":"call_interrupted"`) || strings.Contains(qoderStr, `"tool_call_id":"call_orphan"`) {
		t.Fatalf("orphan tool call/result leaked into Qoder messages: %s", qoderStr)
	}
	if !strings.Contains(qoderStr, `"id":"call_ok"`) || !strings.Contains(qoderStr, `"tool_call_id":"call_ok"`) {
		t.Fatalf("valid paired tool call was lost in Qoder messages: %s", qoderStr)
	}
}

func TestToolSchemaNormalizationBuiltinValidationAndParallelEcho(t *testing.T) {
	rawSchema := `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"oneOf": [
			{"type": "object", "properties": {"id": {"type": "integer", "const": 9007199254740993}}, "required": ["id"]}
		]
	}`
	reqRaw := []byte(`{
		"model": "test-model",
		"parallel_tool_calls": false,
		"tools": [
			{"type": "function", "name": "complex_mcp", "parameters": ` + rawSchema + `},
			{"type": "shell"},
			{"type": "local_shell"},
			{"type": "apply_patch"}
		],
		"input": "run"
	}`)

	// 1. Chat, Qoder, and Anthropic strip $schema, lift object properties, and keep 64-bit integer precision.
	chatReq, err := convertResponsesToChatCompletions(reqRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chatReq.body), `"$schema"`) || !strings.Contains(string(chatReq.body), `9007199254740993`) {
		t.Fatalf("Chat schema normalization failed: %s", chatReq.body)
	}
	anthReq, err := convertResponsesToAnthropicMessages(reqRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(anthReq.body), `"$schema"`) || strings.Contains(string(anthReq.body), `"oneOf"`) || !strings.Contains(string(anthReq.body), `9007199254740993`) {
		t.Fatalf("Anthropic input_schema normalization failed: %s", anthReq.body)
	}
	qoderReq, err := convertResponsesToQoder(reqRaw, []qoderModel{{ID: "test-model", Name: "test-model"}})
	if err != nil {
		t.Fatal(err)
	}
	qToolsBytes, _ := json.Marshal(qoderReq.body["tools"])
	if strings.Contains(string(qToolsBytes), `"$schema"`) || !strings.Contains(string(qToolsBytes), `9007199254740993`) || !strings.Contains(string(qToolsBytes), `"type":"object"`) {
		t.Fatalf("Qoder tool schema normalization failed: %s", qToolsBytes)
	}

	// 2. Built-in shell / local_shell / apply_patch reject empty payloads and accept valid ones.
	parsed, err := parseResponsesRequest(reqRaw)
	if err != nil {
		t.Fatal(err)
	}
	adapter := parsed.adapter("test-model")
	assembler := newResponseAssembler(&adapter, nil)
	if got := assembler.response()["parallel_tool_calls"]; got != false {
		t.Fatalf("envelope parallel_tool_calls = %v, want false", got)
	}
	for _, tc := range []struct {
		name string
		bad  string
		good string
	}{
		{"shell", `{}`, `{"commands":["ls -la"]}`},
		{"local_shell", `{"command":[]}`, `{"command":["pwd"]}`},
		{"apply_patch", `{"type":"update_file"}`, `{"type":"update_file","path":"main.go","diff":"+ok"}`},
	} {
		if err := assembler.addToolUse("bad_"+tc.name, tc.name, tc.bad); err == nil {
			t.Fatalf("expected invalid %s payload %s to fail", tc.name, tc.bad)
		}
		if err := assembler.addToolUse("ok_"+tc.name, tc.name, tc.good); err != nil {
			t.Fatalf("expected valid %s payload %s to pass: %v", tc.name, tc.good, err)
		}
	}

	// 3. Non-stream Chat validates the entire batch before emitting the first call.
	badBatch := `{"choices":[{"message":{"tool_calls":[
		{"id":"ok_1","type":"function","function":{"name":"complex_mcp","arguments":"{\"id\":1}"}},
		{"id":"bad_2","type":"function","function":{"name":"undeclared_tool","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`
	rec := httptest.NewRecorder()
	nsAssembler := newResponseAssembler(&adapter, rec)
	if err := processChatCompletionsNonStream([]byte(badBatch), nsAssembler); err == nil {
		t.Fatal("expected non-stream Chat batch with undeclared second tool to fail")
	}
	if nsAssembler.hasToolUse || strings.Contains(rec.Body.String(), `"type":"function_call"`) {
		t.Fatal("first tool in non-stream Chat batch escaped before second tool was validated")
	}
}
