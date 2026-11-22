package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatch1GatewayBodyErrors413And415AndMalformedJSON(t *testing.T) {
	g := fixtureGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, "openai", Options{})

	base := "http://" + g.listener.Addr().String()

	// 1. Unsupported Content-Encoding -> 415.
	req := httptest.NewRequest(http.MethodPost, base+"/v1/responses", strings.NewReader(`{"model":"gbx/chosen/model-a","input":"hi"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Encoding", "br")
	rec := httptest.NewRecorder()
	g.serve(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported encoding status = %d, want 415; body=%s", rec.Code, rec.Body.String())
	}

	// 2. Content-Length > maxBodyBytes -> 413 request_too_large.
	req413 := httptest.NewRequest(http.MethodPost, base+"/v1/responses", strings.NewReader(`{}`))
	req413.RemoteAddr = "127.0.0.1:12345"
	req413.ContentLength = maxBodyBytes + 1
	req413.Header.Set("Authorization", "Bearer "+g.token)
	rec413 := httptest.NewRecorder()
	g.serve(rec413, req413)
	if rec413.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec413.Body.String(), `"request_too_large"`) {
		t.Fatalf("oversized status=%d body=%s, want 413 request_too_large", rec413.Code, rec413.Body.String())
	}

	// 3. Malformed JSON -> explicit JSON error (not missing model).
	reqBad := httptest.NewRequest(http.MethodPost, base+"/v1/responses", strings.NewReader(`{not valid json`))
	reqBad.RemoteAddr = "127.0.0.1:12345"
	reqBad.Header.Set("Authorization", "Bearer "+g.token)
	recBad := httptest.NewRecorder()
	g.serve(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest || !strings.Contains(strings.ToLower(recBad.Body.String()), "json") {
		t.Fatalf("malformed JSON status=%d body=%s, want 400 JSON error", recBad.Code, recBad.Body.String())
	}
}

func TestBatch1GatewayWriteErrorNullCodeAndHeaderRelay(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "invalid_request_error", "bad input", "")
	if !strings.Contains(rec.Body.String(), `"code":null`) {
		t.Fatalf("empty code should serialize as null, got: %s", rec.Body.String())
	}

	upstream := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type":                 []string{"application/json"},
			"Retry-After":                  []string{"3"},
			"Retry-After-Ms":               []string{"3000"},
			"X-Ratelimit-Remaining-Tokens": []string{"0"},
			"Anthropic-Ratelimit-Requests": []string{"10"},
			"X-Codex-Plan-Type":            []string{"pro"},
			"X-Request-Id":                 []string{"req_123"},
			"Set-Cookie":                   []string{"secret=1"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)),
	}
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	relayResponse(rec2, req, upstream)
	for _, wantHeader := range []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Remaining-Tokens", "Anthropic-Ratelimit-Requests", "X-Codex-Plan-Type", "X-Request-Id"} {
		if rec2.Header().Get(wantHeader) == "" {
			t.Fatalf("missing relayed header %s: %v", wantHeader, rec2.Header())
		}
	}
	if rec2.Header().Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie must not be relayed: %v", rec2.Header())
	}
}

func TestBatch1RelayBridgeStreamMultilineDataAndEmptyKeepalive(t *testing.T) {
	// Include empty keepalive `data:` lines and a multi-line pretty-printed JSON `data:` frame.
	completed, _ := json.MarshalIndent(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_multi",
			"status": "completed",
			"model":  "upstream-m",
			"output": []any{},
		},
	}, "", "  ")
	var raw bytes.Buffer
	raw.WriteString("data:\n\n")
	raw.WriteString("data:   \n\n")
	raw.WriteString("event: response.completed\n")
	for _, line := range strings.Split(string(completed), "\n") {
		raw.WriteString("data: " + line + "\n")
	}
	raw.WriteString("\n")

	rec := httptest.NewRecorder()
	var recorded map[string]any
	relayBridgeStream(rec, &raw, "gbx/chosen/model-a", "", func(result map[string]any) {
		recorded = result
	})

	if recorded == nil || recorded["id"] != "resp_multi" {
		t.Fatalf("multiline SSE event was not recorded: %#v (body=%s)", recorded, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"gbx/chosen/model-a"`) {
		t.Fatalf("model was not rewritten in relayed multiline event: %s", rec.Body.String())
	}
}
