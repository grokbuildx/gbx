package oauthproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grokbuildx/gbx/internal/codexidentity"
	"github.com/grokbuildx/gbx/internal/fsutil"
	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesDefaultOAuthBaseURL = "https://chatgpt.com/backend-api/codex"
	codexResponsesOAuthTokenURL       = "https://auth.openai.com/oauth/token"
	codexResponsesOAuthClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexResponsesMaxBodyBytes        = int64(128 << 20)
	codexResponsesMaxErrorBytes       = int64(1 << 20)
)

var (
	codexOAuthBaseURL  = codexResponsesDefaultOAuthBaseURL
	codexOAuthTokenURL = codexResponsesOAuthTokenURL
)

type codexResponsesAuthorization struct {
	token      string
	accountID  string
	credential string
	// userID and email are xAI session identity fields. Codex leaves them empty;
	// keeping them on the shared authorization value lets the Grok data plane
	// attach the same account headers as the official Grok Build client without
	// re-reading the credential on every inference request.
	userID string
	email  string
}

type codexResponsesAuthorizer interface {
	authorize(context.Context, bool) (codexResponsesAuthorization, error)
	listAuths() []*AuthInfo
	isOAuth() bool
}

type codexStaticAuthorizer struct{ token string }

func (a *codexStaticAuthorizer) authorize(context.Context, bool) (codexResponsesAuthorization, error) {
	return codexResponsesAuthorization{token: a.token, credential: "api-key"}, nil
}
func (*codexStaticAuthorizer) listAuths() []*AuthInfo { return nil }
func (*codexStaticAuthorizer) isOAuth() bool          { return false }

type codexOAuthAuthorizer struct {
	path   string
	client *http.Client
}

type codexOAuthCredential struct {
	metadata     map[string]any
	accessToken  string
	refreshToken string
	accountID    string
	email        string
	expiresAt    time.Time
	disabled     bool
}

type codexResponsesService struct {
	apiKey            string
	endpoint          string
	models            []string
	modelDetails      map[string]protocol.ModelInfo
	modelRoute        map[string]string
	modelEndpoint     map[string]string
	modelsDevProvider string
	authorizer        codexResponsesAuthorizer
	client            *http.Client
	usage             *UsageTracker
	installationID    string
	windowID          string
	// xai switches the Responses data plane from Codex identity to xAI/Grok
	// identity: xAI Grok headers replace the Codex turn headers, and the
	// Codex-specific client_metadata block is omitted. Both backends share the
	// same OpenAI Responses wire protocol.
	xai bool
	// flattenTools re-renders requests that use tool kinds only the OpenAI
	// backend implements (custom tools, namespaces, client tool search) as
	// plain function tools. Grok needs it when Codex is the client.
	flattenTools bool
}

type codexResponsesUpstreamError struct {
	status     int
	body       string
	retryAfter string
	requestID  string
	headers    http.Header
}

// upstreamStatus implements the retry.go fast-retry probe against the raw
// upstream status, before any client-facing mapping.
func (e *codexResponsesUpstreamError) upstreamStatus() int          { return e.status }
func (e *codexResponsesUpstreamError) upstreamRetryAfter() string   { return e.retryAfter }
func (e *codexResponsesUpstreamError) upstreamErrorBody() string    { return e.body }
func (e *codexResponsesUpstreamError) upstreamHeaders() http.Header { return e.headers }

func (e *codexResponsesUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("Codex Responses upstream returned HTTP %d: %s", e.status, message)
}

func startCodexResponsesAPI(parent context.Context, endpoint, upstreamAPIKey, modelSpec string) (*Runtime, error) {
	return startCodexResponsesAPIWithProvider(parent, endpoint, upstreamAPIKey, modelSpec, "")
}

func startCodexResponsesAPIWithProvider(parent context.Context, endpoint, upstreamAPIKey, modelSpec, providerID string) (*Runtime, error) {
	return startCodexResponsesRuntimeWithService(parent, endpoint, modelSpec, &codexStaticAuthorizer{token: strings.TrimSpace(upstreamAPIKey)},
		func(apiKey, endpoint string, routes []runtimeModelRoute, auth codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
			service := newCodexResponsesService(apiKey, endpoint, routes, auth, usage)
			service.modelsDevProvider = strings.TrimSpace(providerID)
			if service.isXaiBackend() {
				service.flattenTools = true
			}
			return service
		})
}

func startCodexOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &codexOAuthAuthorizer{
		path:   path,
		client: newCredentialHTTPClient(30 * time.Second),
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("Codex credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Codex credential %s has no access or refresh token", filepath.Base(path))
	}
	discoveryCtx, cancelDiscovery := context.WithTimeout(parent, 20*time.Second)
	modelInfos, discoverErr := discoverCodexModelInfos(discoveryCtx, codexOAuthBaseURL, authorizer)
	models := codexModelIDs(modelInfos)
	cancelDiscovery()
	catalogFallback := discoverErr != nil
	if catalogFallback {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		LogWarnf("GPT model discovery failed; keeping configured models error=%v", discoverErr)
		if strings.TrimSpace(modelSpec) == "" {
			modelSpec = "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna"
		}
		models = runtimeModelAliases(modelSpec)
	}
	// Preserve configured routes, but publish the live account catalog so stale
	// mappings do not limit the choices shown by gbx map and gbx set.
	effectiveModelSpec := strings.Join(append([]string{modelSpec}, models...), ",")
	details := make(map[string]protocol.ModelInfo, len(modelInfos))
	for _, info := range modelInfos {
		details[strings.ToLower(info.ID)] = info
	}
	runtime, err := startCodexResponsesRuntimeWithService(parent, codexOAuthBaseURL, effectiveModelSpec, authorizer,
		func(apiKey, endpoint string, routes []runtimeModelRoute, auth codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
			service := newCodexResponsesService(apiKey, endpoint, routes, auth, usage)
			service.models = append([]string(nil), models...)
			service.modelDetails = details
			service.modelsDevProvider = "openai"
			return service
		})
	if err != nil {
		return nil, err
	}
	runtime.listAuths = authorizer.listAuths
	runtime.modelDetails = details
	runtime.catalogFallback = catalogFallback
	LogInfof("runtime start oauth provider=gpt backend=codex protocol=codex_responses port=%s credential_file=%s model_count=%d",
		strings.TrimPrefix(strings.TrimSuffix(runtime.endpoint, "/v1"), "http://"), filepath.Base(path), len(runtime.models))
	return runtime, nil
}

func startCodexResponsesRuntime(parent context.Context, endpoint, modelSpec string, authorizer codexResponsesAuthorizer) (*Runtime, error) {
	return startCodexResponsesRuntimeWithService(parent, endpoint, modelSpec, authorizer, newCodexResponsesService)
}

// startCodexResponsesRuntimeWithService starts a Responses data plane using the
// supplied service factory. The Codex and xAI/Grok backends share the loopback
// server lifecycle but build a different service identity (see newResponsesService).
func startCodexResponsesRuntimeWithService(parent context.Context, endpoint, modelSpec string, authorizer codexResponsesAuthorizer, makeService func(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	if endpoint == "" || authorizer == nil {
		return nil, fmt.Errorf("Responses runtime requires endpoint and authorization")
	}
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Responses runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	usage := NewUsageTracker()
	service := makeService(apiKey, endpoint, routes, authorizer, usage)
	runtime, err := startLoopbackServer(parent, "Responses runtime", service.handlerForContext(parent), apiKey, usage)
	if err != nil {
		return nil, err
	}
	runtime.models = append([]string(nil), service.models...)
	runtime.cleanup = append(runtime.cleanup, closeIdleClients(service.client, authorizerHTTPClient(authorizer)))
	return runtime, nil
}

func newCodexResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
	return newResponsesService(apiKey, endpoint, routes, authorizer, usage, false)
}

// newXaiResponsesService builds the same Responses data plane with xAI/Grok
// identity. Grok uses the OpenAI Responses wire protocol with Grok-specific
// headers and no Codex client metadata.
func newXaiResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker) *codexResponsesService {
	return newResponsesService(apiKey, endpoint, routes, authorizer, usage, true)
}

func newResponsesService(apiKey, endpoint string, routes []runtimeModelRoute, authorizer codexResponsesAuthorizer, usage *UsageTracker, xai bool) *codexResponsesService {
	models := make([]string, 0, len(routes))
	modelRoute := make(map[string]string, len(routes))
	seenModels := make(map[string]bool, len(routes))
	for _, route := range routes {
		modelRoute[strings.ToLower(route.Alias)] = route.Name
		if !seenModels[strings.ToLower(route.Alias)] {
			models = append(models, route.Alias)
			seenModels[strings.ToLower(route.Alias)] = true
		}
	}
	return &codexResponsesService{
		apiKey: apiKey, endpoint: endpoint, models: models, modelRoute: modelRoute,
		authorizer: authorizer, usage: usage, installationID: uuidString(), windowID: uuidString(),
		xai: xai, flattenTools: xai,
		client: newCredentialHTTPClient(90 * time.Second),
	}
}

func (s *codexResponsesService) setModelEndpoints(routes []runtimeModelRoute, endpoints map[string]string) {
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

func (s *codexResponsesService) responsesURL(model string) string {
	base := ""
	if len(s.modelEndpoint) > 0 {
		for _, id := range []string{model, stripContextModelSuffix(model)} {
			if candidate := s.modelEndpoint[strings.ToLower(strings.TrimSpace(id))]; candidate != "" {
				base = candidate
				break
			}
		}
	}
	if base == "" {
		base = s.endpoint
	}
	return protocol.NormalizeOpenAIResponsesURL(base)
}

func (s *codexResponsesService) handler() http.Handler {
	return s.handlerForContext(context.Background())
}

func (s *codexResponsesService) handlerForContext(ctx context.Context) http.Handler {
	if s.isXaiBackend() {
		return loopbackFrontMuxForContext(ctx, s.apiKey, s.handleModels, s.handleResponses)
	}
	return loopbackFrontMuxWithCompact(ctx, s.apiKey, s.handleModels, s.handleResponses, s.handleResponses)
}

func (s *codexResponsesService) authorized(request *http.Request) bool {
	return loopbackAuthorized(request, s.apiKey)
}

func (s *codexResponsesService) handleModels(writer http.ResponseWriter, request *http.Request) {
	serveModelListWithProvider(writer, request, s.apiKey, s.models, s.modelDetails, s.modelRoute, s.modelsDevProvider)
}

// handleResponses serves one Responses request. The upstream speaks Responses
// too, so the request is forwarded as the client sent it; only a request whose
// tool dialect this upstream does not implement is re-rendered first.
func (s *codexResponsesService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if !inferenceRequestAllowed(writer, request, s.apiKey) {
		return
	}
	body, err := readInboundBody(writer, request, codexResponsesMaxBodyBytes)
	if err != nil {
		writeInboundBodyError(writer, err)
		return
	}
	var payload map[string]any
	if decodeProtocolJSON(body, &payload) != nil {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request JSON")
		return
	}
	compact := strings.HasSuffix(request.URL.Path, "/compact")
	if s.flattenTools && !compact && responsesNeedFlattening(payload) {
		s.serveConverted(writer, request, body)
		return
	}
	s.servePassthrough(writer, request, payload, compact)
}

// serveConverted re-renders the request with plain function tools, then
// assembles the upstream stream back into the client's own tool dialect.
func (s *codexResponsesService) serveConverted(writer http.ResponseWriter, request *http.Request, raw []byte) {
	requestCtx, requestID := withRequestLogID(request.Context())
	requestCtx = withClientSession(requestCtx, request)
	started := time.Now()
	clientModel := codexResponsesModel(raw)
	route, ok := s.resolveModelRoute(clientModel)
	if !ok && clientModel != "" {
		writeProxyErrorDetail(writer, http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "model is not in the provider catalog")
		return
	}
	scope := s.reasoningScope(firstNonEmptyString(route, clientModel))
	converted, err := convertResponsesForUpstreamWithScope(raw, s.requestSessionID(request), scope)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "codex_responses", "request_id", requestID,
			"status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !ok {
		route, ok = s.resolveModelRoute(converted.clientModel)
		if !ok {
			writeProxyErrorDetail(writer, http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "model is not in the provider catalog")
			return
		}
		scope = s.reasoningScope(route)
		converted.reasoningScope = scope
	}
	converted.model, converted.upstreamModel = route, route
	var body map[string]any
	if decodeProtocolJSON(converted.body, &body) == nil {
		body["model"] = route
		if s.isXaiBackend() {
			converted.thinkingEnabled = s.normalizeXaiReasoning(route, body)
			normalizeXaiImageDetail(body)
		}
		converted.body, _ = json.Marshal(body)
	}
	LogDebugEvent("request_converted", "component", "codex_responses", "request_id", requestID,
		"client_model", converted.clientModel, "upstream_model", converted.model,
		"stream", converted.stream, "body_bytes", len(converted.body))
	response, err := s.callWithResponsesFallback(requestCtx, requestID, converted.model, &converted.body, converted.sessionID, true)
	if err != nil {
		s.writeUpstreamError(writer, requestID, converted.model, started, err)
		return
	}
	defer response.Body.Close()
	copyProxyResponseHeaders(writer.Header(), response.Header)
	if converted.stream {
		assembler := newResponseAssembler(&converted.adapterRequest, writer)
		err = processCodexResponsesStream(response.Body, assembler)
		if err != nil {
			if errors.Is(err, context.Canceled) || requestCtx.Err() != nil {
				LogDebugEvent("stream_cancelled", "component", "codex_responses", "request_id", requestID,
					"model", converted.model, "duration", logDuration(started))
				if assembler.started {
					s.recordUsage(converted.clientModel, assembler)
				}
				return
			}
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "duration", logDuration(started), "error", err)
			writeStreamFailure(writer, assembler, true, "openai_responses", err)
			if assembler.started {
				s.recordUsage(converted.clientModel, assembler)
			}
			return
		}
		s.recordUsage(converted.clientModel, assembler)
	} else {
		assembler := newResponseAssembler(&converted.adapterRequest, nil)
		if err = processCodexResponsesStream(response.Body, assembler); err != nil {
			LogErrorEvent("stream_conversion_failed", "component", "codex_responses", "request_id", requestID,
				"model", converted.model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
			writeStreamFailure(writer, assembler, false, "openai_responses", err)
			return
		}
		s.recordUsage(converted.clientModel, assembler)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(assembler.response())
	}
	LogDebugEvent("request_complete", "component", "codex_responses", "request_id", requestID,
		"model", converted.model, "status", http.StatusOK, "stream", converted.stream, "duration", logDuration(started))
}

func (s *codexResponsesService) resolveModelRoute(model string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	if trimmed == "" {
		return "", false
	}
	if route, ok := s.modelRoute[trimmed]; ok && route != "" {
		return route, true
	}
	if bare := strings.ToLower(stripContextModelSuffix(trimmed)); bare != "" && bare != trimmed {
		if route, ok := s.modelRoute[bare]; ok && route != "" {
			return route, true
		}
	}
	return "", false
}

func (s *codexResponsesService) isXaiBackend() bool {
	return s.xai || strings.EqualFold(strings.TrimSpace(s.modelsDevProvider), "xai") || IsOfficialXaiEndpoint(s.endpoint)
}

func (s *codexResponsesService) reasoningScope(model string) string {
	switch {
	case s.isXaiBackend():
		return "xai:" + strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	case s.authorizer != nil && s.authorizer.isOAuth():
		return "codex-oauth:" + strings.ToLower(strings.TrimRight(strings.TrimSpace(s.endpoint), "/"))
	default:
		return "responses:" + strings.ToLower(strings.TrimRight(strings.TrimSpace(s.endpoint), "/")) + ":" + strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	}
}

func (s *codexResponsesService) writeUpstreamError(writer http.ResponseWriter, requestID, model string, started time.Time, err error) {
	var upstreamErr *codexResponsesUpstreamError
	if errors.As(err, &upstreamErr) {
		copyProxyResponseHeaders(writer.Header(), upstreamErr.headers)
		if upstreamErr.retryAfter != "" {
			writer.Header().Set("Retry-After", upstreamErr.retryAfter)
		}
		if upstreamErr.requestID != "" {
			writer.Header().Set("X-Request-Id", upstreamErr.requestID)
		}
		LogUpstreamEvent(upstreamErr.status, "request_failed", "component", "codex_responses", "request_id", requestID,
			"model", model, "status", upstreamErr.status, "returned_status", upstreamErr.status,
			"retry_after", upstreamErr.retryAfter, "duration", logDuration(started))
		norm := normalizeUpstreamError("openai_responses", upstreamErr.status, upstreamErr.body, upstreamErr.Error())
		writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
		return
	}
	var authErr *oauthAuthError
	if errors.As(err, &authErr) {
		status := authErr.status
		if status == 0 {
			status = http.StatusUnauthorized
		}
		LogUpstreamEvent(status, "request_failed", "component", "codex_responses", "request_id", requestID,
			"model", model, "status", status, "returned_status", status, "duration", logDuration(started))
		writeProxyErrorWithCode(writer, status, proxyErrorType(status), "invalid_api_key", authErr.Error())
		return
	}
	LogErrorEvent("request_failed", "component", "codex_responses", "request_id", requestID,
		"model", model, "returned_status", http.StatusBadGateway, "duration", logDuration(started), "error", err)
	writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", err.Error())
}

// requestSessionID is the client's conversation id, or a fresh one. Codex sends
// Session_id; Grok Build sends x-grok-conv-id.
func (s *codexResponsesService) requestSessionID(request *http.Request) string {
	if id := extractClientSessionID(request); id != "" {
		return id
	}
	return uuidString()
}

func (s *codexResponsesService) recordUsage(clientModel string, assembler *responseAssembler) {
	if s.usage == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

// call runs one upstream round-trip inside the shared fast-retry loop: a 429
// or 5xx outcome is retried twice more (500ms/1s) before being returned
// as-is, letting Claude Code do its own full backoff over the relayed
// status/body/Retry-After. The 401 refresh below is part of one attempt.
func (s *codexResponsesService) call(ctx context.Context, body []byte, sessionID string, dumpPayload bool) (*http.Response, error) {
	// The Codex subscription backend does not accept max_output_tokens.
	// Generic Responses gateways (including xAI) keep the caller's limit.
	if _, subscription := s.authorizer.(*codexOAuthAuthorizer); subscription {
		var err error
		body, err = sjson.DeleteBytes(body, "max_output_tokens")
		if err != nil {
			return nil, err
		}
	}
	if !s.isXaiBackend() && ctx.Value(codexCompactRequest{}) != true {
		var err error
		body, err = s.addClientMetadata(body, sessionID)
		if err != nil {
			return nil, err
		}
	}
	return retryUpstream(ctx, "codex_responses", func() (*http.Response, error) {
		auth, err := s.authorizer.authorize(ctx, false)
		if err != nil {
			return nil, wrapOAuthAuthorizeError("authorize Codex Responses upstream", err)
		}
		response, err := s.callOnce(ctx, body, sessionID, auth, dumpPayload)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && s.authorizer.isOAuth() {
			rejectedErr := codexDrainError(ctx, response)
			LogWarnEvent("credential_refresh", "component", "codex_responses", "request_id", requestLogID(ctx),
				"credential", auth.credential, "status", http.StatusUnauthorized, "action", "refresh_and_retry_once")
			// Another concurrent request may already have refreshed this credential.
			// Reuse its token instead of rotating the same refresh token twice.
			current, currentErr := s.authorizer.authorize(ctx, false)
			if currentErr == nil && current.token != "" && current.token != auth.token {
				auth = current
			} else {
				auth, err = s.authorizer.authorize(withRejectedOAuthToken(ctx, auth.token), true)
			}
			if err != nil {
				return nil, &codexResponsesUpstreamError{
					status: http.StatusUnauthorized,
					body:   fmt.Sprintf("%v (credential refresh failed: %v)", rejectedErr, err),
				}
			}
			response, err = s.callOnce(ctx, body, sessionID, auth, dumpPayload)
			if err != nil {
				return nil, err
			}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, codexDrainError(ctx, response)
		}
		return response, nil
	})
}

func (s *codexResponsesService) callOnce(ctx context.Context, body []byte, sessionID string, auth codexResponsesAuthorization, dumpPayload bool) (*http.Response, error) {
	target := s.responsesURL(codexResponsesModel(body))
	if ctx.Value(codexCompactRequest{}) == true {
		target = strings.TrimRight(target, "/") + "/compact"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if s.xai {
		applyXaiGrokHeaders(request.Header, sessionID, uuidString(), s.installationID, codexResponsesModel(body), auth)
	} else {
		codexidentity.ApplyTurnHeaders(request.Header, sessionID, sessionID, s.windowID)
	}
	request.Header.Set("Authorization", "Bearer "+auth.token)
	request.Header.Set("Content-Type", "application/json")
	if ctx.Value(codexCompactRequest{}) == true {
		request.Header.Set("Accept", "application/json")
	} else {
		request.Header.Set("Accept", "text/event-stream")
	}
	if auth.accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", auth.accountID)
	}
	applyOpenCodeSession(ctx, request)
	LogDebugEvent("upstream_request", "component", "codex_responses", "request_id", requestLogID(ctx),
		"method", http.MethodPost, "endpoint", SafeLogEndpoint(target), "credential", auth.credential,
		"body_bytes", len(body), "payload_logged", dumpPayload,
		"codex_version", codexidentity.ClientVersion, "originator", codexidentity.Originator,
		"user_agent", codexidentity.UserAgent(), "session_id", sessionID, "thread_id", sessionID,
		"window_id", s.windowID)
	if dumpPayload {
		DebugHTTPBody(fmt.Sprintf("codex responses request request_id=%s", requestLogID(ctx)), body)
	}
	started := time.Now()
	trace, traceSnapshot := newCodexHTTPTrace(started)
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := doCredentialRequest(s.client, request)
	if err != nil {
		timings := traceSnapshot()
		LogWarnEvent("upstream_transport_failed", "component", "codex_responses", "request_id", requestLogID(ctx),
			"connection_reused", timings.connectionReused, "connection_wait", timings.connectionWait,
			"dns", timings.dns, "connect", timings.connect, "tls", timings.tls,
			"request_write", timings.requestWrite, "ttfb", timings.ttfb,
			"write_to_first_byte", timings.writeToFirstByte, "wrote_request_error", timings.wroteRequestErr,
			"duration", logDuration(started), "error", err)
		return nil, fmt.Errorf("call Codex Responses upstream: %w", err)
	}
	timings := traceSnapshot()
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "codex_responses", "request_id", requestLogID(ctx),
		"status", response.StatusCode, "credential", auth.credential, "retry_after", response.Header.Get("Retry-After"),
		"upstream_request_id", firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		"content_type", response.Header.Get("Content-Type"), "content_length", response.ContentLength,
		"connection_reused", timings.connectionReused, "connection_wait", timings.connectionWait,
		"dns", timings.dns, "connect", timings.connect, "tls", timings.tls,
		"request_write", timings.requestWrite, "ttfb", timings.ttfb,
		"write_to_first_byte", timings.writeToFirstByte, "wrote_request_error", timings.wroteRequestErr,
		"duration", logDuration(started))
	return response, nil
}

func codexResponsesModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if decodeProtocolJSON(body, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

func (s *codexResponsesService) normalizeXaiReasoning(model string, payload map[string]any) bool {
	if !s.isXaiBackend() || payload == nil {
		return true
	}
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		return false
	}
	effort := strings.ToLower(strings.TrimSpace(stringValue(reasoning["effort"])))
	if effort == "" {
		return true
	}
	if effort == "none" {
		// Never clamp an explicit "none" upward to "low".
		delete(payload, "reasoning")
		return false
	}
	if effort == "ultra" {
		effort = "max"
	}
	var levels []protocol.ReasoningLevel
	known := false
	if info, hasInfo := lookupModelDetail(s.modelDetails, model); hasInfo && info.ReasoningKnown {
		levels = info.ReasoningLevels
		known = true
	} else if index, _ := modelsdev.CachedIndex(); index != nil {
		target := strings.TrimSpace(stripContextModelSuffix(model))
		if efforts, found := index.Efforts("xai", target, model); found {
			levels, _ = reasoningLevelsFromEfforts(efforts, "")
			known = true
		}
	}
	if known {
		if len(levels) == 0 {
			delete(payload, "reasoning")
			return false
		}
		if clamped := protocol.ClampReasoningLevel(effort, levels); clamped != "" {
			effort = clamped
		} else {
			effort = strings.ToLower(strings.TrimSpace(levels[0].Effort))
		}
	}
	reasoning["effort"] = effort
	return true
}

func (s *codexResponsesService) callWithResponsesFallback(ctx context.Context, requestID, model string, body *[]byte, sessionID string, dumpPayload bool) (*http.Response, error) {
	response, err := s.call(ctx, *body, sessionID, dumpPayload)
	if err == nil {
		return response, nil
	}
	triedEffort := false
	triedReasoningInput := false
	for attempt := 0; attempt < 2 && err != nil; attempt++ {
		var upstreamErr *codexResponsesUpstreamError
		if !errors.As(err, &upstreamErr) {
			break
		}
		if !triedEffort && isRejectedResponsesEffortError(upstreamErr.status, upstreamErr.body) && gjson.GetBytes(*body, "reasoning.effort").Exists() {
			triedEffort = true
			updated, delErr := sjson.DeleteBytes(*body, "reasoning.effort")
			if delErr == nil {
				if len(gjson.GetBytes(updated, "reasoning").Map()) == 0 {
					updated, _ = sjson.DeleteBytes(updated, "reasoning")
				}
				LogWarnEvent("responses_reasoning_effort_rejected_fallback", "component", "codex_responses",
					"request_id", requestID, "model", model, "status", upstreamErr.status)
				*body = updated
				response, err = s.call(ctx, *body, sessionID, dumpPayload)
				continue
			}
		}
		if !triedReasoningInput && isRejectedResponsesReasoningItemError(upstreamErr.status, upstreamErr.body) {
			triedReasoningInput = true
			if stripped, changed := stripResponsesInputReasoningItems(*body); changed {
				LogWarnEvent("responses_reasoning_input_rejected_fallback", "component", "codex_responses",
					"request_id", requestID, "model", model, "status", upstreamErr.status)
				*body = stripped
				response, err = s.call(ctx, *body, sessionID, dumpPayload)
				continue
			}
		}
		break
	}
	return response, err
}

func isRejectedResponsesEffortError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "reasoning") || !strings.Contains(lower, "effort") {
		return false
	}
	for _, needle := range []string{"not supported", "unsupported", "invalid", "unrecognized", "unknown", "not allowed", "does not support"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func isRejectedResponsesReasoningItemError(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "encrypted_content") && !strings.Contains(lower, "reasoning") {
		return false
	}
	for _, needle := range []string{"decrypt", "signature", "invalid encrypted", "cannot be decrypted", "not found", "unrecognized reasoning", "corrupt"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func stripResponsesInputReasoningItems(raw []byte) ([]byte, bool) {
	var payload map[string]any
	if decodeProtocolJSON(raw, &payload) != nil || payload == nil {
		return raw, false
	}
	items, ok := payload["input"].([]any)
	if !ok || len(items) == 0 {
		return raw, false
	}
	filtered := make([]any, 0, len(items))
	changed := false
	for _, entry := range items {
		item, isMap := entry.(map[string]any)
		if isMap && stringValue(item["type"]) == "reasoning" {
			changed = true
			continue
		}
		filtered = append(filtered, entry)
	}
	if !changed {
		return raw, false
	}
	payload["input"] = filtered
	encoded, err := json.Marshal(payload)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

// normalizeXaiImageDetail maps OpenAI Responses' "original" image detail value
// to "high" on xAI Responses endpoints, which only accept auto|low|high.
func normalizeXaiImageDetail(payload map[string]any) {
	if payload == nil {
		return
	}
	if input, ok := payload["input"]; ok {
		rewriteXaiImageDetail(input)
	}
}

func rewriteXaiImageDetail(value any) {
	switch v := value.(type) {
	case []any:
		for _, elem := range v {
			rewriteXaiImageDetail(elem)
		}
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringValue(v["type"])))
		if kind == "input_image" || kind == "image_url" || kind == "computer_screenshot" {
			if strings.EqualFold(strings.TrimSpace(stringValue(v["detail"])), "original") {
				v["detail"] = "high"
			}
			if nested, ok := v["image_url"].(map[string]any); ok {
				if strings.EqualFold(strings.TrimSpace(stringValue(nested["detail"])), "original") {
					nested["detail"] = "high"
				}
			}
		}
		for _, elem := range v {
			rewriteXaiImageDetail(elem)
		}
	}
}

func (s *codexResponsesService) addClientMetadata(body []byte, sessionID string) ([]byte, error) {
	var payload map[string]any
	if err := decodeProtocolJSON(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Codex Responses request metadata: %w", err)
	}
	metadata, _ := payload["client_metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any, 4)
	}
	metadata["x-codex-installation-id"] = s.installationID
	if strings.TrimSpace(stringValue(metadata["session_id"])) == "" {
		metadata["session_id"] = sessionID
	}
	if strings.TrimSpace(stringValue(metadata["thread_id"])) == "" {
		metadata["thread_id"] = sessionID
	}
	metadata["x-codex-window-id"] = s.windowID
	payload["client_metadata"] = metadata
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode Codex Responses request metadata: %w", err)
	}
	return encoded, nil
}

func codexDrainError(ctx context.Context, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	_ = response.Body.Close()
	DebugHTTPBody(fmt.Sprintf("codex responses response request_id=%s status=%d", requestLogID(ctx), response.StatusCode), body)
	return &codexResponsesUpstreamError{
		status:     response.StatusCode,
		body:       strings.TrimSpace(string(body)),
		retryAfter: response.Header.Get("Retry-After"),
		requestID:  firstHeader(response.Header, "X-Request-Id", "Request-Id"),
		headers:    extractProxyResponseHeaders(response.Header),
	}
}

func drainAndClose(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	_ = response.Body.Close()
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

type codexCompactRequest struct{}

func (s *codexResponsesService) isCodexOAuth() bool {
	if s.authorizer == nil || !s.authorizer.isOAuth() || s.isXaiBackend() {
		return false
	}
	_, ok := s.authorizer.(*codexOAuthAuthorizer)
	return ok
}

// normalizePassthroughCodexOAuthPayload ensures ChatGPT Codex OAuth requests
// carry top-level instructions (hoisting any leading role:"system" input items)
// and rewrites remaining mid-conversation role:"system" items to "developer".
func normalizePassthroughCodexOAuthPayload(payload map[string]any) {
	if payload == nil {
		return
	}
	instructions := stringValue(payload["instructions"])
	items, ok := payload["input"].([]any)
	if !ok {
		if _, exists := payload["instructions"]; !exists {
			payload["instructions"] = ""
		}
		return
	}
	filtered := make([]any, 0, len(items))
	seenNonSystem := false
	for _, raw := range items {
		item, isMap := raw.(map[string]any)
		if !isMap {
			seenNonSystem = true
			filtered = append(filtered, raw)
			continue
		}
		kind := stringValue(item["type"])
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		if (kind == "" || kind == "message") && role == "system" {
			if !seenNonSystem {
				if text := extractPassthroughMessageText(item["content"]); text != "" {
					if instructions != "" {
						instructions += "\n"
					}
					instructions += text
				}
				continue
			}
			cloned := make(map[string]any, len(item))
			for k, v := range item {
				cloned[k] = v
			}
			cloned["role"] = "developer"
			filtered = append(filtered, cloned)
			continue
		}
		seenNonSystem = true
		filtered = append(filtered, raw)
	}
	payload["instructions"] = instructions
	payload["input"] = filtered
}

func extractPassthroughMessageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, elem := range v {
			m, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			if t := stringValue(m["text"]); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// normalizePassthroughResponsesImages repairs non-standard image parts (such as
// uppercase DATA:, image/jpg, octet-stream, or nested image_url objects) in
// passthrough Responses payloads without forcing full tool flattening.
func normalizePassthroughResponsesImages(payload map[string]any) {
	if payload == nil {
		return
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(item["type"]) {
		case "", "message":
			normalizePassthroughImageContainer(item["content"])
		case "function_call_output", "computer_call_output":
			normalizePassthroughImageContainer(item["output"])
		}
	}
}

func normalizePassthroughImageContainer(raw any) {
	switch v := raw.(type) {
	case map[string]any:
		normalizePassthroughImagePart(v)
	case []any:
		for _, elem := range v {
			if m, ok := elem.(map[string]any); ok {
				normalizePassthroughImagePart(m)
			}
		}
	}
}

func normalizePassthroughImagePart(part map[string]any) {
	if !responsesImagePartNeedsNormalization(part) {
		return
	}
	parsed, err := responsesImagePart(part)
	if err != nil {
		return
	}
	normalized := responsesInputImagePart(parsed)
	clear(part)
	for k, v := range normalized {
		part[k] = v
	}
}

// servePassthrough forwards a Responses request to the Responses upstream. It
// shares authorization, refresh and retry with converted requests, and reads
// token usage off the relayed stream.
func (s *codexResponsesService) servePassthrough(writer http.ResponseWriter, request *http.Request, payload map[string]any, compact bool) {
	started := time.Now()
	model, _ := payload["model"].(string)
	resolved, ok := s.resolveModelRoute(model)
	if !ok {
		writeProxyErrorDetail(writer, http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "model is not in the provider catalog")
		return
	}
	payload["model"] = resolved
	scope := s.reasoningScope(resolved)
	wantsStream, _ := payload["stream"].(bool)
	if s.isCodexOAuth() {
		normalizePassthroughCodexOAuthPayload(payload)
	}
	normalizePassthroughResponsesImages(payload)
	sanitizePassthroughResponsesInputScoped(payload, scope)
	var compactFallbackBody []byte
	if compact {
		fallbackPayload := cloneMapShallow(payload)
		fallbackPayload["model"] = model
		compactFallbackBody, _ = json.Marshal(fallbackPayload)
		sanitizeCompactRequestPayload(payload)
	} else {
		payload["stream"] = true
		payload["store"] = false
		// Strip non-Responses Chat-front parameters before forwarding.
		delete(payload, "stop")
		delete(payload, "seed")
		delete(payload, "presence_penalty")
		delete(payload, "frequency_penalty")
		delete(payload, "logit_bias")
		delete(payload, "logprobs")
		payload["include"] = mergePassthroughInclude(payload["include"], "reasoning.encrypted_content")
		if s.isXaiBackend() {
			s.normalizeXaiReasoning(resolved, payload)
			normalizeXaiImageDetail(payload)
		}
	}
	sessionID := s.requestSessionID(request)
	if !compact && strings.TrimSpace(stringValue(payload["prompt_cache_key"])) == "" {
		payload["prompt_cache_key"] = sessionID
	}
	body, _ := json.Marshal(payload)
	ctx, requestID := withRequestLogID(request.Context())
	ctx = withClientSession(ctx, request)
	if compact {
		ctx = context.WithValue(ctx, codexCompactRequest{}, true)
	}
	response, err := s.callWithResponsesFallback(ctx, requestID, resolved, &body, sessionID, true)
	if err != nil {
		if compact && len(compactFallbackBody) > 0 && shouldFallbackFromUpstreamCompact(err) {
			LogWarnEvent("responses_compact_local_fallback", "component", "codex_responses",
				"request_id", requestID, "model", resolved, "error", err)
			fallbackReq := request.Clone(request.Context())
			fallbackReq.Body = io.NopCloser(bytes.NewReader(compactFallbackBody))
			fallbackReq.ContentLength = int64(len(compactFallbackBody))
			fallbackReq.URL.Path = "/v1/responses/compact"
			localCompactionHandler(s.apiKey, s.handleResponses)(writer, fallbackReq)
			return
		}
		s.writeUpstreamError(writer, requestID, resolved, started, err)
		return
	}
	defer response.Body.Close()
	if ct := response.Header.Get("Content-Type"); ct != "" {
		writer.Header().Set("Content-Type", ct)
	}
	copyProxyResponseHeaders(writer.Header(), response.Header)
	isEventStream := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	if !compact && !wantsStream && isEventStream {
		var completed json.RawMessage
		err := readSSEPayload(response.Body, "openai_responses", func(raw []byte) error {
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(raw, &event) != nil {
				return fmt.Errorf("invalid Responses stream event")
			}
			if event.Type == "response.completed" || event.Type == "response.incomplete" {
				completed = append(json.RawMessage(nil), event.Response...)
			}
			if event.Type == "response.failed" {
				errBody := string(raw)
				if len(event.Response) > 0 {
					errBody = string(event.Response)
				}
				return buildStreamUpstreamError("openai_responses", errBody, "")
			}
			if event.Type == "error" {
				return buildStreamUpstreamError("openai_responses", string(raw), "")
			}
			return nil
		})
		if err != nil {
			var streamErr *streamUpstreamError
			if errors.As(err, &streamErr) {
				writeProxyErrorDetail(writer, streamErr.status, streamErr.errType, streamErr.code, streamErr.param, streamErr.Error())
				return
			}
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", err.Error())
			return
		}
		if len(completed) == 0 {
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "upstream_stream_error", "", "upstream Responses stream did not complete")
			return
		}
		observer := &responsesUsageObserver{reasoningScope: scope}
		observer.event(completed)
		s.recordObservedUsage(model, observer)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(completed)
		return
	}
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(response.StatusCode)
	observer := &responsesUsageObserver{reasoningScope: scope}
	defer func() {
		observer.finish()
		s.recordObservedUsage(model, observer)
	}()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			observer.observe(buffer[:n])
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				return
			}
			_ = http.NewResponseController(writer).Flush()
		}
		if readErr != nil {
			observer.finish()
			if readErr != io.EOF && request.Context().Err() == nil {
				if isEventStream && !observer.terminalSeen {
					errPayload, _ := json.Marshal(map[string]any{
						"type":    "error",
						"code":    "upstream_stream_error",
						"message": truncateErrorMessage(readErr.Error(), maxClientErrorMessageBytes),
						"param":   nil,
					})
					_, _ = fmt.Fprintf(writer, "event: error\ndata: %s\n\n", errPayload)
					_ = http.NewResponseController(writer).Flush()
				}
				return
			}
			if readErr == io.EOF && isEventStream && !compact && !observer.terminalSeen && request.Context().Err() == nil {
				errPayload, _ := json.Marshal(map[string]any{
					"type":    "error",
					"code":    "upstream_stream_error",
					"message": "upstream Responses stream ended before a terminal event",
					"param":   nil,
				})
				_, _ = fmt.Fprintf(writer, "event: error\ndata: %s\n\n", errPayload)
				_ = http.NewResponseController(writer).Flush()
			}
			return
		}
	}
}

func mergePassthroughInclude(raw any, required string) []string {
	var existing []string
	switch v := raw.(type) {
	case []string:
		existing = v
	case []any:
		for _, elem := range v {
			if s, ok := elem.(string); ok {
				existing = append(existing, s)
			}
		}
	}
	return mergeResponsesInclude(existing, required)
}

func (s *codexResponsesService) recordObservedUsage(clientModel string, observer *responsesUsageObserver) {
	if s.usage == nil || !observer.seen {
		return
	}
	fresh := max(0, observer.input-observer.cached-observer.cacheWrite)
	s.usage.Add(clientModel, int64(fresh), int64(observer.output), int64(observer.cached), int64(observer.cacheWrite))
}

// sanitizePassthroughResponsesInput filters input reasoning items before
// forwarding with store=false. Stateless Responses upstreams reject reasoning
// items that lack provider-encrypted content, carry GBX synthetic IDs, or were
// encrypted by a different backend/model scope.
func sanitizePassthroughResponsesInput(payload map[string]any) {
	sanitizePassthroughResponsesInputScoped(payload, "")
}

func sanitizeCompactRequestPayload(payload map[string]any) {
	if payload == nil {
		return
	}
	// OpenAI and ChatGPT Codex /responses/compact accept model, input,
	// instructions, and previous_response_id; extra turn fields are rejected.
	for _, key := range []string{
		"stream", "store", "tools", "tool_choice", "parallel_tool_calls",
		"max_output_tokens", "reasoning", "include", "prompt_cache_key",
		"text", "temperature", "top_p", "truncation", "service_tier",
		"metadata", "client_metadata", "stop", "seed", "presence_penalty",
		"frequency_penalty", "logit_bias", "logprobs",
	} {
		delete(payload, key)
	}
}

func shouldFallbackFromUpstreamCompact(err error) bool {
	var upstreamErr *codexResponsesUpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		norm := normalizeUpstreamError("openai_responses", upstreamErr.status, upstreamErr.body, upstreamErr.Error())
		if norm.Code == "context_length_exceeded" {
			return true
		}
		lower := strings.ToLower(upstreamErr.body)
		for _, needle := range []string{"not found", "unsupported", "unknown url", "invalid url", "not implemented", "does not support", "unknown endpoint", "unrecognized request url"} {
			if strings.Contains(lower, needle) {
				return true
			}
		}
	}
	return false
}

func sanitizePassthroughResponsesInputScoped(payload map[string]any, scope string) {
	items, ok := payload["input"].([]any)
	if !ok || len(items) == 0 {
		return
	}
	filtered := make([]any, 0, len(items))
	for _, raw := range items {
		item, isMap := raw.(map[string]any)
		if !isMap {
			filtered = append(filtered, raw)
			continue
		}
		itemType := stringValue(item["type"])
		if itemType == "compaction" {
			if summary, ok := DecodeCompactionSummary(stringValue(item["encrypted_content"])); ok {
				filtered = append(filtered, map[string]any{
					"type": "message",
					"role": "user",
					"content": []any{map[string]any{
						"type": "input_text",
						"text": "Conversation summary:\n" + summary,
					}},
				})
				continue
			}
			filtered = append(filtered, raw)
			continue
		}
		if itemType != "reasoning" {
			filtered = append(filtered, raw)
			continue
		}
		id := stringValue(item["id"])
		enc := strings.TrimSpace(stringValue(item["encrypted_content"]))
		if strings.HasPrefix(id, gbxReasoningIDPrefix) {
			if strings.HasPrefix(enc, codexReasoningSignaturePrefix) {
				if unwrapped, matched, err := codexReasoningFromSignatureScoped(enc, scope); err == nil && matched {
					var unwrappedMap map[string]any
					switch typed := unwrapped.(type) {
					case map[string]any:
						unwrappedMap = typed
					case json.RawMessage:
						_ = json.Unmarshal(typed, &unwrappedMap)
					}
					if unwrappedMap != nil && strings.TrimSpace(stringValue(unwrappedMap["encrypted_content"])) != "" {
						filtered = append(filtered, unwrappedMap)
					}
				}
			}
			continue
		}
		if enc == "" || protocol.IsGBXManagedSignature(enc) {
			continue
		}
		if scope != "" && !protocol.ReasoningScopeMatches(enc, scope) {
			continue
		}
		filtered = append(filtered, raw)
	}
	payload["input"] = filtered
}

// responsesUsageObserver reads token usage, terminal event presence, and
// encrypted reasoning payloads off a relayed Responses stream (or JSON body)
// without altering it.
type responsesUsageObserver struct {
	pending                           []byte
	input, output, cached, cacheWrite int
	seen                              bool
	terminalSeen                      bool
	reasoningScope                    string
}

const responsesUsageObserverMaxEvent = 8 << 20

func (o *responsesUsageObserver) observe(data []byte) {
	o.pending = append(o.pending, data...)
	for {
		end, sepLen := nextSSEFrameBoundary(o.pending)
		if end < 0 {
			break
		}
		o.event(o.pending[:end])
		o.pending = o.pending[end+sepLen:]
	}
	if len(o.pending) > responsesUsageObserverMaxEvent {
		o.pending = nil
	}
}

func (o *responsesUsageObserver) finish() {
	if len(o.pending) > 0 {
		o.event(o.pending)
		o.pending = nil
	}
}

func (o *responsesUsageObserver) event(raw []byte) {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimSpace(value)...)
		}
	}
	if len(data) == 0 {
		data = bytes.TrimSpace(raw)
	}
	if o.reasoningScope != "" && bytes.Contains(data, []byte(`"encrypted_content"`)) {
		recordObservedReasoningScope(data, o.reasoningScope)
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.completed", "response.incomplete", "response.failed", "error":
		o.terminalSeen = true
	}
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	usage := gjson.GetBytes(data, "response.usage")
	if !usage.Exists() {
		usage = gjson.GetBytes(data, "usage")
	}
	if !usage.IsObject() {
		return
	}
	o.seen = true
	o.input = int(usage.Get("input_tokens").Int())
	o.output = int(usage.Get("output_tokens").Int())
	o.cached = int(usage.Get("input_tokens_details.cached_tokens").Int())
	o.cacheWrite = int(usage.Get("input_tokens_details.cache_write_tokens").Int())
	if o.cacheWrite == 0 {
		o.cacheWrite = int(usage.Get("cache_write_tokens").Int())
	}
}

func recordObservedReasoningScope(data []byte, scope string) {
	if item := gjson.GetBytes(data, "item"); item.IsObject() && item.Get("type").String() == "reasoning" {
		if enc := strings.TrimSpace(item.Get("encrypted_content").String()); enc != "" {
			protocol.RecordReasoningScope(enc, scope)
		}
	}
	outputs := gjson.GetBytes(data, "response.output")
	if !outputs.IsArray() {
		outputs = gjson.GetBytes(data, "output")
	}
	if outputs.IsArray() {
		for _, item := range outputs.Array() {
			if item.Get("type").String() == "reasoning" {
				if enc := strings.TrimSpace(item.Get("encrypted_content").String()); enc != "" {
					protocol.RecordReasoningScope(enc, scope)
				}
			}
		}
	}
}

func (a *codexOAuthAuthorizer) isOAuth() bool { return true }

func (a *codexOAuthAuthorizer) authorize(ctx context.Context, force bool) (codexResponsesAuthorization, error) {
	release, err := lockOAuthCredential(ctx, a.path)
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	defer release()
	credential, err := a.load()
	if err != nil {
		return codexResponsesAuthorization{}, err
	}
	if credential.disabled {
		return codexResponsesAuthorization{}, fmt.Errorf("Codex credential %s is disabled", filepath.Base(a.path))
	}
	force = force && oauthTokenStillRejected(ctx, credential.accessToken)
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return codexResponsesAuthorization{}, err
		}
	}
	return codexResponsesAuthorization{
		token: credential.accessToken, accountID: credential.accountID, credential: filepath.Base(a.path),
	}, nil
}

func (a *codexOAuthAuthorizer) load() (*codexOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Codex credentials found; run `gbx oauth gpt` first")
		}
		return nil, fmt.Errorf("read Codex credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Codex credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != ProviderCodex && credentialType != ProviderChatGPT {
		return nil, fmt.Errorf("credential %s is type %q, not Codex", filepath.Base(a.path), credentialType)
	}
	expiresAt := parseCodexExpiry(firstMetadataString(metadata, "expired", "expires_at", "expiry"))
	disabled, _ := metadata["disabled"].(bool)
	return &codexOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), accountID: firstMetadataString(metadata, "account_id"),
		email: firstMetadataString(metadata, "email"), expiresAt: expiresAt, disabled: disabled,
	}, nil
}

func (a *codexOAuthAuthorizer) refresh(ctx context.Context, credential *codexOAuthCredential) (*codexOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Codex credential %s has no refresh token", filepath.Base(a.path))
	}
	form := url.Values{
		"client_id": {codexResponsesOAuthClientID}, "grant_type": {"refresh_token"},
		"refresh_token": {credential.refreshToken}, "scope": {"openid profile email"},
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := doCredentialRequest(a.client, request)
	if err != nil {
		return nil, fmt.Errorf("refresh Codex token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, codexResponsesMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Codex token refresh: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh Codex token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode Codex token refresh: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("refresh Codex token: response has no access token")
	}
	credential.accessToken = token.AccessToken
	if token.RefreshToken != "" {
		credential.refreshToken = token.RefreshToken
	}
	if token.ExpiresIn > 0 {
		credential.expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	} else {
		credential.expiresAt = time.Time{}
	}
	if accountID, email := codexJWTIdentity(token.IDToken); accountID != "" || email != "" {
		if accountID != "" {
			credential.accountID = accountID
		}
		if email != "" {
			credential.email = email
		}
	}
	credential.metadata["type"] = ProviderCodex
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	if token.IDToken != "" {
		credential.metadata["id_token"] = token.IDToken
	}
	if credential.accountID != "" {
		credential.metadata["account_id"] = credential.accountID
	}
	if credential.email != "" {
		credential.metadata["email"] = credential.email
	}
	delete(credential.metadata, "expires_at")
	delete(credential.metadata, "expiry")
	if !credential.expiresAt.IsZero() {
		credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	} else {
		delete(credential.metadata, "expired")
	}
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=codex_responses credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

func (a *codexOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: ProviderCodex, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: safeAuthMetadata(credential.metadata),
	}}
}

func writeCodexCredentialAtomic(path string, metadata map[string]any) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex credential update: %w", err)
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func parseCodexExpiry(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func codexJWTIdentity(token string) (accountID, email string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Auth.AccountID, claims.Email
}
