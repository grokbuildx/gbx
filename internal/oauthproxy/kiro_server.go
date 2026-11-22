package oauthproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
)

const (
	kiroIDEVersion             = "2.3.0"
	kiroMaxInboundRequestBytes = int64(128 << 20)
	kiroMaxUpstreamErrorBytes  = int64(1 << 20)
)

// kiroRateLimitBackoff is how long to wait before each retry of a rate-limited
// request. Kiro answers short bursts with HTTP 429 / USER_REQUEST_RATE_EXCEEDED,
// which normally clears within seconds, so the turn is retried here instead of
// being handed straight back to the user.
var kiroRateLimitBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

type kiroService struct {
	apiKey       string
	models       []string
	modelCatalog *kiroModelCatalog
	pool         *kiroCredentialPool
	client       *http.Client
	upstreamURL  func(*kiroCredential) string
	// rateLimitBackoff overrides kiroRateLimitBackoff (tests only).
	rateLimitBackoff []time.Duration
	// usage accumulates per-model token totals for this runtime.
	usage *UsageTracker
}

type kiroUpstreamError struct {
	status     int
	body       string
	retryAfter string
	headers    http.Header
}

func (e *kiroUpstreamError) Error() string {
	return fmt.Sprintf("Kiro upstream returned HTTP %d: %s", e.status, e.body)
}

func (e *kiroUpstreamError) upstreamStatus() int          { return e.status }
func (e *kiroUpstreamError) upstreamRetryAfter() string   { return e.retryAfter }
func (e *kiroUpstreamError) upstreamErrorBody() string    { return e.body }
func (e *kiroUpstreamError) upstreamHeaders() http.Header { return e.headers }

func startKiroOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	pool := newKiroCredentialPool(authDir, credentialFile)
	credentials, err := pool.load()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no %s credentials found; run `gbx oauth %s` first", ProviderKiro, ProviderKiro)
	}

	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	models := kiroRuntimeModels(modelSpec)
	usageTracker := NewUsageTracker()
	service := &kiroService{
		apiKey:       apiKey,
		models:       models,
		modelCatalog: newKiroModelCatalog(kiroAvailableModelsEndpoint),
		pool:         pool,
		usage:        usageTracker,
		client:       newCredentialHTTPClient(60 * time.Second),
		upstreamURL: func(credential *kiroCredential) string {
			return "https://q." + credential.effectiveAPIRegion() + ".amazonaws.com/generateAssistantResponse"
		},
	}
	proxyRuntime, err := startLoopbackServer(parent, "Kiro runtime", service.handlerForContext(parent), apiKey, usageTracker)
	if err != nil {
		return nil, err
	}
	proxyRuntime.listAuths = pool.listAuths
	// The service's own catalog is loaded lazily by /v1/models, but Runtime
	// must carry the configured models too: providersession.Prepare and the
	// launcher read Runtime.Models() without asking the runtime to start.
	proxyRuntime.models = models
	proxyRuntime.modelDetails = kiroInitialModelDetails(modelSpec, models)
	proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(service.client, pool.client))
	LogInfof("runtime start oauth provider=kiro backend=kiro protocol=responses port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(proxyRuntime.endpoint, "/v1"), "http://"), filepath.Base(credentialFile), len(models))
	return proxyRuntime, nil
}

// kiroRuntimeModels lists the client-visible model aliases of a model spec.
func kiroRuntimeModels(modelSpec string) []string {
	if strings.TrimSpace(modelSpec) == "" {
		modelSpec = "claude-opus-4-6,claude-sonnet-4-6,claude-haiku-4-5"
	}
	return runtimeModelAliases(modelSpec)
}

func kiroInitialModelDetails(modelSpec string, models []string) map[string]protocol.ModelInfo {
	index, _ := modelsdev.CachedIndex()
	routes := make(map[string]string, len(models))
	for _, route := range runtimeModelRoutes(modelSpec) {
		routes[strings.ToLower(strings.TrimSpace(route.Alias))] = route.Name
	}
	details := make(map[string]protocol.ModelInfo, len(models))
	for _, alias := range models {
		trimmed := strings.TrimSpace(alias)
		if trimmed == "" {
			continue
		}
		target := routes[strings.ToLower(trimmed)]
		if target == "" {
			target = trimmed
		}
		mapped := mapKiroModel(target)
		var info protocol.ModelInfo
		matched := false
		if index != nil {
			if modalities, ok := index.InputModalities("anthropic", mapped, target); ok {
				info.InputModalities = modalities
				matched = true
			}
			if maxOut, ok := index.MaxOutputTokens("anthropic", mapped, target); ok {
				info.MaxOutputTokens = maxOut
				matched = true
			}
			if efforts, ok := index.Efforts("anthropic", mapped, target); ok {
				info.ReasoningLevels, info.DefaultReasoningLevel = reasoningLevelsFromEfforts(efforts, "high")
				info.ReasoningKnown = true
				matched = true
			}
		}
		if len(info.InputModalities) == 0 && kiroKnownVisionModel(mapped) {
			info.InputModalities = []string{"text", "image"}
			matched = true
		}
		if !info.ReasoningKnown && kiroKnownEffortModel(mapped) {
			info.ReasoningLevels, info.DefaultReasoningLevel = reasoningLevelsFromEfforts([]string{"low", "medium", "high", "max"}, "high")
			info.ReasoningKnown = true
			matched = true
		}
		if matched {
			info.ID = trimmed
			details[strings.ToLower(trimmed)] = info
		}
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func kiroKnownEffortModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	return strings.HasPrefix(lower, "claude-opus-4") || strings.HasPrefix(lower, "claude-sonnet-4")
}

func kiroKnownVisionModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	for _, prefix := range []string{
		"claude-3-5", "claude-3.5", "claude-3-7", "claude-3.7",
		"claude-4", "claude-sonnet-4", "claude-opus-4", "claude-haiku-4",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func (s *kiroService) handler() http.Handler {
	return s.handlerForContext(context.Background())
}

func (s *kiroService) handlerForContext(ctx context.Context) http.Handler {
	return loopbackFrontMuxForContext(ctx, s.apiKey, s.handleModels, s.handleResponses)
}

func (s *kiroService) authorized(request *http.Request) bool {
	return loopbackAuthorized(request, s.apiKey)
}

func (s *kiroService) handleModels(writer http.ResponseWriter, request *http.Request) {
	if !modelsRequestAllowed(writer, request, s.apiKey) {
		return
	}
	models, err := s.availableModels(request.Context())
	if err != nil {
		LogErrorf("kiro models discovery failed error=%v", err)
		writeProxyError(writer, http.StatusBadGateway, "api_error", "Unable to load available models from Kiro: "+err.Error())
		return
	}
	index, _ := modelsdev.CachedIndex()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		displayName := strings.TrimSpace(model.ModelName)
		if displayName == "" {
			displayName = model.ModelID
		}
		item := modelListEntry(model.ModelID)
		item["display_name"] = displayName
		if model.Description != "" {
			item["description"] = model.Description
		}
		if model.TokenLimits != nil {
			if model.TokenLimits.MaxInputTokens > 0 {
				item["max_input_tokens"] = model.TokenLimits.MaxInputTokens
				item["context_window"] = model.TokenLimits.MaxInputTokens
			}
			if model.TokenLimits.MaxOutputTokens > 0 {
				item["max_output_tokens"] = model.TokenLimits.MaxOutputTokens
			}
		}
		if model.RateMultiplier != nil {
			item["rate_multiplier"] = *model.RateMultiplier
		}
		if model.RateUnit != "" {
			item["rate_unit"] = model.RateUnit
		}
		if len(model.SupportedInputTypes) > 0 {
			item["supported_input_types"] = model.SupportedInputTypes
			if modalities := normalizeModelModalities(model.SupportedInputTypes); len(modalities) > 0 {
				item["input_modalities"] = modalities
			}
		}
		mapped := mapKiroModel(model.ModelID)
		var levels []protocol.ReasoningLevel
		var defaultLevel string
		known := false
		if index != nil {
			if efforts, ok := index.Efforts("anthropic", mapped, displayName); ok {
				levels, defaultLevel = reasoningLevelsFromEfforts(efforts, "high")
				known = true
			}
		}
		if !known && kiroKnownEffortModel(mapped) {
			levels, defaultLevel = reasoningLevelsFromEfforts([]string{"low", "medium", "high", "max"}, "high")
			known = true
		}
		if !known {
			levels, defaultLevel, known = resolveFallbackModelReasoning(index, "anthropic", mapped, displayName)
		}
		applyModelEntryReasoning(item, levels, defaultLevel, known)
		data = append(data, item)
	}
	writeModelList(writer, data)
}

func (s *kiroService) cachedContextWindow(model string) int {
	if s.modelCatalog == nil {
		return 0
	}
	s.modelCatalog.mu.Lock()
	defer s.modelCatalog.mu.Unlock()
	for _, entry := range s.modelCatalog.cache {
		for _, item := range entry.models {
			if strings.EqualFold(strings.TrimSpace(item.ModelID), strings.TrimSpace(model)) && item.TokenLimits != nil && item.TokenLimits.MaxInputTokens > 0 {
				return int(item.TokenLimits.MaxInputTokens)
			}
		}
	}
	return 0
}

func (s *kiroService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeProxyErrorWithCode(writer, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "method", request.Method, "status", http.StatusMethodNotAllowed,
			"reason", "unsupported_method")
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	LogDebugEvent("request_received", "component", "kiro", "request_id", requestID,
		"path", request.URL.RequestURI(), "content_length", request.ContentLength,
		"transfer_encoding_count", len(request.TransferEncoding), "content_type", request.Header.Get("Content-Type"),
		"content_encoding", request.Header.Get("Content-Encoding"))
	raw, err := readInboundBody(writer, request, kiroMaxInboundRequestBytes)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "read_body", "error", err)
		writeInboundBodyError(writer, err)
		return
	}
	LogDebugEvent("request_body_read", "component", "kiro", "request_id", requestID, "body_bytes", len(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "empty_body")
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request: request body is empty")
		return
	}
	converted, err := convertResponsesToKiro(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "kiro", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if window := s.cachedContextWindow(converted.model); window > 0 {
		converted.contextWindow = window
	}
	if converted.droppedMedia > 0 || converted.dedupedMedia > 0 ||
		converted.resizedMedia > 0 || converted.correctedMedia > 0 {
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "inline_media", "kept", converted.inlineMedia, "capped", converted.droppedMedia,
			"deduplicated", converted.dedupedMedia, "resized", converted.resizedMedia,
			"mime_corrected", converted.correctedMedia, "limit", kiroMaxInlineMediaSegments)
	}
	if converted.droppedToolUses > 0 || converted.droppedToolRuns > 0 || converted.emptyToolRuns > 0 {
		// dropped_results > 0 means the model will not see those tool outputs at
		// all, which it reports as the tooling being broken; empty_results are
		// forwarded with a placeholder instead of an empty string.
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "tool_pairing", "dropped_uses", converted.droppedToolUses,
			"dropped_results", converted.droppedToolRuns, "empty_results", converted.emptyToolRuns)
	}
	if converted.truncatedTexts > 0 {
		LogWarnEvent("request_normalized", "component", "kiro", "request_id", requestID,
			"kind", "content_truncation", "fields", converted.truncatedTexts,
			"dropped_bytes", converted.droppedText, "largest_original_bytes", converted.largestText,
			"limit", kiroMaxTextFieldBytes)
	}
	LogDebugEvent("request_converted", "component", "kiro", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model, "stream", converted.stream,
		"body_bytes", len(raw))
	upstream, err := s.callUpstream(requestCtx, converted)
	if err != nil {
		var upstreamErr *kiroUpstreamError
		if errors.As(err, &upstreamErr) {
			copyProxyResponseHeaders(writer.Header(), upstreamErr.headers)
			if upstreamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", upstreamErr.retryAfter)
			}
			if upstreamErr.status >= 400 && upstreamErr.status < 500 {
				norm := normalizeUpstreamError("kiro", upstreamErr.status, upstreamErr.body, err.Error())
				LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "kiro", "request_id", requestID,
					"model", converted.model, "status", upstreamErr.status, "returned_status", norm.Status,
					"error_type", norm.Type, "stream", converted.stream, "duration", logDuration(started))
				// err, not upstreamErr: the wrapper carries extra context such as a
				// failed token refresh.
				writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
				return
			}
		}
		var authErr *oauthAuthError
		if errors.As(err, &authErr) {
			status := authErr.status
			if status == 0 {
				status = http.StatusUnauthorized
			}
			LogUpstreamEvent(status, "request_failed", "component", "kiro", "request_id", requestID,
				"model", converted.model, "status", status, "returned_status", status, "stream", converted.stream,
				"duration", logDuration(started))
			writeProxyErrorWithCode(writer, status, proxyErrorType(status), "invalid_api_key", authErr.Error())
			return
		}
		// Everything else is a proxy-side or upstream server failure.
		LogErrorEvent("request_failed", "component", "kiro", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway, "stream", converted.stream,
			"duration", logDuration(started), "error", err)
		writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", err.Error())
		return
	}
	defer upstream.Body.Close()
	copyProxyResponseHeaders(writer.Header(), upstream.Header)

	if converted.stream {
		assembler := newResponseAssembler(&converted.adapterRequest, writer)
		streamErr := processKiroEventStream(upstream.Body, assembler)
		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) || requestCtx.Err() != nil {
				LogDebugEvent("stream_cancelled", "component", "kiro", "request_id", requestID,
					"model", converted.model, "duration", logDuration(started))
				if assembler.started {
					s.recordKiroUsage(converted, assembler)
				}
				return
			}
			LogErrorEvent("stream_conversion_failed", "component", "kiro", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", streamErr)
			writeStreamFailure(writer, assembler, true, "kiro", streamErr)
			if assembler.started {
				s.recordKiroUsage(converted, assembler)
			}
			return
		}
		s.recordKiroUsage(converted, assembler)
		LogDebugEvent("request_complete", "component", "kiro", "request_id", requestID,
			"model", converted.model, "status", http.StatusOK, "stream", true, "duration", logDuration(started))
		return
	}

	assembler := newResponseAssembler(&converted.adapterRequest, nil)
	if err := processKiroEventStream(upstream.Body, assembler); err != nil {
		LogErrorEvent("stream_conversion_failed", "component", "kiro", "request_id", requestID,
			"model", converted.model, "returned_status", http.StatusBadGateway,
			"duration", logDuration(started), "error", err)
		writeStreamFailure(writer, assembler, false, "kiro", err)
		return
	}
	s.recordKiroUsage(converted, assembler)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
	LogDebugEvent("request_complete", "component", "kiro", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", false, "duration", logDuration(started))
}

// recordKiroUsage adds the token totals of one completed turn to the runtime's
// usage tracker, keyed by the model the client requested (the [1m]-suffixed
// alias when present) rather than the upstream model id, so it matches the slot
// name Claude Code shows the user.
func (s *kiroService) recordKiroUsage(converted *kiroConvertedRequest, assembler *responseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), 0, 0)
}

func readInboundBody(writer http.ResponseWriter, request *http.Request, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("invalid request body limit")
	}
	if request.ContentLength > maxBytes {
		return nil, &requestBodyTooLargeError{limitLabel: requestLimitLabel(maxBytes)}
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, &requestBodyTooLargeError{limitLabel: requestLimitLabel(maxBytes)}
		}
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return raw, nil
}

// drainKiroErrorBody reads a bounded prefix of an upstream error body and closes
// it, so the diagnostics survive after the response is discarded.
func drainKiroErrorBody(ctx context.Context, response *http.Response) string {
	if response == nil || response.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, kiroMaxUpstreamErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("kiro response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return strings.TrimSpace(string(body))
}

func requestLimitLabel(maxBytes int64) string {
	if maxBytes >= 1<<20 && maxBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", maxBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxBytes)
}

// callUpstream sends the converted request upstream, retrying while the account
// is rate limited or hits transient 5xx failures. Every credential is tried
// before a retry sleeps, so rotating to a second account is always preferred
// over waiting. If Retry-After exceeds the next backoff step, return immediately
// so the client can honor the cooldown.
func (s *kiroService) callUpstream(ctx context.Context, converted *kiroConvertedRequest) (*http.Response, error) {
	backoff := s.rateLimitBackoff
	if backoff == nil {
		backoff = kiroRateLimitBackoff
	}
	for attempt := 0; ; attempt++ {
		response, err := s.callUpstreamOnce(ctx, converted, attempt+1)
		if err == nil {
			return response, nil
		}
		if !isKiroRetryableError(err) || attempt >= len(backoff) {
			return nil, err
		}
		baseDelay := backoff[attempt]
		if hint, ok := outcomeRetryAfterDelay(nil, err); ok && hint > baseDelay {
			LogWarnEvent("upstream_retry", "component", "kiro", "request_id", requestLogID(ctx),
				"model", converted.model, "status", outcomeStatus(nil, err), "action", "relay_retry_after")
			return nil, err
		}
		delay := jitterBackoffDelay(baseDelay)
		LogWarnEvent("upstream_retry", "component", "kiro", "request_id", requestLogID(ctx),
			"model", converted.model, "status", outcomeStatus(nil, err),
			"retry", attempt+1, "retry_limit", len(backoff), "wait", delay, "action", "retry_all_credentials")
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			// The client gave up: report the upstream error, not the cancellation.
			return nil, err
		}
	}
}

// isKiroRetryableError reports whether err is a transient Kiro 429 or 5xx error.
// Permanent quota exhaustion (such as MONTHLY_REQUEST_COUNT) is excluded.
func isKiroRetryableError(err error) bool {
	var upstreamErr *kiroUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if !isFastRetryStatus(upstreamErr.status) {
		return false
	}
	if upstreamErr.status == http.StatusTooManyRequests && isNonRetryableQuotaBody(upstreamErr.body) {
		return false
	}
	return true
}

// callUpstreamOnce tries every selected credential once and returns the first
// successful response.
func (s *kiroService) callUpstreamOnce(ctx context.Context, converted *kiroConvertedRequest, round int) (*http.Response, error) {
	credentials, err := s.pool.orderedCredentials()
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no usable Kiro credentials")
	}
	var lastErr error
	var retryableErr error
	otherFailure := false
	for index, candidate := range credentials {
		attempt := index + 1
		credential, err := s.pool.usableCredential(ctx, candidate, false)
		if err != nil {
			LogWarnEvent("credential_unavailable", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential_count", len(credentials),
				"credential", candidate.fileName, "error", err)
			lastErr = wrapOAuthAuthorizeError("Kiro credential unavailable", err)
			otherFailure = true
			continue
		}
		LogDebugEvent("upstream_attempt", "component", "kiro", "request_id", requestLogID(ctx),
			"round", round, "attempt", attempt, "credential_count", len(credentials),
			"credential", credential.fileName, "model", converted.model)
		response, err := s.doUpstreamRequest(ctx, converted, credential)
		if err != nil {
			LogWarnEvent("upstream_attempt_failed", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential_count", len(credentials),
				"credential", credential.fileName, "model", converted.model, "error", err)
			lastErr = err
			otherFailure = true
			continue
		}
		if response.StatusCode == http.StatusUnauthorized {
			// Drain the rejected response before closing it: if the refresh then
			// fails we still have the upstream diagnostics to report.
			unauthorizedHeaders := extractProxyResponseHeaders(response.Header)
			unauthorized := drainKiroErrorBody(ctx, response)
			LogWarnEvent("credential_refresh", "component", "kiro", "request_id", requestLogID(ctx),
				"round", round, "attempt", attempt, "credential", credential.fileName,
				"status", http.StatusUnauthorized, "action", "force_refresh")
			refreshed, refreshErr := s.pool.usableCredential(ctx, credential, true)
			if refreshErr != nil {
				lastErr = fmt.Errorf("%w (credential refresh failed: %v)",
					&kiroUpstreamError{status: http.StatusUnauthorized, body: unauthorized, headers: unauthorizedHeaders}, refreshErr)
				otherFailure = true
				continue
			}
			response, err = s.doUpstreamRequest(ctx, converted, refreshed)
			if err != nil {
				lastErr = err
				otherFailure = true
				continue
			}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		upstreamErr := &kiroUpstreamError{
			status:     response.StatusCode,
			retryAfter: response.Header.Get("Retry-After"),
			headers:    extractProxyResponseHeaders(response.Header),
			body:       drainKiroErrorBody(ctx, response),
		}
		if response.StatusCode == http.StatusBadRequest {
			return nil, upstreamErr
		}
		if isKiroRetryableError(upstreamErr) {
			retryableErr = upstreamErr
		} else {
			otherFailure = true
		}
		lastErr = upstreamErr
	}
	// A transient rate limit/5xx is the retryable outcome, so report it when it
	// is the only thing that went wrong. If another credential failed for a
	// reason waiting cannot fix, that error stops the backoff from burning its
	// full budget.
	if retryableErr != nil && !otherFailure {
		return nil, retryableErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all Kiro credentials failed")
	}
	return nil, lastErr
}

func (s *kiroService) doUpstreamRequest(ctx context.Context, converted *kiroConvertedRequest, credential *kiroCredential) (*http.Response, error) {
	body := make(map[string]any, len(converted.body)+1)
	for key, value := range converted.body {
		body[key] = value
	}
	if profileARN := credential.streamingProfileARN(); profileARN != "" {
		body["profileArn"] = profileARN
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := s.upstreamURL(credential)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	machineID := credential.effectiveMachineID()
	osName := runtime.GOOS
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connection", "close")
	request.Header.Set("x-amzn-codewhisperer-optout", "true")
	request.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	request.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-"+kiroIDEVersion+"-"+machineID)
	request.Header.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/"+osName+" lang/js md/nodejs#22.22.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-"+kiroIDEVersion+"-"+machineID)
	request.Header.Set("amz-sdk-invocation-id", uuidString())
	request.Header.Set("amz-sdk-request", "attempt=1; max=3")
	request.Header.Set("Authorization", "Bearer "+credential.accessToken)
	if strings.EqualFold(credential.authMethod, "external_idp") {
		request.Header.Set("tokentype", "EXTERNAL_IDP")
	}
	LogDebugEvent("upstream_request", "component", "kiro", "request_id", requestLogID(ctx),
		"host", request.URL.Host, "model", converted.model, "credential", credential.fileName,
		"body_bytes", len(raw), "media", converted.inlineMedia, "budget_original_bytes", converted.originalBody,
		"budget_final_bytes", converted.finalBody, "budget_original_tokens", converted.originalTokens,
		"budget_final_tokens", converted.finalTokens, "budget_dropped_media", converted.budgetMedia,
		"budget_dropped_history", converted.budgetHistory, "budget_truncated_texts", converted.budgetTexts,
		"budget_dropped_text_bytes", converted.budgetTextBytes, "budget_dropped_tools", converted.budgetTools)
	DebugHTTPBody(fmt.Sprintf("kiro request request_id=%s path=%s", requestLogID(ctx), request.URL.Path), raw)
	started := time.Now()
	response, err := doCredentialRequest(s.client, request)
	if err != nil {
		return nil, fmt.Errorf("call Kiro upstream: %w", err)
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "kiro", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "model", converted.model, "credential", credential.fileName,
		"retry_after", response.Header.Get("Retry-After"), "duration", logDuration(started))
	return response, nil
}

// proxyErrorType maps an HTTP status onto the error type the local
// Responses adapter uses for it, so a forwarded status stays self-consistent for
// clients that branch on error.type rather than on the status code.
func proxyErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestTimeout:
		return "request_timeout_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

func uuidString() string {
	return uuid.NewString()
}
