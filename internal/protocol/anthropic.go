package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicVersion is the Messages API version header GBX sends.
const AnthropicVersion = "2023-06-01"

// NormalizeAnthropicMessagesURL turns a base URL into its Messages endpoint.
// Like the AI SDK's Anthropic provider, a base ends at the API version
// ("…/v1") and "/messages" is appended.
func NormalizeAnthropicMessagesURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/messages") {
		return base
	}
	return base + "/messages"
}

// ProbeAnthropicMessagesStatusContext sends a one-token Messages request and
// reports the HTTP status (0 on transport error). It authenticates with
// x-api-key, as @ai-sdk/anthropic does.
func ProbeAnthropicMessagesStatusContext(parent context.Context, endpoint, apiKey, model string, timeout time.Duration) (int, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeAnthropicMessagesURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", AnthropicVersion)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
