package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/tidwall/gjson"
)

func TestBatch3GatewayNativeResponsesHistoryStoreFalseAndItemRef(t *testing.T) {
	var mu sync.Mutex
	var upstreamInputs []string
	var forwardedSessionID string
	turn := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		turn++
		cur := turn
		upstreamInputs = append(upstreamInputs, string(b))
		forwardedSessionID = r.Header.Get("Session-Id")
		mu.Unlock()
		if gjson.GetBytes(b, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			respJSON := `{"id":"resp_nat_` + string(rune('0'+cur)) + `","object":"response","status":"completed","model":"model-a","output":[{"id":"msg_nat_` + string(rune('0'+cur)) + `","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"reply-` + string(rune('0'+cur)) + `","annotations":[]}]}]}`
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+respJSON+"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_nat_ns","object":"response","status":"completed","model":"model-a","output":[{"id":"msg_nat_ns","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"reply-ns","annotations":[]}]}]}`)
	}))
	defer upstream.Close()

	// Disable per-runtime history cache just like proxymanager.prepareBindings does.
	rt, err := oauthproxy.StartProvider(context.Background(), oauthproxy.StartOptions{
		Protocol:            "openai_responses",
		Endpoint:            upstream.URL + "/v1",
		APIKey:              "fake-key",
		ModelSpec:           "model-a",
		DisableHistoryCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	g, err := New(context.Background(), rt, provider.Provider{Name: "chosen", Type: "openai_responses", Model: "model-a"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	doPost := func(payload map[string]any) (*http.Response, []byte) {
		raw, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, g.BaseURL()+"/responses", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+g.Token())
		req.Header.Set("X-Grok-Conv-Id", "grok-conv-7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// 1. store:false is honored on NativeResponses route.
	resp, _ := doPost(map[string]any{"model": "gbx/chosen/model-a", "stream": true, "store": false, "input": "ephemeral"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp, b := doPost(map[string]any{"model": "gbx/chosen/model-a", "stream": true, "previous_response_id": "resp_nat_1", "input": "follow"})
	if resp.StatusCode != http.StatusBadRequest || gjson.GetBytes(b, "error.code").String() != "previous_response_not_found" || gjson.GetBytes(b, "error.param").String() != "previous_response_id" {
		t.Fatalf("expected 400 previous_response_not_found with param, got status=%d body=%s", resp.StatusCode, b)
	}

	// 2. Stored NativeResponses turn chains via previous_response_id AND item_reference.
	resp, _ = doPost(map[string]any{"model": "gbx/chosen/model-a", "stream": true, "input": "turn-one-goal"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp, b = doPost(map[string]any{"model": "gbx/chosen/model-a", "stream": true, "previous_response_id": "resp_nat_2", "input": "turn-two"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected previous_response_id chaining to work on NativeResponses, got %d: %s", resp.StatusCode, b)
	}
	mu.Lock()
	chained := upstreamInputs[len(upstreamInputs)-1]
	sessID := forwardedSessionID
	mu.Unlock()
	if !strings.Contains(chained, "turn-one-goal") || !strings.Contains(chained, "reply-2") || gjson.Get(chained, "previous_response_id").Exists() {
		t.Fatalf("expected expanded history and stripped previous_response_id upstream, got: %s", chained)
	}
	if sessID != "grok-conv-7" {
		t.Fatalf("expected X-Grok-Conv-Id propagated to upstream Session-Id, got %q", sessID)
	}

	// 3. item_reference expansion on NativeResponses route.
	resp, b = doPost(map[string]any{"model": "gbx/chosen/model-a", "stream": false, "input": []any{
		map[string]any{"type": "item_reference", "id": "msg_nat_2"},
		map[string]any{"type": "message", "role": "user", "content": "check ref"},
	}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected item_reference expansion on NativeResponses, got %d: %s", resp.StatusCode, b)
	}
	mu.Lock()
	refReq := upstreamInputs[len(upstreamInputs)-1]
	mu.Unlock()
	if !strings.Contains(refReq, "reply-2") || strings.Contains(refReq, "item_reference") {
		t.Fatalf("expected item_reference expanded before upstream, got: %s", refReq)
	}
}

func TestBatch3GatewayBridgeStoreFalseItemRefAndResponseSummary(t *testing.T) {
	// 1. responseSummary ignores reasoning items and joins multiple output_text parts with newline.
	summary := responseSummary(map[string]any{
		"output": []any{
			map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": "SECRET CHAIN"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "refusal", "refusal": "ignored"},
				map[string]any{"type": "output_text", "text": "Part A"},
				map[string]any{"type": "output_text", "text": "Part B"},
			}},
		},
	})
	if summary != "Part A\nPart B" {
		t.Fatalf("responseSummary = %q, want %q", summary, "Part A\nPart B")
	}

	// 2. responseHistory composite key survives A -> B -> A without overwriting route A.
	var h responseHistory
	h.put("shared-id", "route-a", "owner-1", []map[string]any{{"id": "item-a", "type": "message", "role": "user", "content": "A"}})
	h.put("shared-id", "route-b", "owner-1", []map[string]any{{"id": "item-b", "type": "message", "role": "user", "content": "B"}})
	gotA, okA := h.get("shared-id", "route-a", "owner-1")
	gotB, okB := h.get("shared-id", "route-b", "owner-1")
	if !okA || !okB || gotA[0]["content"] != "A" || gotB[0]["content"] != "B" {
		t.Fatalf("expected composite historyKey isolation across routes, got A=%v/%v B=%v/%v", gotA, okA, gotB, okB)
	}

	// 3. serveBridge honors store:false and reports param:"previous_response_id" on miss.
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}, "openai", Options{})
	resp := postResponses(t, g, map[string]any{"model": "gbx/chosen/model-a", "stream": false, "store": false, "input": "hello"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resObj := responseObject(t, resp)
	id, _ := resObj["id"].(string)
	missResp := postResponses(t, g, map[string]any{"model": "gbx/chosen/model-a", "stream": false, "previous_response_id": id, "input": "next"})
	missBody, _ := io.ReadAll(missResp.Body)
	if missResp.StatusCode != http.StatusBadRequest || gjson.GetBytes(missBody, "error.param").String() != "previous_response_id" {
		t.Fatalf("expected 400 with param:previous_response_id for unstored response, got %d: %s", missResp.StatusCode, missBody)
	}
}

func TestBatch3CompactionAndMigrationHardening(t *testing.T) {
	// 1. compactionItems scrubs omitted-"type":"message" images, input_file, and input_audio.
	items := []map[string]any{
		{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
				map[string]any{"type": "input_file", "filename": "big.pdf", "file_data": "data:application/pdf;base64,JVBERi0xLjQK"},
				map[string]any{"type": "input_audio", "data": "UklGRg==", "format": "wav"},
			},
		},
	}
	compacted := compactionItems(items)
	raw, _ := json.Marshal(compacted)
	if strings.Contains(string(raw), "AAAA") || strings.Contains(string(raw), "JVBERi0xLjQK") || strings.Contains(string(raw), "UklGRg==") {
		t.Fatalf("compactionItems leaked inline media payload: %s", raw)
	}
	if !strings.Contains(string(raw), "[image omitted]") || !strings.Contains(string(raw), "[file omitted]") || !strings.Contains(string(raw), "[audio omitted]") {
		t.Fatalf("expected omitted placeholders in compactionItems: %s", raw)
	}

	// 2. pruneInlineHistoryImages demotes paired computer_call + computer_call_output together.
	hist := []map[string]any{
		{"type": "computer_call", "call_id": "cc_1", "action": map[string]any{"type": "screenshot"}},
		{"type": "computer_call_output", "call_id": "cc_1", "output": map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;base64,OLD"}},
		{"type": "computer_call", "call_id": "cc_2", "action": map[string]any{"type": "screenshot"}},
		{"type": "computer_call_output", "call_id": "cc_2", "output": map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;base64,NEW"}},
	}
	pruned := pruneInlineHistoryImages(hist, 2, 1)
	prunedJSON, _ := json.Marshal(pruned)
	if strings.Contains(string(prunedJSON), "OLD") || !strings.Contains(string(prunedJSON), "NEW") {
		t.Fatalf("expected older screenshot pruned and newer kept: %s", prunedJSON)
	}
	// First item must be demoted from computer_call to message because its output screenshot was pruned.
	if pruned[0]["type"] != "message" || pruned[1]["type"] != "message" || pruned[2]["type"] != "computer_call" {
		t.Fatalf("expected paired computer_call + output demoted to message: %s", prunedJSON)
	}

	// 3. DecodeCompactionSummary rejects invalid UTF-8 and oversized summaries.
	badUTF8 := "gbx1:" + base64.RawURLEncoding.EncodeToString([]byte{0xff, 0xfe, 0xfd})
	if _, ok := decodeCompactionSummary(badUTF8); ok {
		t.Fatal("expected invalid UTF-8 compaction summary to be rejected")
	}

	// 4. scanMigrationTranscript tolerates blank lines and case-insensitive UUID matching.
	sessionLower := "12345678-1234-1234-1234-1234567890ab"
	sessionUpper := strings.ToUpper(sessionLower)
	lines := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + sessionUpper + `"}}`,
		``,
		`   `,
		`{"type":"response_item","payload":{"role":"user","content":"kept goal"}}`,
		`{"type":"compacted","payload":{"replacement_history":[{"type":"compaction","encrypted_content":"opaque-abc"}]}}`,
	}, "\n")
	var excerpt migrationExcerpt
	matched, err := scanMigrationTranscript(strings.NewReader(lines), sessionLower, "opaque-abc", "", &excerpt)
	if err != nil || !matched || !strings.Contains(excerpt.text(), "kept goal") {
		t.Fatalf("expected blank-line tolerant, case-insensitive UUID migration scan, matched=%v err=%v excerpt=%q", matched, err, excerpt.text())
	}
}
