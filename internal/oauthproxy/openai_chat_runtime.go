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
	"github.com/tidwall/sjson"
)

const (
	chatMaxBodyBytes     = int64(128 << 20)
	chatMaxErrorBytes    = int64(1 << 20)
	chatMaxResponseBytes = int64(64 << 20)
)

// chatAuthorizer resolves the Bearer token for one upstream request. Static
// API-key gateways return a fixed token; OAuth subscriptions refresh on demand
// and report isOAuth so the data plane can retry once after a 401.
type chatAuthorizer interface {
	authorize(context.Context, bool) (string, error)
	isOAuth() bool
}

// chatStaticAuthorizer pins a single API key for manual gateways.
type chatStaticAuthorizer struct{ token string }

func (a *chatStaticAuthorizer) authorize(context.Context, bool) (string, error) { return a.token, nil }
func (*chatStaticAuthorizer) isOAuth() bool                                     { return false }

// chatCompletionsService is the GBX-owned data plane for an OpenAI Chat
// Completions upstream. Request conversion, SSE conversion, error mapping, and
// usage accounting all live here, independent of external runtime upgrades;
// these details define how openai(chat) behaves.
type chatCompletionsService struct {
	apiKey            string
	endpoint          string
	authorizer        chatAuthorizer
	models            []string
	modelDetails      map[string]protocol.ModelInfo
	modelRoute        map[string]string
	modelEndpoint     map[string]string
	modelsDevProvider string
	client            *http.Client
	usage             *UsageTracker
	// fastRetry controls GBX's hidden 429/5xx replay loop. It defaults to true
	// for ordinary OpenAI-compatible gateways. Subscription brokers that charge
	// per invocation can turn it off so an ambiguous 5xx is never submitted a
	// second time behind the user's back.
	fastRetry bool
	// normalizeModel rewrites the upstream model ID after alias routing. Kimi uses
	// it to strip its "kimi-" prefix and remap legacy code aliases.
	normalizeModel func(string) string
	// decorateHeader adds backend-specific headers before the upstream request.
	// The converted request is included because some gateways route with one
	// model ID in a header while requiring a different ID in the JSON body.
	decorateHeader func(http.Header, *chatCompletionsConvertedRequest)
	// normalizeBody rewrites the marshalled upstream body after alias routing and
	// model normalization but before the request is sent. Kimi uses it to link
	// tool results to tool calls and drop empty assistant messages.
	normalizeBody func([]byte) ([]byte, error)
	// normalizeRequest is the request-aware form of normalizeBody. It may update
	// both the body and the routed model, which is required when a backend routes
	// multimodal requests through a dedicated model/header pair.
	normalizeRequest func(*chatCompletionsConvertedRequest) error
	// roundTrip optionally replaces the default HTTP POST + 401 refresh inside
	// one fast-retry attempt (used by single-hop WorkBuddy).
	roundTrip func(context.Context, *chatCompletionsConvertedRequest) (*http.Response, error)
}

type chatCompletionsUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
	headers    http.Header
}

func (e *chatCompletionsUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("OpenAI Chat Completions upstream returned HTTP %d: %s", e.status, message)
}

// upstreamStatus implements the retry.go fast-retry probe against the raw
// upstream status, before any client-facing mapping.
func (e *chatCompletionsUpstreamError) upstreamStatus() int          { return e.status }
func (e *chatCompletionsUpstreamError) upstreamRetryAfter() string   { return e.retryAfter }
func (e *chatCompletionsUpstreamError) upstreamErrorBody() string    { return e.body }
func (e *chatCompletionsUpstreamError) upstreamHeaders() http.Header { return e.headers }

// startOpenAIChatRuntime starts a GBX-owned Responses entrypoint that
// translates requests to OpenAI Chat Completions upstream. It mirrors
// startCodexResponsesRuntime's loopback/server lifecycle with no OAuth and no
// compaction, since neither applies to a static-key Chat gateway.
func startOpenAIChatRuntime(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	return startOpenAIChatRuntimeWithProvider(parent, endpoint, upstreamAPIKey, modelSpec, "")
}

func startOpenAIChatRuntimeWithProvider(parent context.Context, endpoint, upstreamAPIKey, modelSpec, providerID string) (*Runtime, error) {
	endpoint = normalizeOpenAIBaseURL(endpoint)
	upstreamAPIKey = strings.TrimSpace(upstreamAPIKey)
	if endpoint == "" || upstreamAPIKey == "" {
		return nil, fmt.Errorf("OpenAI Chat runtime requires endpoint and API key")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	return startOpenAIChatRuntimeService(parent, endpoint, routes, &chatStaticAuthorizer{token: upstreamAPIKey}, func(service *chatCompletionsService) {
		service.modelsDevProvider = strings.TrimSpace(providerID)
	})
}

// startOpenAIChatRuntimeWithAuthorizer starts the same Chat Completions data
// plane but resolves the upstream Bearer token through an authorizer, so OAuth
// subscription backends (grok/kimi/workbuddy) can reuse it unchanged.
func startOpenAIChatRuntimeWithAuthorizer(parent context.Context, endpoint, modelSpec string, authorizer chatAuthorizer) (*Runtime, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	return startOpenAIChatRuntimeRoutes(parent, endpoint, routes, authorizer)
}

// startOpenAIChatRuntimeRoutes starts the Chat Completions data plane against an
// explicit route list. Backends whose alias→name mapping cannot be expressed as
// a CSV model spec (e.g. WorkBuddy, which derives routes from a live catalog)
// call this directly instead of startOpenAIChatRuntimeWithAuthorizer.
func startOpenAIChatRuntimeRoutes(parent context.Context, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer) (*Runtime, error) {
	return startOpenAIChatRuntimeService(parent, endpoint, routes, authorizer, nil)
}

// startOpenAIChatRuntimeService is the shared server lifecycle. configure, when
// non-nil, is called with the fully-built service before the server starts, so
// backends like Kimi can install a model normalizer and header decorator.
func startOpenAIChatRuntimeService(parent context.Context, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer, configure func(*chatCompletionsService)) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("OpenAI Chat runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	usage := NewUsageTracker()
	service := newChatCompletionsServiceWithAuthorizer(apiKey, endpoint, routes, authorizer, usage)
	if configure != nil {
		configure(service)
	}
	runtime, err := startLoopbackServer(parent, "OpenAI Chat runtime", service.handlerForContext(parent), apiKey, usage)
	if err != nil {
		return nil, err
	}
	runtime.models = append([]string(nil), service.models...)
	runtime.cleanup = append(runtime.cleanup, closeIdleClients(service.client), closeIdleResource(authorizer))
	return runtime, nil
}

// newChatCompletionsService keeps the static-key convenience signature for the
// API-key gateways (StartOpenAIChatAPI, mixed chat bucket) by wrapping a
// chatStaticAuthorizer. OAuth backends use newChatCompletionsServiceWithAuthorizer.
func newChatCompletionsService(apiKey, endpoint string, routes []runtimeModelRoute, upstreamAPIKey string, usage *UsageTracker) *chatCompletionsService {
	return newChatCompletionsServiceWithAuthorizer(apiKey, endpoint, routes, &chatStaticAuthorizer{token: upstreamAPIKey}, usage)
}

func newChatCompletionsServiceWithAuthorizer(apiKey, endpoint string, routes []runtimeModelRoute, authorizer chatAuthorizer, usage *UsageTracker) *chatCompletionsService {
	models := make([]string, 0, len(routes))
	modelRoute := make(map[string]string, len(routes))
	seenModels := make(map[string]bool, len(routes))
	for _, route := range routes {
		key := strings.ToLower(route.Alias)
		if _, exists := modelRoute[key]; !exists {
			modelRoute[key] = route.Name
		}
		if !seenModels[key] {
			models = append(models, route.Alias)
			seenModels[key] = true
		}
	}
	return &chatCompletionsService{
		apiKey: apiKey, endpoint: endpoint, authorizer: authorizer,
		models: models, modelRoute: modelRoute, usage: usage, fastRetry: true,
		client: newCredentialHTTPClient(90 * time.Second),
	}
}

func (s *chatCompletionsService) setModelEndpoints(routes []runtimeModelRoute, endpoints map[string]string) {
	if len(endpoints) == 0 {
		return
	}
	resolved := make(map[string]string)
	for _, route := range routes {
		for _, id := range []string{route.Alias, route.Name, stripContextModelSuffix(route.Name)} {
			if base := strings.TrimSpace(endpoints[strings.ToLower(id)]); base != "" {
				resolved[strings.ToLower(route.Alias)] = base
				resolved[strings.ToLower(route.Name)] = base
				break
			}
		}
	}
	if len(resolved) > 0 {
		s.modelEndpoint = resolved
	}
}

func (s *chatCompletionsService) chatURL(converted *chatCompletionsConvertedRequest) string {
	base := ""
	if converted != nil && len(s.modelEndpoint) > 0 {
		for _, id := range []string{converted.clientModel, converted.model, converted.upstreamModel, stripContextModelSuffix(converted.model)} {
			if candidate := s.modelEndpoint[strings.ToLower(strings.TrimSpace(id))]; candidate != "" {
				base = candidate
				break
			}
		}
	}
	if base == "" {
		base = s.endpoint
	}
	return protocol.NormalizeOpenAIChatCompletionsURL(base)
}

func (s *chatCompletionsService) handler() http.Handler {
	return s.handlerForContext(context.Background())
}

func (s *chatCompletionsService) handlerForContext(ctx context.Context) http.Handler {
	return loopbackFrontMuxForContext(ctx, s.apiKey, s.handleModels, s.handleResponses)
}

func (s *chatCompletionsService) authorized(request *http.Request) bool {
	return loopbackAuthorized(request, s.apiKey)
}

func (s *chatCompletionsService) handleModels(writer http.ResponseWriter, request *http.Request) {
	serveModelListWithProvider(writer, request, s.apiKey, s.models, s.modelDetails, s.modelRoute, s.modelsDevProvider)
}

func (s *chatCompletionsService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	requestCtx = withClientSession(requestCtx, request)
	started := time.Now()
	LogDebugEvent("request_received", "component", "openai_chat", "request_id", requestID,
		"path", request.URL.Path, "method", request.Method)
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "openai_chat", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
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
		LogWarnEvent("request_rejected", "component", "openai_chat", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	LogDebugEvent("request_converted", "component", "openai_chat", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "body_bytes", len(converted.body),
		"content_structure", chatContentStructure(converted.body))
	response, err := s.call(requestCtx, converted)
	if err != nil {
		var upstreamErr *chatCompletionsUpstreamError
		if errors.As(err, &upstreamErr) && gjson.GetBytes(converted.body, "reasoning_effort").Exists() && isRejectedReasoningEffortError(upstreamErr.status, upstreamErr.body) {
			if stripped, stripErr := sjson.DeleteBytes(converted.body, "reasoning_effort"); stripErr == nil {
				LogWarnEvent("reasoning_effort_rejected_fallback", "component", "openai_chat", "request_id", requestID,
					"model", converted.model, "status", upstreamErr.status)
				converted.body = stripped
				converted.thinkingEnabled = false
				response, err = s.call(requestCtx, converted)
			}
		}
	}
	if err != nil {
		var upstreamErr *chatCompletionsUpstreamError
		if errors.As(err, &upstreamErr) {
			copyProxyResponseHeaders(writer.Header(), upstreamErr.headers)
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			if upstreamErr.requestID != "" {
				writer.Header().Set("X-Request-Id", upstreamErr.requestID)
			}
			LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
				"retry_after", upstreamErr.retryAfter, "duration", logDuration(started))
			norm := normalizeUpstreamError("openai_chat", upstreamErr.status, upstreamErr.body, upstreamErr.Error())
			writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
			return
		}
		var authErr *oauthAuthError
		if errors.As(err, &authErr) {
			status := authErr.status
			if status == 0 {
				status = http.StatusUnauthorized
			}
			LogUpstreamEvent(status, "request_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "status", status, "returned_status", status, "duration", logDuration(started))
			writeProxyErrorWithCode(writer, status, proxyErrorType(status), "invalid_api_key", authErr.Error())
			return
		}
		LogErrorEvent("request_failed", "component", "openai_chat", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
		writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", err.Error())
		return
	}
	defer response.Body.Close()
	copyProxyResponseHeaders(writer.Header(), response.Header)
	if converted.stream {
		assembler := newResponseAssembler(&converted.adapterRequest, writer)
		if err := processChatCompletionsStream(response.Body, assembler); err != nil {
			if errors.Is(err, context.Canceled) || requestCtx.Err() != nil {
				LogDebugEvent("stream_cancelled", "component", "openai_chat", "request_id", requestID,
					"model", converted.model, "duration", logDuration(started))
				if assembler.started {
					s.recordUsage(converted, assembler)
				}
				return
			}
			LogErrorEvent("stream_conversion_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", err)
			writeStreamFailure(writer, assembler, true, "openai_chat", err)
			if assembler.started {
				s.recordUsage(converted, assembler)
			}
			return
		}
		s.recordUsage(converted, assembler)
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, chatMaxResponseBytes+1))
		if readErr != nil {
			LogErrorEvent("response_read_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", readErr)
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", readErr.Error())
			return
		}
		if int64(len(body)) > chatMaxResponseBytes {
			limitErr := fmt.Errorf("OpenAI Chat Completions response exceeds %s limit", requestLimitLabel(chatMaxResponseBytes))
			LogErrorEvent("response_read_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", limitErr)
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", limitErr.Error())
			return
		}
		assembler := newResponseAssembler(&converted.adapterRequest, nil)
		if err := processChatCompletionsNonStream(body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "openai_chat", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
			writeStreamFailure(writer, assembler, false, "openai_chat", err)
			return
		}
		s.recordUsage(converted, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "openai_chat", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

func (s *chatCompletionsService) recordUsage(converted *chatCompletionsConvertedRequest, assembler *responseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

// call runs one upstream round-trip inside the shared fast-retry loop: a 429
// or 5xx outcome is retried twice more (500ms/1s) before being returned
// as-is, letting Claude Code do its own full backoff over the relayed
// status/body/Retry-After. The 401 refresh below is part of one attempt.
func (s *chatCompletionsService) call(ctx context.Context, converted *chatCompletionsConvertedRequest) (*http.Response, error) {
	attempt := func() (*http.Response, error) {
		var response *http.Response
		var err error
		if s.roundTrip != nil {
			response, err = s.roundTrip(ctx, converted)
			if err != nil {
				return nil, err
			}
		} else {
			response, err = s.callOnce(ctx, converted)
			if err != nil {
				return nil, err
			}
			if response.StatusCode == http.StatusUnauthorized && s.authorizer != nil && s.authorizer.isOAuth() {
				// OAuth subscription: a stale token 401s once, then we force-refresh and
				// retry the request exactly once before surfacing the failure.
				rejectedCtx := rejectedResponseContext(ctx, response)
				rejectedErr := chatDrainError(ctx, response)
				if _, authErr := s.authorizer.authorize(rejectedCtx, true); authErr != nil {
					return nil, &oauthAuthError{
						status:  http.StatusUnauthorized,
						message: fmt.Sprintf("%v (refresh OpenAI Chat OAuth token failed: %v)", rejectedErr, authErr),
						err:     authErr,
					}
				}
				response, err = s.callOnce(ctx, converted)
				if err != nil {
					return nil, err
				}
			}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, chatDrainError(ctx, response)
		}
		return response, nil
	}
	if !s.fastRetry {
		return attempt()
	}
	return retryUpstream(ctx, "openai_chat", attempt)
}

func (s *chatCompletionsService) callOnce(ctx context.Context, converted *chatCompletionsConvertedRequest) (*http.Response, error) {
	target := s.chatURL(converted)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(converted.body))
	if err != nil {
		return nil, err
	}
	token, err := s.authorizer.authorize(ctx, false)
	if err != nil {
		return nil, wrapOAuthAuthorizeError("authorize OpenAI Chat Completions upstream", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if converted.stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	if s.decorateHeader != nil {
		s.decorateHeader(request.Header, converted)
	}
	applyOpenCodeSession(ctx, request)
	LogDebugEvent("upstream_request", "component", "openai_chat", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target),
		"body_bytes", len(converted.body), "stream", converted.stream, "model", converted.model)
	started := time.Now()
	response, err := doCredentialRequest(s.client, request)
	if err != nil {
		return nil, fmt.Errorf("call OpenAI Chat Completions upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "openai_chat", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "duration", logDuration(started))
	return response, nil
}

func chatDrainError(ctx context.Context, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("openai chat response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return &chatCompletionsUpstreamError{
		status:     response.StatusCode,
		body:       strings.TrimSpace(string(body)),
		retryAfter: response.Header.Get("Retry-After"),
		requestID:  firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		headers:    extractProxyResponseHeaders(response.Header),
	}
}

// convertRequest converts one inbound request and applies alias routing and
// backend-specific normalization.
func (s *chatCompletionsService) convertRequest(raw []byte) (*chatCompletionsConvertedRequest, error) {
	converted, err := convertResponsesToChatCompletions(raw)
	if err != nil {
		return nil, err
	}
	if route := s.modelRoute[strings.ToLower(converted.clientModel)]; route != "" {
		if s.normalizeModel != nil {
			route = s.normalizeModel(route)
		}
		converted.model, converted.upstreamModel = route, route
		var body map[string]any
		if err := decodeProtocolJSON(converted.body, &body); err != nil {
			return nil, err
		}
		body["model"] = route
		converted.body, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	if err := s.normalizeChatReasoning(converted); err != nil {
		return nil, err
	}
	if s.normalizeBody != nil {
		converted.body, err = s.normalizeBody(converted.body)
		if err != nil {
			return nil, err
		}
	}
	if s.normalizeRequest != nil {
		if err := s.normalizeRequest(converted); err != nil {
			return nil, err
		}
	}
	return converted, nil
}

func (s *chatCompletionsService) normalizeChatReasoning(converted *chatCompletionsConvertedRequest) error {
	if converted == nil {
		return nil
	}
	effortField := gjson.GetBytes(converted.body, "reasoning_effort")
	if !effortField.Exists() {
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(effortField.String()))
	if effort == "" {
		return nil
	}
	levels, known := s.knownChatReasoningLevels(converted.clientModel, converted.model)
	if !known {
		return nil
	}
	if len(levels) == 0 {
		stripped, err := sjson.DeleteBytes(converted.body, "reasoning_effort")
		if err != nil {
			return err
		}
		converted.body = stripped
		converted.thinkingEnabled = false
		return nil
	}
	clamped := protocol.ClampReasoningLevel(effort, levels)
	if clamped == "" {
		stripped, err := sjson.DeleteBytes(converted.body, "reasoning_effort")
		if err != nil {
			return err
		}
		converted.body = stripped
		converted.thinkingEnabled = false
		return nil
	}
	if clamped != effort {
		updated, err := sjson.SetBytes(converted.body, "reasoning_effort", clamped)
		if err != nil {
			return err
		}
		converted.body = updated
	}
	return nil
}

func (s *chatCompletionsService) knownChatReasoningLevels(clientModel, upstreamModel string) ([]protocol.ReasoningLevel, bool) {
	for _, candidate := range []string{clientModel, upstreamModel} {
		if info, ok := lookupModelDetail(s.modelDetails, candidate); ok && info.ReasoningKnown {
			return info.ReasoningLevels, true
		}
	}
	for _, candidate := range []string{clientModel, upstreamModel} {
		lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(candidate)))
		if hasGeminiEffortSuffix(lower) || lower == "kimi-for-coding-highspeed" {
			return nil, true
		}
	}
	if index, _ := modelsdev.CachedIndex(); index != nil {
		target := strings.TrimSpace(stripContextModelSuffix(upstreamModel))
		if target == "" {
			target = strings.TrimSpace(stripContextModelSuffix(clientModel))
		}
		if efforts, found := index.Efforts(s.modelsDevProvider, target, clientModel); found {
			levels, _ := reasoningLevelsFromEfforts(efforts, "")
			return levels, true
		}
	}
	return nil, false
}

func isRejectedReasoningEffortError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "reasoning_effort") && !strings.Contains(lower, "reasoning effort") {
		return false
	}
	for _, needle := range []string{"not supported", "unsupported", "unrecognized", "invalid", "unknown", "does not support", "extra_forbidden", "cannot be used", "not allowed"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
