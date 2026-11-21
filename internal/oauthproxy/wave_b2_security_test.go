package oauthproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorProtobufDeepNestingAndSpecialFloats(t *testing.T) {
	// Build a deeply nested google.protobuf.Value (Struct -> field "a" -> Value -> ...)
	// exceeding cursorProtobufValueMaxDepth; must not stack-overflow.
	val := cursorAppendString(nil, 3, "leaf")
	for range 128 {
		entry := cursorAppendString(nil, 1, "a")
		entry = cursorAppendBytes(entry, 2, val)
		structMsg := cursorAppendBytes(nil, 1, entry)
		val = cursorAppendBytes(nil, 5, structMsg)
	}
	got := cursorProtobufValueToJSON(val)
	if !json.Valid(got) {
		t.Fatalf("deeply nested struct produced invalid JSON: %s", got)
	}

	// Build a deeply nested ListValue as well.
	listVal := cursorAppendString(nil, 3, "leaf")
	for range 128 {
		listMsg := cursorAppendBytes(nil, 1, listVal)
		listVal = cursorAppendBytes(nil, 6, listMsg)
	}
	gotList := cursorProtobufValueToJSON(listVal)
	if !json.Valid(gotList) {
		t.Fatalf("deeply nested list produced invalid JSON: %s", gotList)
	}

	// NaN and Inf must serialize safely to JSON null instead of empty/invalid bytes.
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		raw := cursorAppendFixed64(nil, 2, math.Float64bits(f))
		encoded := cursorProtobufValueToJSON(raw)
		if string(encoded) != "null" {
			t.Fatalf("float %v encoded as %q, want null", f, encoded)
		}
	}
}

func TestCursorSessionCloseUnblocksBlockedSend(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	session := newCursorSession(&http.Response{Body: io.NopCloser(strings.NewReader(""))}, pw, func() {})

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- session.send([]byte("blocked-frame"))
	}()
	// Give send() time to block inside io.PipeWriter.Write.
	time.Sleep(25 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() {
		session.close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session.close() deadlocked while send() was blocked on pipe")
	}
	select {
	case err := <-sendDone:
		if err == nil {
			t.Fatal("expected blocked send() to fail after close()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked send() did not unblock after session.close()")
	}
}

func TestKiroAndXaiSSRFGuards(t *testing.T) {
	// Kiro region must match strict AWS region pattern.
	if _, err := validateKiroRegion("us-east-1"); err != nil {
		t.Fatalf("valid region rejected: %v", err)
	}
	for _, bad := range []string{"evil.com#", "us-east-1.attacker.example", "../us-east-1", "us_east_1"} {
		if _, err := validateKiroRegion(bad); err == nil {
			t.Fatalf("malicious Kiro region %q should be rejected", bad)
		}
	}

	dir := t.TempDir()
	credPath := filepath.Join(dir, "kiro-bad.json")
	if err := os.WriteFile(credPath, []byte(`{"type":"kiro","access_token":"tok","region":"evil.com#"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKiroCredential(credPath); err == nil {
		t.Fatal("loadKiroCredential accepted invalid region")
	}

	// xAI token_endpoint must use https on x.ai / *.x.ai without userinfo.
	if _, err := validateXaiAuthEndpoint("https://auth.x.ai/oauth2/token"); err != nil {
		t.Fatalf("valid xAI endpoint rejected: %v", err)
	}
	for _, bad := range []string{
		"http://auth.x.ai/oauth2/token",
		"https://evil.example/oauth2/token",
		"https://x.ai.evil.example/oauth2/token",
		"https://user:pass@auth.x.ai/oauth2/token",
		"https://169.254.169.254/latest/meta-data",
	} {
		if _, err := validateXaiAuthEndpoint(bad); err == nil {
			t.Fatalf("untrusted xAI endpoint %q should be rejected", bad)
		}
	}
}

func TestMixedProtocolRouterHonorsPerModelEndpointsForChatAndResponses(t *testing.T) {
	var hitDefault, hitChatOverride, hitRespOverride int
	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitDefault++
		http.Error(w, "wrong default endpoint", http.StatusTeapot)
	}))
	defer defaultSrv.Close()

	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitChatOverride++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"chat-ok"},"finish_reason":"stop"}]}`)
	}))
	defer chatSrv.Close()

	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitRespOverride++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"resp-ok"}]}],"usage":{"input_tokens":12,"output_tokens":4}}`)
	}))
	defer respSrv.Close()

	rt, err := StartMixedProtocolAPIKeyRuntime(
		context.Background(),
		defaultSrv.URL+"/v1",
		"upstream-key",
		"chat-m,resp-m",
		map[string]string{
			"chat-m": "openai",
			"resp-m": "openai_responses",
		},
		map[string]string{
			"chat-m": chatSrv.URL + "/v1",
			"resp-m": respSrv.URL + "/v1",
		},
	)
	if err != nil {
		t.Fatalf("StartMixedProtocolAPIKeyRuntime error: %v", err)
	}
	defer rt.Stop()

	post := func(model string) string {
		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(`{"model":"`+model+`","stream":false,"input":"hi"}`))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", model, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status=%d body=%s", model, resp.StatusCode, body)
		}
		return string(body)
	}

	if body := post("chat-m"); !strings.Contains(body, "chat-ok") {
		t.Fatalf("chat-m body = %s", body)
	}
	if body := post("resp-m"); !strings.Contains(body, "resp-ok") {
		t.Fatalf("resp-m body = %s", body)
	}
	if hitDefault != 0 || hitChatOverride != 1 || hitRespOverride != 1 {
		t.Fatalf("hits default=%d chat=%d resp=%d", hitDefault, hitChatOverride, hitRespOverride)
	}
	// Also verify non-SSE Responses passthrough recorded usage via observer.finish().
	totals, ok := rt.Usage().Snapshot()
	if !ok {
		t.Fatal("expected non-empty usage snapshot")
	}
	var respUsage UsageTotals
	for _, entry := range totals {
		if entry.Model == "resp-m" {
			respUsage = entry.UsageTotals
		}
	}
	if respUsage.InputTokens != 12 || respUsage.OutputTokens != 4 {
		t.Fatalf("resp-m non-SSE usage = %+v, want input=12 output=4", respUsage)
	}
}

func TestQoderStreamRejectsTruncatedStreamWithoutTerminalEvent(t *testing.T) {
	reqParsed, err := parseResponsesRequest([]byte(`{"model":"qoder-m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	adapter := reqParsed.adapter("qoder-m")
	assembler := newResponseAssembler(&adapter, nil)
	partial := `data: {"statusCodeValue":200,"body":{"choices":[{"delta":{"content":"partial"}}]}}` + "\n\n"
	_, err = processQoderEventStream(context.Background(), strings.NewReader(partial), assembler)
	if !errors.Is(err, io.ErrUnexpectedEOF) || assembler.finished {
		t.Fatalf("truncated Qoder stream: err=%v finished=%v", err, assembler.finished)
	}
}

func TestKiroBudgetTrimStripsToolSpecificationDescription(t *testing.T) {
	longDesc := strings.Repeat("d", 12_000)
	body := map[string]any{
		"conversationState": map[string]any{
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": "hi",
					"userInputMessageContext": map[string]any{
						"tools": []any{
							kiroToolDefinition("t1", longDesc, map[string]any{"type": "object"}),
						},
					},
				},
			},
		},
	}
	enforceKiroContentBudget(body, 0, 200)
	ctx := kiroCurrentMessageContext(body["conversationState"].(map[string]any))
	tools := kiroAnySlice(ctx["tools"])
	if len(tools) == 1 {
		spec := kiroAnyMap(kiroAnyMap(tools[0])["toolSpecification"])
		if _, hasDesc := spec["description"]; hasDesc {
			t.Fatal("expected toolSpecification.description to be stripped under budget pressure")
		}
	}
}
