package oauthproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/grokbuildx/gbx/internal/protocol"
)

const mixedMaxBodyBytes = int64(128 << 20)

// mixedRouteSet buckets a model spec's routes by upstream protocol using
// an explicit per-model table from models.dev metadata.
type mixedRouteSet struct {
	chat      []runtimeModelRoute
	responses []runtimeModelRoute
	messages  []runtimeModelRoute
	models    []string
}

// mixedProtocolForModel resolves a route's upstream protocol from the protocol
// table, keyed by lowercase model ID. The alias is consulted first (it is what
// the client sends), then the technical name; an unknown model falls back to
// chat so a single hand-edited slot never blocks the whole runtime.
func mixedProtocolForModel(protocols map[string]string, name, alias string) string {
	if proto, ok := protocols[strings.ToLower(strings.TrimSpace(alias))]; ok && proto != "" {
		return proto
	}
	if proto, ok := protocols[strings.ToLower(strings.TrimSpace(name))]; ok && proto != "" {
		return proto
	}
	return "openai"
}

// buildMixedRoutes maps a model spec into per-protocol route buckets using an
// explicit protocol table. It reports an error only when no usable route is
// produced.
func buildMixedRoutes(modelSpec string, protocols map[string]string) (mixedRouteSet, error) {
	routes := runtimeModelRoutes(modelSpec)
	if len(routes) == 0 {
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime requires at least one model")
	}
	result := mixedRouteSet{}
	seen := make(map[string]bool)
	modelSeen := make(map[string]bool)
	for _, route := range routes {
		proto := mixedProtocolForModel(protocols, route.Name, route.Alias)
		key := strings.ToLower(proto + "\x00" + route.Name + "\x00" + route.Alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		switch proto {
		case "anthropic":
			result.messages = append(result.messages, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		case "openai_responses":
			result.responses = append(result.responses, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		default:
			result.chat = append(result.chat, runtimeModelRoute{Name: route.Name, Alias: route.Alias})
		}
		alias := strings.TrimSpace(route.Alias)
		aliasKey := strings.ToLower(alias)
		if aliasKey != "" && !modelSeen[aliasKey] {
			modelSeen[aliasKey] = true
			result.models = append(result.models, alias)
		}
	}
	if len(result.chat)+len(result.responses)+len(result.messages) == 0 {
		return mixedRouteSet{}, fmt.Errorf("mixed-protocol runtime produced no usable model routes")
	}
	return result, nil
}

// StartMixedProtocolAPIKeyRuntime starts a single Responses entrypoint that
// routes each request to the correct upstream protocol for the requested
// model: OpenAI Responses is forwarded, OpenAI Chat goes through GBX's Chat
// Completions adapter, and Anthropic Messages through its Messages adapter.
// endpoints overrides the base URL per lower-case model ID.
func StartMixedProtocolAPIKeyRuntime(parent context.Context, endpoint, upstreamAPIKey, modelSpec string, protocols, endpoints map[string]string) (*Runtime, error) {
	return startMixedProtocolAPIKeyRuntimeWithProvider(parent, endpoint, upstreamAPIKey, modelSpec, protocols, endpoints, inferModelsDevProviderFromHint("", endpoint))
}

func startMixedProtocolAPIKeyRuntimeWithProvider(parent context.Context, endpoint, upstreamAPIKey, modelSpec string, protocols, endpoints map[string]string, providerID string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	endpoint = normalizeOpenAIBaseURL(endpoint)
	upstreamAPIKey = strings.TrimSpace(upstreamAPIKey)
	if endpoint == "" || upstreamAPIKey == "" {
		return nil, fmt.Errorf("mixed-protocol runtime requires endpoint and API key")
	}
	routes, err := buildMixedRoutes(modelSpec, protocols)
	if err != nil {
		return nil, err
	}
	proxyRuntime, err := startMixedProtocolRouterWithProvider(parent, endpoint, upstreamAPIKey, routes, endpoints, providerID)
	if err != nil {
		return nil, err
	}
	LogInfof("runtime start mixed_protocol endpoint=%q local_endpoint=%q models_chat=%d models_responses=%d models_messages=%d",
		SafeLogEndpoint(endpoint), SafeLogEndpoint(proxyRuntime.endpoint), len(routes.chat), len(routes.responses), len(routes.messages))
	return proxyRuntime, nil
}

// mixedProtocolRouter exposes the Responses front and dispatches each request
// by model to the service for that model's upstream protocol.
type mixedProtocolRouter struct {
	apiKey            string
	models            []string
	modelsDevProvider string
	responses         map[string]bool
	chat              map[string]bool
	messages          map[string]bool
	codex             *codexResponsesService
	chatSvc           *chatCompletionsService
	messagesSvc       *anthropicMessagesService
}

func startMixedProtocolRouter(parent context.Context, endpoint, upstreamAPIKey string, routes mixedRouteSet, endpoints map[string]string) (*Runtime, error) {
	return startMixedProtocolRouterWithProvider(parent, endpoint, upstreamAPIKey, routes, endpoints, "")
}

func startMixedProtocolRouterWithProvider(parent context.Context, endpoint, upstreamAPIKey string, routes mixedRouteSet, endpoints map[string]string, providerID string) (*Runtime, error) {
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	responseRoutes := make([]runtimeModelRoute, 0, len(routes.responses))
	responseModels := make(map[string]bool, len(routes.responses))
	for _, route := range routes.responses {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		responseRoutes = append(responseRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		responseModels[strings.ToLower(alias)] = true
	}
	chatRoutes := make([]runtimeModelRoute, 0, len(routes.chat))
	chatModels := make(map[string]bool, len(routes.chat))
	for _, route := range routes.chat {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		chatRoutes = append(chatRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		chatModels[strings.ToLower(alias)] = true
	}
	messageRoutes := make([]runtimeModelRoute, 0, len(routes.messages))
	messageModels := make(map[string]bool, len(routes.messages))
	for _, route := range routes.messages {
		alias := route.Alias
		if strings.TrimSpace(alias) == "" {
			alias = route.Name
		}
		messageRoutes = append(messageRoutes, runtimeModelRoute{Name: route.Name, Alias: alias})
		messageModels[strings.ToLower(alias)] = true
	}
	usage := NewUsageTracker()
	router := &mixedProtocolRouter{
		apiKey: apiKey, models: append([]string(nil), routes.models...), modelsDevProvider: strings.TrimSpace(providerID),
		responses: responseModels, chat: chatModels, messages: messageModels,
	}
	if len(responseRoutes) > 0 {
		router.codex = newCodexResponsesService(apiKey, endpoint, responseRoutes, &codexStaticAuthorizer{token: upstreamAPIKey}, usage)
		router.codex.modelsDevProvider = router.modelsDevProvider
		router.codex.setModelEndpoints(responseRoutes, endpoints)
	}
	if len(chatRoutes) > 0 {
		router.chatSvc = newChatCompletionsService(apiKey, endpoint, chatRoutes, upstreamAPIKey, usage)
		router.chatSvc.modelsDevProvider = router.modelsDevProvider
		router.chatSvc.setModelEndpoints(chatRoutes, endpoints)
	}
	if len(messageRoutes) > 0 {
		router.messagesSvc = newAnthropicMessagesService(apiKey, endpoint, messageRoutes, endpoints, upstreamAPIKey, usage)
		router.messagesSvc.modelsDevProvider = router.modelsDevProvider
	}
	proxyRuntime, err := startLoopbackServer(parent, "mixed-protocol router", router.handlerForContext(parent), apiKey, usage)
	if err != nil {
		return nil, err
	}
	// Surface the catalog so callers like `gbx map` and `gbx provider models --all`
	// see the routed models instead of falling through to heuristics.
	proxyRuntime.models = append([]string(nil), routes.models...)
	if router.codex != nil {
		proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(router.codex.client))
	}
	if router.chatSvc != nil {
		proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(router.chatSvc.client))
	}
	if router.messagesSvc != nil {
		proxyRuntime.cleanup = append(proxyRuntime.cleanup, closeIdleClients(router.messagesSvc.client))
	}
	return proxyRuntime, nil
}

func (r *mixedProtocolRouter) handler() http.Handler {
	return r.handlerForContext(context.Background())
}

func (r *mixedProtocolRouter) handlerForContext(ctx context.Context) http.Handler {
	return loopbackFrontMuxForContext(ctx, r.apiKey, r.handleModels, r.handleResponses)
}

func (r *mixedProtocolRouter) handleModels(writer http.ResponseWriter, request *http.Request) {
	var routes map[string]string
	var details map[string]protocol.ModelInfo
	mergeService := func(svcRoutes map[string]string, svcDetails map[string]protocol.ModelInfo) {
		for k, v := range svcRoutes {
			if routes == nil {
				routes = make(map[string]string)
			}
			routes[k] = v
		}
		for k, v := range svcDetails {
			if details == nil {
				details = make(map[string]protocol.ModelInfo)
			}
			details[k] = v
		}
	}
	if r.codex != nil {
		mergeService(r.codex.modelRoute, r.codex.modelDetails)
	}
	if r.chatSvc != nil {
		mergeService(r.chatSvc.modelRoute, r.chatSvc.modelDetails)
	}
	if r.messagesSvc != nil {
		mergeService(r.messagesSvc.modelRoute, r.messagesSvc.modelDetails)
	}
	serveModelListWithProvider(writer, request, r.apiKey, r.models, details, routes, r.modelsDevProvider)
}

// handleResponses dispatches one Responses request to the service that owns
// the requested model's upstream protocol.
func (r *mixedProtocolRouter) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if !inferenceRequestAllowed(writer, request, r.apiKey) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, mixedMaxBodyBytes+1))
	if err != nil {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if int64(len(body)) > mixedMaxBodyBytes {
		writeProxyError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	model := requestModelID(body)
	lower := strings.ToLower(model)
	if r.codex != nil && r.responses[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "openai_responses", "owner", "gbx")
		r.codex.handleResponses(writer, request)
		return
	}
	if r.chatSvc != nil && r.chat[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "openai_chat", "owner", "gbx")
		r.chatSvc.handleResponses(writer, request)
		return
	}
	if r.messagesSvc != nil && r.messages[lower] {
		LogDebugEvent("protocol_route", "component", "mixed", "model", model, "protocol", "anthropic_messages", "owner", "gbx")
		r.messagesSvc.handleResponses(writer, request)
		return
	}
	writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "model is not routed to a supported protocol")
}
