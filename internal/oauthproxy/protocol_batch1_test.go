package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatch1NormalizeUpstreamErrorCodes(t *testing.T) {
	cases := []struct {
		name      string
		dialect   string
		status    int
		body      string
		wantCode  string
		wantType  string
		wantParam string
	}{
		{
			name:      "openai context length exceeded",
			dialect:   "openai_responses",
			status:    http.StatusBadRequest,
			body:      `{"error":{"message":"Your input exceeds the context window of this model","type":"invalid_request_error","code":"context_length_exceeded","param":"input"}}`,
			wantCode:  "context_length_exceeded",
			wantType:  "invalid_request_error",
			wantParam: "input",
		},
		{
			name:     "anthropic prompt too long",
			dialect:  "anthropic",
			status:   http.StatusBadRequest,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`,
			wantCode: "context_length_exceeded",
			wantType: "invalid_request_error",
		},
		{
			name:     "permanent quota 429 stays insufficient_quota",
			dialect:  "openai_chat",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"message":"You exceeded your current quota, please check your plan","type":"insufficient_quota","code":"insufficient_quota"}}`,
			wantCode: "insufficient_quota",
			wantType: "insufficient_quota",
		},
		{
			name:     "transient 429 normalizes to rate_limit_exceeded",
			dialect:  "openai_chat",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`,
			wantCode: "rate_limit_exceeded",
			wantType: "rate_limit_error",
		},
		{
			name:     "401 normalizes to invalid_api_key",
			dialect:  "openai_chat",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"message":"Incorrect API key provided"}}`,
			wantCode: "invalid_api_key",
			wantType: "authentication_error",
		},
		{
			name:     "404 model not found",
			dialect:  "openai_chat",
			status:   http.StatusNotFound,
			body:     `{"error":{"message":"The model gpt-999 does not exist","code":"model_not_found"}}`,
			wantCode: "model_not_found",
			wantType: "not_found_error",
		},
		{
			name:     "413 request too large",
			dialect:  "openai_chat",
			status:   http.StatusRequestEntityTooLarge,
			body:     `{"error":{"message":"body too big"}}`,
			wantCode: "request_too_large",
			wantType: "request_too_large",
		},
		{
			name:     "502 server_error",
			dialect:  "openai_chat",
			status:   http.StatusBadGateway,
			body:     "<html>" + strings.Repeat("A", 10000) + "</html>",
			wantCode: "server_error",
			wantType: "api_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeUpstreamError(tc.dialect, tc.status, tc.body, "")
			if got.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (full=%+v)", got.Code, tc.wantCode, got)
			}
			if got.Type != tc.wantType {
				t.Fatalf("type = %q, want %q (full=%+v)", got.Type, tc.wantType, got)
			}
			if tc.wantParam != "" && got.Param != tc.wantParam {
				t.Fatalf("param = %q, want %q", got.Param, tc.wantParam)
			}
			if len(got.Message) > maxClientErrorMessageBytes {
				t.Fatalf("message length = %d, exceeds %d", len(got.Message), maxClientErrorMessageBytes)
			}
		})
	}
}

func TestBatch1RetryUpstreamExcludesQuotaAnd501AndParsesRetryAfter(t *testing.T) {
	oldBackoff := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = oldBackoff })

	// 1. Permanent quota 429 must NOT retry.
	var quotaCalls atomic.Int32
	_, err := retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		quotaCalls.Add(1)
		return nil, &chatCompletionsUpstreamError{
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":"insufficient_quota","type":"insufficient_quota","message":"You exceeded your current quota"}}`,
		}
	})
	if err == nil || quotaCalls.Load() != 1 {
		t.Fatalf("permanent quota 429 retried: calls=%d err=%v", quotaCalls.Load(), err)
	}

	// 2. HTTP 501 Not Implemented must NOT retry.
	var notImplCalls atomic.Int32
	_, err = retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		notImplCalls.Add(1)
		return nil, &chatCompletionsUpstreamError{status: http.StatusNotImplemented, body: "not implemented"}
	})
	if err == nil || notImplCalls.Load() != 1 {
		t.Fatalf("HTTP 501 retried: calls=%d err=%v", notImplCalls.Load(), err)
	}

	// 3. HTTP 408 Request Timeout MUST retry.
	var timeoutCalls atomic.Int32
	_, err = retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		if timeoutCalls.Add(1) == 1 {
			return nil, &chatCompletionsUpstreamError{status: http.StatusRequestTimeout, body: "timeout"}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	if err != nil || timeoutCalls.Load() != 2 {
		t.Fatalf("HTTP 408 did not retry: calls=%d err=%v", timeoutCalls.Load(), err)
	}

	// 4. Fractional Retry-After and Retry-After-Ms parsing.
	if delay, ok := retryAfterDelay("1.5"); !ok || delay != 1500*time.Millisecond {
		t.Fatalf("fractional Retry-After = (%v, %v), want (1.5s, true)", delay, ok)
	}
	headers := http.Header{"Retry-After-Ms": []string{"2500"}}
	errWithMS := &chatCompletionsUpstreamError{status: http.StatusTooManyRequests, headers: headers}
	if delay, ok := outcomeRetryAfterDelay(nil, errWithMS); !ok || delay != 2500*time.Millisecond {
		t.Fatalf("Retry-After-Ms = (%v, %v), want (2.5s, true)", delay, ok)
	}
	var msCalls atomic.Int32
	_, _ = retryUpstream(context.Background(), "test", func() (*http.Response, error) {
		msCalls.Add(1)
		return nil, errWithMS
	})
	if msCalls.Load() != 1 {
		t.Fatalf("Retry-After-Ms > backoff should short-circuit fast retry; calls=%d", msCalls.Load())
	}
}

func TestBatch1OpenAIChatPreStreamGateAndHTTP200ErrorEnvelope(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run("stream="+boolString(stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-RateLimit-Remaining-Requests", "0")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"slow down"}}`)
			}))
			defer upstream.Close()

			svc := &chatCompletionsService{
				apiKey:     "local-key",
				endpoint:   upstream.URL,
				authorizer: &chatStaticAuthorizer{token: "up-key"},
				client:     upstream.Client(),
			}
			body, _ := json.Marshal(map[string]any{
				"model":  "gpt-4o",
				"stream": stream,
				"input":  "hello",
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-key")
			rec := httptest.NewRecorder()
			svc.handleResponses(rec, req)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("X-RateLimit-Remaining-Requests") != "0" {
				t.Fatalf("missing relayed rate-limit header: %v", rec.Header())
			}
			if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("leaked SSE content-type on pre-stream failure: %v", rec.Header())
			}
			var decoded struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("invalid JSON error: %v (%s)", err, rec.Body.String())
			}
			if decoded.Error.Code != "rate_limit_exceeded" {
				t.Fatalf("error.code = %q, want rate_limit_exceeded", decoded.Error.Code)
			}
		})
	}
}

func TestBatch1AnthropicPreStreamGateContextLengthExceeded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10}}}\n\n")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"prompt is too long: 210000 tokens\"}}\n\n")
	}))
	defer upstream.Close()

	svc := newAnthropicMessagesService("local-key", upstream.URL, nil, nil, "up-key", nil)
	svc.client = upstream.Client()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	svc.handleResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var decoded struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON error: %v (%s)", err, rec.Body.String())
	}
	if decoded.Error.Code != "context_length_exceeded" {
		t.Fatalf("error.code = %q, want context_length_exceeded", decoded.Error.Code)
	}
}

func TestBatch1CodexResponsesStreamRejectsHTTP200ErrorAndFailed(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "http 200 json error envelope",
			body:       `{"error":{"message":"Prompt is too long","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "context_length_exceeded",
		},
		{
			name:       "http 200 non-stream failed response",
			body:       `{"id":"resp_1","object":"response","status":"failed","error":{"code":"rate_limit_exceeded","message":"rate limited"}}`,
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "rate_limit_exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			assembler := newResponseAssembler(&adapterRequest{clientModel: "gpt-5", stream: true}, rec)
			err := processCodexResponsesStream(strings.NewReader(tc.body), assembler)
			if err == nil {
				t.Fatal("expected error, got nil (was faked as response.completed!)")
			}
			if assembler.started || rec.Body.Len() != 0 {
				t.Fatalf("pre-stream gate leaked SSE bytes: started=%v body=%q", assembler.started, rec.Body.String())
			}
			var streamErr *streamUpstreamError
			if !errors.As(err, &streamErr) {
				t.Fatalf("expected *streamUpstreamError, got %T: %v", err, err)
			}
			if streamErr.status != tc.wantStatus || streamErr.code != tc.wantCode {
				t.Fatalf("got status=%d code=%q, want %d/%q", streamErr.status, streamErr.code, tc.wantStatus, tc.wantCode)
			}
		})
	}

	// response.incomplete with an unknown reason must NEVER fall back to completed.
	assembler := newResponseAssembler(&adapterRequest{clientModel: "gpt-5", stream: false}, nil)
	sse := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"turn_limit\"},\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n"
	if err := processCodexResponsesStream(strings.NewReader(sse), assembler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp := assembler.response()
	if resp["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", resp["status"])
	}
	usage := resp["usage"].(map[string]any)
	outDetails := usage["output_tokens_details"].(map[string]any)
	if outDetails["reasoning_tokens"] != 1 {
		t.Fatalf("reasoning_tokens = %v, want 1", outDetails["reasoning_tokens"])
	}
}

func encodeBatch1KiroExceptionFrame(exceptionType string, payloadRaw []byte) []byte {
	headers := append(encodeKiroTestHeader(":message-type", "exception"), encodeKiroTestHeader(":exception-type", exceptionType)...)
	totalLength := kiroEventPreludeSize + len(headers) + len(payloadRaw) + 4
	frame := make([]byte, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payloadRaw)
	binary.BigEndian.PutUint32(frame[totalLength-4:], crc32.ChecksumIEEE(frame[:totalLength-4]))
	return frame
}

func TestBatch1KiroZeroOutputContextLengthAndEmptyEOF(t *testing.T) {
	// Zero-output ContentLengthExceededException -> HTTP 400 context_length_exceeded.
	var zeroPayload bytes.Buffer
	zeroPayload.Write(encodeBatch1KiroExceptionFrame("ContentLengthExceededException", []byte(`{"message":"Input is too long"}`)))

	service, closeUpstream := newKiroUpstreamTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zeroPayload.Bytes())
	})
	defer closeUpstream()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.6","stream":true,"input":"hello"}`))
	req.Header.Set("x-api-key", service.apiKey)
	service.handleResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zero-output ContentLengthExceededException status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"context_length_exceeded"`) {
		t.Fatalf("missing context_length_exceeded code: %s", rec.Body.String())
	}

	// 0-frame empty HTTP 200 EOF must fail instead of faking response.completed.
	emptyService, closeEmpty := newKiroUpstreamTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer closeEmpty()
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.6","stream":true,"input":"hello"}`))
	req2.Header.Set("x-api-key", emptyService.apiKey)
	emptyService.handleResponses(rec2, req2)
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("empty 0-frame EOF status = %d, want 502; body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestBatch1RefusalVsContentFilterAndAssemblerAccounting(t *testing.T) {
	// 1. Refusal emits response.refusal.delta/.done and type="refusal" while keeping contentBlocks().Text.
	rec := httptest.NewRecorder()
	a := newResponseAssembler(&adapterRequest{clientModel: "m", stream: true}, rec)
	a.stopReason = stopRefusal
	if err := a.addRefusal("I cannot comply."); err != nil {
		t.Fatal(err)
	}
	if err := a.finish(); err != nil {
		t.Fatal(err)
	}
	streamBody := rec.Body.String()
	if !strings.Contains(streamBody, "event: response.refusal.delta") || !strings.Contains(streamBody, "event: response.refusal.done") {
		t.Fatalf("missing refusal SSE events: %s", streamBody)
	}
	if !strings.Contains(streamBody, `"usage":null`) {
		t.Fatalf("response.created/in_progress should emit usage:null: %s", streamBody)
	}
	blocks := a.contentBlocks()
	if len(blocks) != 1 || blocks[0].Text != "I cannot comply." {
		t.Fatalf("contentBlocks = %+v", blocks)
	}

	// 2. Content filter after partial text + unclosed tool -> response.incomplete(reason="content_filter"),
	// active block marked status="incomplete", unclosed tool dropped cleanly.
	rec2 := httptest.NewRecorder()
	a2 := newResponseAssembler(&adapterRequest{clientModel: "m", stream: true}, rec2)
	if err := a2.addText("partial text"); err != nil {
		t.Fatal(err)
	}
	a2.tools["call_1"] = &assemblerToolAccumulator{name: "lookup"}
	a2.stopReason = stopContentFilter
	if err := a2.finish(); err != nil {
		t.Fatalf("content_filter finish failed: %v", err)
	}
	if !strings.Contains(rec2.Body.String(), "event: response.incomplete") || !strings.Contains(rec2.Body.String(), `"reason":"content_filter"`) {
		t.Fatalf("expected response.incomplete(content_filter): %s", rec2.Body.String())
	}
	resp2 := a2.response()
	output2 := resp2["output"].([]map[string]any)
	if output2[0]["status"] != "incomplete" {
		t.Fatalf("interrupted block status = %v, want incomplete", output2[0]["status"])
	}

	// 3. Cumulative byte output estimation avoids 1-byte delta 4x inflation.
	a3 := newResponseAssembler(&adapterRequest{clientModel: "m"}, nil)
	for i := 0; i < 40; i++ {
		_ = a3.addText("a")
	}
	_, outTokens := a3.tokenTotals()
	if outTokens != 10 {
		t.Fatalf("40 ASCII 1-byte deltas estimated %d tokens, want 10", outTokens)
	}

	// 4. Base64 image in parseResponsesRequest does NOT inflate rawTokens by 333K/MiB.
	bigPNG := base64.StdEncoding.EncodeToString(append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x42}, 512<<10)...))
	reqJSON, _ := json.Marshal(map[string]any{
		"model": "gpt-5",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "describe this"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + bigPNG},
			},
		}},
	})
	parsed, err := parseResponsesRequest(reqJSON)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.rawTokens > 5000 {
		t.Fatalf("512 KiB base64 image inflated rawTokens to %d (want ~1000)", parsed.rawTokens)
	}
}

func TestBatch1ChatFrontRefusalDoneTruncationAndFailedNonStream(t *testing.T) {
	// 1. Streaming refusal.delta + .done-only output_text + truncated stream without terminal event.
	mux := loopbackFrontMux("key",
		func(w http.ResponseWriter, _ *http.Request) { writeModelList(w, nil) },
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "refusal-model" {
				prepareEventStream(w)
				_, _ = io.WriteString(w, "event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"no can do\"}\n\n")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
				return
			}
			if body["model"] == "done-only-model" {
				prepareEventStream(w)
				_, _ = io.WriteString(w, "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":\"recovered\"}\n\n")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
				return
			}
			if body["model"] == "truncated-model" {
				prepareEventStream(w)
				_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"half\"}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"r3","object":"response","status":"failed","error":{"code":"context_length_exceeded","message":"too long"}}`)
		},
	)

	postChat := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	recRefusal := postChat(`{"model":"refusal-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(recRefusal.Body.String(), `"refusal":"no can do"`) {
		t.Fatalf("missing streamed refusal delta: %s", recRefusal.Body.String())
	}

	recDone := postChat(`{"model":"done-only-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(recDone.Body.String(), `"content":"recovered"`) {
		t.Fatalf("failed to recover .done-only text: %s", recDone.Body.String())
	}

	recTrunc := postChat(`{"model":"truncated-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(recTrunc.Body.String(), `"upstream_stream_error"`) {
		t.Fatalf("truncated stream must emit error chunk before [DONE]: %s", recTrunc.Body.String())
	}

	recFailed := postChat(`{"model":"failed-nonstream","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if recFailed.Code != http.StatusBadRequest || !strings.Contains(recFailed.Body.String(), `"context_length_exceeded"`) {
		t.Fatalf("non-stream failed response should return HTTP 400 context_length_exceeded, got %d: %s", recFailed.Code, recFailed.Body.String())
	}
}

func TestBatch1QoderUsageSplitFramesAndFullCacheHit(t *testing.T) {
	// Frame 1: prompt_tokens=5000, cached_tokens=5000 (100% cache hit -> fresh input=0).
	// Frame 2: completion_tokens=42, reasoning_tokens=15 (omits prompt_tokens!).
	// Also tests reasoning_content when thinkingEnabled=false still routes to thinking.
	sse := strings.Join([]string{
		`data: {"body":{"choices":[{"delta":{"reasoning_content":"secret plan"}}],"usage":{"prompt_tokens":5000,"prompt_tokens_details":{"cached_tokens":5000}}}}`,
		`data: {"body":{"choices":[{"delta":{"content":"final answer"}}]}}`,
		`data: {"body":{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":42,"completion_tokens_details":{"reasoning_tokens":15}}}}`,
		"",
	}, "\n\n")

	a := newResponseAssembler(&adapterRequest{clientModel: "qmodel", inputTokens: 9999, thinkingEnabled: false}, nil)
	usage, err := processQoderEventStream(context.Background(), strings.NewReader(sse), a)
	if err != nil {
		t.Fatalf("processQoderEventStream error: %v", err)
	}
	if usage.input != 0 || usage.cacheRead != 5000 || usage.output != 42 {
		t.Fatalf("usage = %+v, want input=0 cacheRead=5000 output=42 (no fallback double-count!)", usage)
	}
	if a.reasoningTokens != 15 {
		t.Fatalf("reasoningTokens = %d, want 15", a.reasoningTokens)
	}
	blocks := a.contentBlocks()
	if len(blocks) != 2 || blocks[0].Type != blockThinking || blocks[0].Thinking != "secret plan" || blocks[1].Type != blockText || blocks[1].Text != "final answer" {
		t.Fatalf("reasoning_content leaked into text or misrouted: %+v", blocks)
	}
}

func TestBatch1CursorTrailerRetryAuthRefreshAndTokenDelta(t *testing.T) {
	oldBackoff := upstreamFastRetryBackoff
	upstreamFastRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { upstreamFastRetryBackoff = oldBackoff })

	// Set up OAuth credential file + token refresh server.
	credPath := filepath.Join(t.TempDir(), "cursor.json")
	if err := writeCodexCredentialAtomic(credPath, map[string]any{
		"type":          ProviderCursor,
		"access_token":  "stale-token",
		"refresh_token": "refresh-1",
		"expired":       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessToken":"fresh-token","refreshToken":"refresh-2"}`)
	}))
	defer refreshSrv.Close()
	oldTokenURL := cursorOAuthToken
	cursorOAuthToken = refreshSrv.URL
	t.Cleanup(func() { cursorOAuthToken = oldTokenURL })

	var runCalls atomic.Int32
	stub := newCursorStubUpstream(t, func(w http.ResponseWriter, r *http.Request, _ <-chan cursorStubFrame) {
		call := runCalls.Add(1)
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		switch call {
		case 1:
			// First call: ConnectRPC EndStream unauthenticated trailer -> triggers 401 refresh!
			_, _ = w.Write(cursorFrameConnect([]byte(`{"error":{"code":"unauthenticated","message":"token expired"}}`), cursorConnectEndStream))
		case 2:
			// Second call: must carry refreshed token; return transient unavailable trailer -> triggers fast retry!
			if r.Header.Get("Authorization") != "Bearer fresh-token" {
				t.Errorf("second call auth = %q, want Bearer fresh-token", r.Header.Get("Authorization"))
			}
			_, _ = w.Write(cursorFrameConnect([]byte(`{"error":{"code":"unavailable","message":"try again"}}`), cursorConnectEndStream))
		default:
			// Third call: emit text + token delta + turn_end!
			writeCursorFrame(w, cursorTestInteraction(cursorIUTextDelta, "hello from cursor"))
			tokenFrame := cursorAppendBytes(nil, cursorASMInteractionUpdate,
				cursorAppendBytes(nil, cursorIUTokenDelta, cursorAppendUint32(nil, 1, 27)))
			writeCursorFrame(w, tokenFrame)
			writeCursorFrame(w, cursorTestTurnEnd())
			_, _ = w.Write(cursorFrameConnect([]byte(`{}`), cursorConnectEndStream))
		}
	})

	svc := &cursorService{
		apiKey:     "local-key",
		models:     []string{"composer-2"},
		authorizer: &cursorOAuthAuthorizer{path: credPath, client: refreshSrv.Client()},
		client:     stub.server.Client(),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"composer-2","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	svc.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", refreshes.Load())
	}
	if runCalls.Load() != 3 {
		t.Fatalf("run calls = %d, want 3 (unauth -> unavailable -> ok)", runCalls.Load())
	}
	if !strings.Contains(rec.Body.String(), "hello from cursor") || !strings.Contains(rec.Body.String(), `"output_tokens":27`) {
		t.Fatalf("missing text or TokenDelta output_tokens=27: %s", rec.Body.String())
	}

	// Verify cursorConnectStatus mappings.
	if got := cursorConnectStatus("invalid_argument"); got != http.StatusBadRequest {
		t.Fatalf("invalid_argument -> %d, want 400", got)
	}
	if got := cursorConnectStatus("not_found"); got != http.StatusNotFound {
		t.Fatalf("not_found -> %d, want 404", got)
	}
	if got := cursorConnectStatus("deadline_exceeded"); got != http.StatusGatewayTimeout {
		t.Fatalf("deadline_exceeded -> %d, want 504", got)
	}
}

func TestBatch1OAuthRefreshFailureReturns401Not502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"expired token"}}`)
	}))
	defer upstream.Close()

	svc := &chatCompletionsService{
		apiKey:   "local-key",
		endpoint: upstream.URL,
		authorizer: failingChatOAuthAuthorizer{
			err: errors.New("refresh token invalid_grant"),
		},
		client: upstream.Client(),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	svc.handleResponses(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invalid_api_key"`) {
		t.Fatalf("expected invalid_api_key code: %s", rec.Body.String())
	}
}

func TestBatch1InboundBodyTooLargeReturns413(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", 64)))
	_, err := readInboundBody(rec, req, 16)
	if !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("err = %v, want errRequestBodyTooLarge", err)
	}
	writeInboundBodyError(rec, err)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), `"request_too_large"`) {
		t.Fatalf("status=%d body=%s, want 413 request_too_large", rec.Code, rec.Body.String())
	}
}

type failingChatOAuthAuthorizer struct {
	err error
}

func (a failingChatOAuthAuthorizer) authorize(_ context.Context, force bool) (string, error) {
	if force {
		return "", a.err
	}
	return "stale-token", nil
}
func (failingChatOAuthAuthorizer) isOAuth() bool { return true }

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
