package oauthproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatToolChoiceNoneIsPreserved(t *testing.T) {
	choice, ok := chatToolChoice(conversationToolChoice{Mode: toolChoiceNone})
	if !ok {
		t.Fatal("chatToolChoice(none) reported nothing to send")
	}
	if choice != "none" {
		t.Fatalf("chatToolChoice(none) = %v, want none", choice)
	}
}

func TestInternalSubscriptionGatewaysRequireRuntimeKey(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "workbuddy", handler: (&workbuddyGateway{key: "runtime-secret"}).serveHTTP},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			recorder := httptest.NewRecorder()
			tc.handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), "invalid gateway key") {
				t.Fatalf("body = %q", recorder.Body.String())
			}
		})
	}
}

func TestKimiDeviceTokenSlowDownGrowsInterval(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"error":"slow_down"}`)
	}))
	defer stub.Close()
	original := kimiTokenURL
	kimiTokenURL = stub.URL
	defer func() { kimiTokenURL = original }()

	token, next, err := exchangeKimiDeviceToken(context.Background(), stub.Client(), "device-id", nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "" {
		t.Fatalf("slow_down returned a token: %+v", token)
	}
	if next != 10*time.Second {
		t.Fatalf("slow_down next interval = %s, want 10s (5s + 5s step)", next)
	}
}

func TestMixedRuntimeSurfacesModelCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"object":"list","data":[]}`)
	}))
	defer upstream.Close()
	runtime, err := StartMixedProtocolAPIKeyRuntime(context.Background(), upstream.URL, "upstream-key",
		"chat-model,responses-model",
		map[string]string{"responses-model": "openai_responses", "chat-model": "openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	models := runtime.Models()
	if len(models) != 2 || models[0] != "chat-model" || models[1] != "responses-model" {
		t.Fatalf("runtime models = %v, want [chat-model responses-model]", models)
	}
}

func TestStreamingReadersBoundMultilineEvents(t *testing.T) {
	line := strings.Repeat("a", codexResponsesMaxSSEEventBytes/2)
	input := "data: " + line + "\ndata: " + line + "\n"
	if err := readChatCompletionsSSE(strings.NewReader(input), func([]byte) error { return nil }); err == nil {
		t.Fatal("OpenAI multiline SSE event exceeded the limit without an error")
	}
	if err := readCodexSSE(strings.NewReader(input), func([]byte) error { return nil }); err == nil {
		t.Fatal("Codex multiline SSE event exceeded the limit without an error")
	}
}

func TestResponsesThinkingBlockIncludesEncryptedSignature(t *testing.T) {
	recorder := httptest.NewRecorder()
	assembler := newResponseAssembler(&adapterRequest{
		clientModel:     "test-model",
		thinkingEnabled: true,
	}, recorder)
	if err := assembler.start(); err != nil {
		t.Fatalf("start() error: %v", err)
	}
	if err := assembler.addThinking("reasoning step"); err != nil {
		t.Fatalf("addThinking() error: %v", err)
	}
	assembler.blocks[assembler.activeIndex].Signature = "test-sig"
	if err := assembler.finish(); err != nil {
		t.Fatalf("finish() error: %v", err)
	}
	stream := recorder.Body.String()
	if !strings.Contains(stream, `"encrypted_content":"test-sig"`) {
		t.Fatalf("stream missing encrypted_content on reasoning output item: %s", stream)
	}
}
