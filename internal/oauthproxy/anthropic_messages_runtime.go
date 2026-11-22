package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/tidwall/gjson"
)

// anthropicMessagesUpstreamError is a non-2xx Messages response, relayed to
// the client with its status and Retry-After.
type anthropicMessagesUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
	headers    http.Header
}

func (e *anthropicMessagesUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("Anthropic Messages upstream returned HTTP %d: %s", e.status, message)
}

func (e *anthropicMessagesUpstreamError) upstreamStatus() int          { return e.status }
func (e *anthropicMessagesUpstreamError) upstreamRetryAfter() string   { return e.retryAfter }
func (e *anthropicMessagesUpstreamError) upstreamErrorBody() string    { return e.body }
func (e *anthropicMessagesUpstreamError) upstreamHeaders() http.Header { return e.headers }

// anthropicMessagesService serves the Responses front for models whose
// upstream speaks Anthropic Messages. Each model may live on its own base URL
// (models.dev provider.api), falling back to the provider endpoint.
type anthropicMessagesService struct {
	apiKey            string
	endpoint          string
	upstreamKey       string
	models            []string
	modelDetails      map[string]protocol.ModelInfo
	modelRoute        map[string]string
	modelEndpoint     map[string]string
	modelsDevProvider string
	client            *http.Client
	usage             *UsageTracker
}

func newAnthropicMessagesService(apiKey, endpoint string, routes []runtimeModelRoute, endpoints map[string]string, upstreamKey string, usage *UsageTracker) *anthropicMessagesService {
	s := &anthropicMessagesService{
		apiKey: apiKey, endpoint: endpoint, upstreamKey: upstreamKey, usage: usage,
		modelRoute: make(map[string]string, len(routes)), modelEndpoint: make(map[string]string),
		client: newCredentialHTTPClient(90 * time.Second),
	}
	for _, route := range routes {
		key := strings.ToLower(route.Alias)
		if _, exists := s.modelRoute[key]; exists {
			continue
		}
		s.modelRoute[key] = route.Name
		s.models = append(s.models, route.Alias)
		for _, id := range []string{route.Alias, route.Name, stripContextModelSuffix(route.Name)} {
			if base := strings.TrimSpace(endpoints[strings.ToLower(id)]); base != "" {
				s.modelEndpoint[key] = base
				break
			}
		}
	}
	return s
}

// messagesURL is the Messages endpoint for a client model.
func (s *anthropicMessagesService) messagesURL(clientModel string) string {
	base := s.modelEndpoint[strings.ToLower(clientModel)]
	if base == "" {
		base = s.endpoint
	}
	return protocol.NormalizeAnthropicMessagesURL(base)
}

func (s *anthropicMessagesService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	requestCtx = withClientSession(requestCtx, request)
	started := time.Now()
	if !loopbackAuthorized(request, s.apiKey) {
		writeProxyErrorWithCode(writer, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readInboundBody(writer, request, chatMaxBodyBytes)
	if err != nil {
		writeInboundBodyError(writer, err)
		return
	}
	converted, err := s.convertRequest(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "anthropic_messages", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_converted", "component", "anthropic_messages", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "body_bytes", len(converted.body))
	response, err := retryUpstream(requestCtx, "anthropic_messages", func() (*http.Response, error) {
		return s.callOnce(requestCtx, converted)
	})
	if err != nil {
		triedAdaptive := false
		triedStripThinking := false
		for attempt := 0; attempt < 2 && err != nil; attempt++ {
			var upstreamErr *anthropicMessagesUpstreamError
			if !errors.As(err, &upstreamErr) {
				break
			}
			if !triedAdaptive && converted.thinkingEnabled &&
				gjson.GetBytes(converted.body, "thinking.type").String() == "enabled" &&
				isRejectedEnabledThinkingError(upstreamErr.status, upstreamErr.body) {
				triedAdaptive = true
				if updated, ok := switchAnthropicToAdaptiveThinking(converted); ok {
					LogWarnEvent("anthropic_thinking_adaptive_fallback", "component", "anthropic_messages", "request_id", requestID,
						"model", converted.model, "status", upstreamErr.status)
					converted.body = updated
					response, err = retryUpstream(requestCtx, "anthropic_messages", func() (*http.Response, error) {
						return s.callOnce(requestCtx, converted)
					})
					continue
				}
			}
			if !triedStripThinking && (isRejectedThinkingError(upstreamErr.status, upstreamErr.body) || isInvalidAnthropicSignatureError(upstreamErr.status, upstreamErr.body)) {
				triedStripThinking = true
				if updated, ok := stripAnthropicThinkingAndRestoreParams(converted); ok {
					LogWarnEvent("anthropic_thinking_rejected_fallback", "component", "anthropic_messages", "request_id", requestID,
						"model", converted.model, "status", upstreamErr.status)
					converted.body = updated
					converted.thinkingEnabled = false
					response, err = retryUpstream(requestCtx, "anthropic_messages", func() (*http.Response, error) {
						return s.callOnce(requestCtx, converted)
					})
					continue
				}
			}
			break
		}
	}
	if err != nil {
		var upstreamErr *anthropicMessagesUpstreamError
		if errors.As(err, &upstreamErr) {
			copyProxyResponseHeaders(writer.Header(), upstreamErr.headers)
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			if upstreamErr.requestID != "" {
				writer.Header().Set("X-Request-Id", upstreamErr.requestID)
			}
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "anthropic_messages", "request_id", requestID,
				"model", converted.model, "status", upstreamErr.status, "duration", logDuration(started))
			norm := normalizeUpstreamError("anthropic", upstreamErr.status, upstreamErr.body, upstreamErr.Error())
			writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
			return
		}
		LogErrorEvent("request_failed", "component", "anthropic_messages", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
		writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", err.Error())
		return
	}
	defer response.Body.Close()
	copyProxyResponseHeaders(writer.Header(), response.Header)

	// The upstream always streams; a non-streaming client gets the assembled
	// response object.
	var assembler *responseAssembler
	if converted.stream {
		assembler = newResponseAssembler(&converted.adapterRequest, writer)
	} else {
		assembler = newResponseAssembler(&converted.adapterRequest, nil)
	}
	if err := processAnthropicMessagesStream(response.Body, assembler); err != nil {
		if errors.Is(err, context.Canceled) || requestCtx.Err() != nil {
			LogDebugEvent("stream_cancelled", "component", "anthropic_messages", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started))
			if assembler.started {
				s.recordUsage(converted, assembler)
			}
			return
		}
		LogErrorEvent("stream_conversion_failed", "component", "anthropic_messages", "request_id", requestID,
			"model", converted.model, "duration", logDuration(started), "error", err)
		writeStreamFailure(writer, assembler, converted.stream, "anthropic", err)
		if assembler.started {
			s.recordUsage(converted, assembler)
		}
		return
	}
	s.recordUsage(converted, assembler)
	if !converted.stream {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "anthropic_messages", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

func (s *anthropicMessagesService) recordUsage(converted *anthropicConvertedRequest, assembler *responseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

func (s *anthropicMessagesService) callOnce(ctx context.Context, converted *anthropicConvertedRequest) (*http.Response, error) {
	target := s.messagesURL(converted.clientModel)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(converted.body))
	if err != nil {
		return nil, err
	}
	// The AI SDK's Anthropic provider authenticates with x-api-key; models.dev
	// gateways that declare @ai-sdk/anthropic expect the same.
	request.Header.Set("x-api-key", s.upstreamKey)
	request.Header.Set("anthropic-version", protocol.AnthropicVersion)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	applyOpenCodeSession(ctx, request)
	LogDebugEvent("upstream_request", "component", "anthropic_messages", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target),
		"body_bytes", len(converted.body), "model", converted.model)
	started := time.Now()
	response, err := doCredentialRequest(s.client, request)
	if err != nil {
		return nil, fmt.Errorf("call Anthropic Messages upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "anthropic_messages", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "Request-Id", "X-Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
		_ = response.Body.Close()
		DebugHTTPBody(fmt.Sprintf("anthropic messages response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
		return nil, &anthropicMessagesUpstreamError{
			status:     response.StatusCode,
			body:       strings.TrimSpace(string(body)),
			retryAfter: response.Header.Get("Retry-After"),
			requestID:  firstHeader(response.Header, "Request-Id", "X-Request-Id"),
			headers:    extractProxyResponseHeaders(response.Header),
		}
	}
	return response, nil
}

// convertRequest converts one inbound request and applies alias routing,
// model output/thinking capabilities, and reasoning scope binding.
func (s *anthropicMessagesService) convertRequest(raw []byte) (*anthropicConvertedRequest, error) {
	clientModel := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
	upstreamModel := stripContextModelSuffix(clientModel)
	if route := s.modelRoute[strings.ToLower(clientModel)]; route != "" {
		upstreamModel = stripContextModelSuffix(route)
	}
	opts := s.convertOptions(clientModel, upstreamModel)
	converted, err := convertResponsesToAnthropicMessagesWithOptions(raw, opts)
	if err != nil {
		return nil, err
	}
	if err := s.normalizeAnthropicThinking(converted); err != nil {
		return nil, err
	}
	return converted, nil
}

func (s *anthropicMessagesService) reasoningScope(clientModel, upstreamModel string) string {
	target := strings.ToLower(strings.TrimRight(strings.TrimSpace(s.messagesURL(clientModel)), "/"))
	model := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(firstNonEmptyString(upstreamModel, clientModel))))
	if target == "" && model == "" {
		return ""
	}
	return "anthropic:" + target + ":" + model
}

func (s *anthropicMessagesService) convertOptions(clientModel, upstreamModel string) anthropicConvertOptions {
	opts := anthropicConvertOptions{
		overrideModel: upstreamModel,
		scope:         s.reasoningScope(clientModel, upstreamModel),
	}
	for _, candidate := range []string{clientModel, upstreamModel} {
		if info, ok := lookupModelDetail(s.modelDetails, candidate); ok {
			if info.MaxOutputTokens > 0 && opts.maxOutputTokens == 0 {
				opts.maxOutputTokens = info.MaxOutputTokens
			}
			if info.ReasoningKnown && !opts.supportsThinkingKnown {
				opts.supportsThinkingKnown = true
				opts.supportsThinking = len(info.ReasoningLevels) > 0
				for _, lvl := range info.ReasoningLevels {
					opts.efforts = append(opts.efforts, lvl.Effort)
				}
			}
		}
	}
	if index, _ := modelsdev.CachedIndex(); index != nil {
		target := strings.TrimSpace(stripContextModelSuffix(firstNonEmptyString(upstreamModel, clientModel)))
		if opts.maxOutputTokens == 0 {
			if maxOut, ok := index.MaxOutputTokens(s.modelsDevProvider, target, clientModel); ok && maxOut > 0 {
				opts.maxOutputTokens = maxOut
			}
		}
		if supports, ok := index.SupportsThinking(s.modelsDevProvider, target, clientModel); ok {
			opts.supportsThinkingKnown = true
			opts.supportsThinking = supports
		}
		if adaptive, _, ok := index.ThinkingMode(s.modelsDevProvider, target, clientModel); ok && adaptive {
			opts.adaptiveThinking = true
		}
		if len(opts.efforts) == 0 {
			if efforts, ok := index.Efforts(s.modelsDevProvider, target, clientModel); ok && len(efforts) > 0 {
				opts.efforts = efforts
			}
		}
	}
	return opts
}

func (s *anthropicMessagesService) normalizeAnthropicThinking(converted *anthropicConvertedRequest) error {
	if converted == nil || !converted.thinkingEnabled || !gjson.GetBytes(converted.body, "thinking").Exists() {
		return nil
	}
	disable := false
	if index, _ := modelsdev.CachedIndex(); index != nil {
		if supports, ok := index.SupportsThinking(s.modelsDevProvider, converted.model, converted.clientModel); ok {
			if supports {
				return nil
			}
			disable = true
		}
	}
	if !disable {
		if info, ok := lookupModelDetail(s.modelDetails, converted.clientModel); ok && info.ReasoningKnown && len(info.ReasoningLevels) == 0 {
			disable = true
		} else if info, ok := lookupModelDetail(s.modelDetails, converted.model); ok && info.ReasoningKnown && len(info.ReasoningLevels) == 0 {
			disable = true
		}
	}
	if !disable {
		return nil
	}
	if updated, ok := stripAnthropicThinkingAndRestoreParams(converted); ok {
		converted.body = updated
	}
	converted.thinkingEnabled = false
	return nil
}

func switchAnthropicToAdaptiveThinking(converted *anthropicConvertedRequest) ([]byte, bool) {
	var body map[string]any
	if decodeProtocolJSON(converted.body, &body) != nil || body == nil {
		return nil, false
	}
	body["thinking"] = map[string]any{"type": "adaptive"}
	body["output_config"] = map[string]any{"effort": clampAnthropicEffort(converted.requestedEffort, nil)}
	if converted.originalMaxTokens > 0 {
		body["max_tokens"] = converted.originalMaxTokens
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func stripAnthropicThinkingAndRestoreParams(converted *anthropicConvertedRequest) ([]byte, bool) {
	var body map[string]any
	if decodeProtocolJSON(converted.body, &body) != nil || body == nil {
		return nil, false
	}
	changed := false
	if _, exists := body["thinking"]; exists {
		delete(body, "thinking")
		changed = true
	}
	if _, exists := body["output_config"]; exists {
		delete(body, "output_config")
		changed = true
	}
	if converted.originalMaxTokens > 0 {
		body["max_tokens"] = converted.originalMaxTokens
	}
	if _, hasTemp := body["temperature"]; !hasTemp {
		if _, hasTopP := body["top_p"]; !hasTopP {
			applyAnthropicSampling(body, converted.originalTemperature, converted.originalTopP)
		}
	}
	if stripAnthropicHistoryThinkingBlocks(body) {
		changed = true
	}
	if !changed {
		return nil, false
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func stripAnthropicHistoryThinkingBlocks(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	changed := false
	for _, rawMsg := range messages {
		msg, _ := rawMsg.(map[string]any)
		if msg == nil || stringValue(msg["role"]) != "assistant" {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok || len(blocks) == 0 {
			continue
		}
		filtered := make([]any, 0, len(blocks))
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			kind := stringValue(block["type"])
			if kind == "thinking" || kind == "redacted_thinking" {
				changed = true
				continue
			}
			filtered = append(filtered, rawBlock)
		}
		if len(filtered) != len(blocks) {
			if len(filtered) == 0 {
				filtered = []any{map[string]any{"type": "text", "text": "..."}}
			}
			msg["content"] = filtered
		}
	}
	return changed
}

func isRejectedEnabledThinkingError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "adaptive") ||
		(strings.Contains(lower, "budget_tokens") && (strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "deprecated") || strings.Contains(lower, "invalid"))) ||
		(strings.Contains(lower, "thinking") && strings.Contains(lower, "enabled") && (strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported") || strings.Contains(lower, "deprecated")))
}

func isInvalidAnthropicSignatureError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "signature") && (strings.Contains(lower, "thinking") || strings.Contains(lower, "invalid"))
}

func isRejectedThinkingError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "thinking") && !strings.Contains(lower, "output_config") {
		return false
	}
	for _, needle := range []string{"not supported", "unsupported", "unrecognized", "invalid", "not enabled", "cannot be used", "does not support", "unknown field", "extra_forbidden", "must start with a thinking block"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
