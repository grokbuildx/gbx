// Package codex connects Codex clients to GBX providers without moving native
// Codex credentials. The desktop picker and routing allow-list are
// deliberately separate: only names owned by GBX can reach a GBX runtime.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/klauspost/compress/zstd"
)

const maxBodyBytes = 64 << 20

type Options struct {
	// DeferredStart lets the daemon finish its catalog/configuration plan before
	// serving clients reconnecting to a reused port.
	DeferredStart bool
	// CatalogRemote is opt-in for custom providers; native OpenAI only accepts
	// catalog URLs on allowed ChatGPT hosts. Local JSON remains the default.
	CatalogMode CatalogMode
	// Independent selects a custom provider with its own loopback credential,
	// rather than sharing OpenAI's provider/account state in the desktop app.
	Independent bool
	Desktop     bool
	// ListenAddress preserves the desktop endpoint across daemon replacement.
	// It may only name an explicit IPv4 loopback port; empty chooses a free one.
	ListenAddress string
	// Native upstream overrides are for local integration fixtures. Commands
	// never populate them from client requests or environment variables.
	OpenAIURL  string
	ChatGPTURL string
}

type Route struct {
	Provider         string
	Alias            string
	Model            string
	NativeResponses  bool
	CodexNativeTools bool
	MaxOutputTokens  int
}

type Gateway struct {
	runtime          *oauthproxy.Runtime
	listener         net.Listener
	server           *http.Server
	client           *http.Client
	token            string
	routes           map[string]Route
	models           []Model
	modelSnapshot    atomic.Pointer[catalogSnapshot]
	catalogMu        sync.RWMutex
	reloadMu         sync.Mutex
	options          Options
	history          responseHistory
	migrations       responseHistory
	migrationNotices responseHistory
	// Only compaction of a catalogued previous native model may use the
	// independent provider's selected default. Normal unknown routes still fail.
	legacyCompaction map[string]Route
	closeOnce        sync.Once
	startOnce        sync.Once
	closed           bool
	generation       *runtimeGeneration
	nativeCatalog    Catalog
}

func randomID(prefix string) string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// New initially borrows the runtime. OwnRuntime/ReplaceProviders transfer
// lifecycle cleanup to request-leased generations while the listener stays put.
func New(ctx context.Context, runtime *oauthproxy.Runtime, configured provider.Provider, options Options) (*Gateway, error) {
	if _, err := ParseCatalogMode(string(options.CatalogMode)); err != nil {
		return nil, err
	}
	options.CatalogMode = options.CatalogMode.Effective()
	if options.CatalogMode == CatalogRemote && !options.Independent {
		return nil, fmt.Errorf("remote GBX catalogs require independent mode; shared mode uses local JSON")
	}
	if err := provider.ValidateRuntime(configured); err != nil {
		return nil, err
	}
	address := options.ListenAddress
	if address == "" {
		address = "127.0.0.1:0"
	} else {
		host, port, err := net.SplitHostPort(address)
		n, portErr := strconv.Atoi(port)
		if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("Codex listener requires an explicit 127.0.0.1 port")
		}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // A local credential must never go through a shell proxy.
	g := &Gateway{runtime: runtime, listener: listener, options: options, token: randomID("gbx_"),
		routes: make(map[string]Route), client: &http.Client{Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := g.buildModels(runtime, configured); err != nil {
		_ = listener.Close()
		return nil, err
	}
	g.server = &http.Server{Handler: http.HandlerFunc(g.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	g.modelSnapshot.Store(g.currentLocked())
	if !options.DeferredStart {
		g.Start()
	}
	return g, nil
}

func (g *Gateway) Start() {
	g.startOnce.Do(func() { go func() { _ = g.server.Serve(g.listener) }() })
}

// buildModels only prepares an unpublished route/catalog pair.
func (g *Gateway) buildModels(runtime *oauthproxy.Runtime, configured provider.Provider) error {
	models, routes, err := buildProviderCatalog(runtime, configured)
	if err != nil {
		return err
	}
	g.models = models
	g.routes = routes
	return nil
}

func isNativeCodexModel(p provider.Provider, model string) (native, codexOAuth bool) {
	p.Type = provider.EffectiveType(p)
	oauth := strings.ToLower(strings.TrimSpace(p.OAuthProvider))
	codexOAuth = oauth == "gpt" || oauth == "codex"
	xaiAPI := oauthproxy.IsXaiAPIProvider(p)
	mixed := oauth == "" && len(p.ModelProtocols) > 0
	providerNative := codexOAuth ||
		(oauth == "" && !mixed && !xaiAPI && provider.IsOpenAIResponsesType(p.Type))
	if providerNative {
		return true, codexOAuth
	}
	if !mixed || xaiAPI {
		return false, false
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	modelProtocol := p.ModelProtocols[lower]
	if modelProtocol == "" {
		modelProtocol = p.ModelProtocols[provider.StripContextSuffix(lower)]
	}
	return provider.IsOpenAIResponsesType(modelProtocol), false
}

func buildProviderCatalog(runtime *oauthproxy.Runtime, configured provider.Provider) ([]Model, map[string]Route, error) {
	var err error
	p := configured
	p.Type = provider.EffectiveType(p)
	details, names := runtime.ModelDetails(), runtime.ModelDisplayNames()
	rawModels := runtime.Models()
	models := make([]string, 0, len(rawModels))
	seenModels := make(map[string]bool, len(rawModels))
	for _, raw := range rawModels {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if seenModels[key] {
			continue
		}
		seenModels[key] = true
		models = append(models, model)
	}
	details = withGPTReasoningDefaults(models, details)
	details, err = withModelsDevMetadataPerModel(models, details, names, p, func(model string) bool {
		modelNative, _ := isNativeCodexModel(p, model)
		return !modelNative
	})
	if err != nil {
		return nil, nil, err
	}
	routes := make(map[string]Route, len(models))
	catalogModels := make([]Model, 0, len(models))
	for _, model := range models {
		alias := "gbx/" + url.PathEscape(p.Name) + "/" + model
		_, info, _ := lookupModelInfo(details, model)
		modelNative, codexOAuth := isNativeCodexModel(p, model)
		routes[alias] = Route{Provider: p.Name, Alias: alias, Model: model, NativeResponses: modelNative, CodexNativeTools: codexOAuth, MaxOutputTokens: info.MaxOutputTokens}
		entry := newModel(alias, model, details, names)
		if modelNative && !codexOAuth {
			entry["supports_search_tool"] = false
		}
		applyProviderContext(entry, model, p)
		catalogModels = append(catalogModels, entry)
	}
	if p.OAuthProvider == oauthproxy.ProviderWorkBuddy || p.OAuthProvider == oauthproxy.ProviderWorkBuddyCN {
		catalogModels = uniqueWorkBuddyModels(catalogModels, routes)
	}
	disambiguateModelNames(catalogModels, routes)
	if len(catalogModels) == 0 {
		return nil, nil, fmt.Errorf("provider %q has no models for Codex", p.Name)
	}
	if def := provider.StripContextSuffix(provider.NormalizeModelID(p.DefaultModel)); def != "" {
		promoteDefaultCatalogModel(catalogModels, routes, names, def)
	}
	return catalogModels, routes, nil
}

func promoteDefaultCatalogModel(models []Model, routes map[string]Route, names map[string]string, def string) {
	for i := range models {
		slug, _ := models[i]["slug"].(string)
		modelID := routes[slug].Model
		if strings.EqualFold(provider.StripContextSuffix(modelID), def) || strings.EqualFold(strings.TrimSpace(names[modelID]), def) {
			if i > 0 {
				chosen := models[i]
				copy(models[1:i+1], models[:i])
				models[0] = chosen
			}
			return
		}
	}
}

func (g *Gateway) BaseURL() string { return "http://" + g.listener.Addr().String() + "/v1" }
func (g *Gateway) Token() string   { return g.token }
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		_ = g.server.Close()
		// Close may run before Serve registers its listener with the server.
		// Release it synchronously so the replacement can reuse this port.
		_ = g.listener.Close()
		g.client.CloseIdleConnections()
		g.catalogMu.Lock()
		g.closed = true
		gen := g.currentLocked().generation
		gen.retire()
		g.catalogMu.Unlock()
		gen.maybeClose()
	})
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || r.Host != g.listener.Addr().String() || r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "permission_error", "loopback client required", "")
		return
	}
	if !g.options.Desktop && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid local GBX key", "")
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusUpgradeRequired, "invalid_request_error", "GBX uses HTTP Responses; retry over HTTP", "")
		return
	}
	snapshot, release := g.acquireSnapshot()
	defer release()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	if r.Method == http.MethodGet && (path == "/models" || path == "/codex/models") {
		g.serveCatalog(w, r, snapshot)
		return
	}
	if r.Method != http.MethodPost || (path != "/responses" && path != "/responses/compact") {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unsupported Codex endpoint", "")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		var bodyErr *bodyReadError
		if errors.As(err, &bodyErr) {
			writeError(w, bodyErr.status, bodyErr.kind, bodyErr.msg, bodyErr.code)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	var request responseRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid Responses request JSON", "")
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "a Responses model is required", "")
		return
	}
	route, ok := snapshot.routes[request.Model]
	if !ok && g.options.Independent {
		items, _ := inputItems(request.Input)
		if isClientCompaction(r, request, items, path) {
			if replacement, allowed := snapshot.legacyCompaction[request.Model]; allowed {
				route, ok = replacement, true
				request.Model = route.Alias
				request.Reasoning = nil
				for _, model := range snapshot.models {
					if model["slug"] == route.Alias {
						if effort, _ := model["default_reasoning_level"].(string); effort != "" {
							request.Reasoning = map[string]any{"effort": effort}
						}
						break
					}
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(body, &fields)
				fields["model"], _ = json.Marshal(request.Model)
				if request.Reasoning == nil {
					delete(fields, "reasoning")
				} else {
					fields["reasoning"], _ = json.Marshal(request.Reasoning)
				}
				body, _ = json.Marshal(fields)
			}
		}
	}
	if !ok {
		if !g.options.Desktop || strings.HasPrefix(strings.ToLower(request.Model), "gbx/") {
			name := request.Model
			if len(name) > 128 {
				name = name[:128] + "..."
			}
			writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("model is not in the GBX routing catalog: %q; select a model from the active GBX provider or switch Codex back to the chat's provider", name), "model_not_found")
			return
		}
		g.serveNative(w, r, nativeHistory(body), path)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), runtimeRequestKey{}, snapshot.runtimes[route.Alias]))
	if route.NativeResponses {
		g.forwardResponses(w, r, nativeHistory(body), request, route, path)
		return
	}
	g.serveBridge(w, r, body, request, route, path == "/responses/compact")
}

type bodyReadError struct {
	status int
	kind   string
	code   string
	msg    string
}

func (e *bodyReadError) Error() string { return e.msg }

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	if r.ContentLength > maxBodyBytes {
		return nil, &bodyReadError{
			status: http.StatusRequestEntityTooLarge,
			kind:   "request_too_large",
			code:   "request_too_large",
			msg:    "Responses request body exceeds limit",
		}
	}
	var reader io.Reader = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "zstd":
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderMaxMemory(maxBodyBytes*2))
		if err != nil {
			return nil, &bodyReadError{
				status: http.StatusBadRequest,
				kind:   "invalid_request_error",
				msg:    "invalid zstd request",
			}
		}
		defer decoder.Close()
		reader = decoder
	default:
		return nil, &bodyReadError{
			status: http.StatusUnsupportedMediaType,
			kind:   "invalid_request_error",
			msg:    "unsupported request content encoding",
		}
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &bodyReadError{
				status: http.StatusRequestEntityTooLarge,
				kind:   "request_too_large",
				code:   "request_too_large",
				msg:    "Responses request body exceeds limit",
			}
		}
		return nil, &bodyReadError{
			status: http.StatusBadRequest,
			kind:   "invalid_request_error",
			msg:    "Responses request cannot be decoded",
		}
	}
	if len(body) > maxBodyBytes {
		return nil, &bodyReadError{
			status: http.StatusRequestEntityTooLarge,
			kind:   "request_too_large",
			code:   "request_too_large",
			msg:    "Responses request body exceeds limit",
		}
	}
	return body, nil
}

func (g *Gateway) forwardResponses(w http.ResponseWriter, r *http.Request, body []byte, request responseRequest, route Route, path string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		fields = map[string]json.RawMessage{}
	}
	items, err := inputItems(fields["input"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	owner := historyOwner(r)
	inputModified := false
	if request.PreviousResponseID != "" {
		previous, ok := g.history.get(request.PreviousResponseID, route.Alias, owner)
		if !ok {
			writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "previous_response_id is unknown, expired, or belongs to another route", "previous_response_not_found", "previous_response_id")
			return
		}
		items = append(previous, items...)
		delete(fields, "previous_response_id")
		inputModified = true
	}
	hasItemRef := false
	for _, item := range items {
		if kind, _ := item["type"].(string); kind == "item_reference" {
			hasItemRef = true
			break
		}
	}
	if hasItemRef {
		items, err = g.history.expandItemReferences(items, route.Alias, owner)
		if err != nil {
			writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "item_not_found", "input")
			return
		}
		inputModified = true
	}
	if !route.CodexNativeTools && hasOpaqueCompaction(items) {
		var migrated bool
		items, migrated, err = g.migrateNativeContext(r, route, items, request.ClientMetadata)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "context_migration_unavailable")
			return
		}
		if migrated || len(items) > 0 {
			inputModified = true
		}
	}
	if inputModified {
		fields["input"], _ = json.Marshal(items)
	}
	fields["model"], _ = json.Marshal(route.Model)
	body, _ = json.Marshal(fields)
	response, err := g.callRuntime(r, body, path)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "GBX provider connection failed", "server_error")
		return
	}
	defer response.Body.Close()

	shouldStore := path == "/responses" && (request.Store == nil || *request.Store) && response.StatusCode >= 200 && response.StatusCode < 300
	if !shouldStore {
		relayResponse(w, r, response)
		return
	}
	observer := newForwardResponseObserver(response.Header.Get("Content-Type"))
	relayResponseWithObserver(w, r, response, observer)
	if result := observer.terminalResponse(); result != nil {
		id, _ := result["id"].(string)
		if id != "" {
			full := make([]map[string]any, 0, len(items))
			full = append(full, items...)
			if output, ok := result["output"].([]any); ok {
				for _, value := range output {
					if item, ok := value.(map[string]any); ok {
						full = append(full, item)
					}
				}
			}
			g.history.put(id, route.Alias, owner, full)
		}
	}
}

func hasOpaqueCompaction(items []map[string]any) bool {
	for _, item := range items {
		if kind, _ := item["type"].(string); kind == "compaction" {
			encoded, _ := item["encrypted_content"].(string)
			if _, ok := decodeCompactionSummary(encoded); !ok {
				return true
			}
		}
	}
	return false
}

type forwardResponseObserver struct {
	sse       bool
	pending   []byte
	raw       bytes.Buffer
	overflow  bool
	terminal  map[string]any
	maxBuffer int
}

func newForwardResponseObserver(contentType string) *forwardResponseObserver {
	return &forwardResponseObserver{
		sse:       strings.Contains(strings.ToLower(contentType), "text/event-stream"),
		maxBuffer: bridgeResponseLimit,
	}
}

func (o *forwardResponseObserver) Observe(chunk []byte) {
	if o == nil || len(chunk) == 0 || o.terminal != nil {
		return
	}
	if !o.sse {
		if o.overflow {
			return
		}
		if o.raw.Len()+len(chunk) > o.maxBuffer {
			o.overflow = true
			o.raw.Reset()
			return
		}
		_, _ = o.raw.Write(chunk)
		return
	}
	o.pending = append(o.pending, chunk...)
	for {
		idx := bytes.Index(o.pending, []byte("\n\n"))
		sep := 2
		if crlf := bytes.Index(o.pending, []byte("\r\n\r\n")); crlf >= 0 && (idx < 0 || crlf < idx) {
			idx = crlf
			sep = 4
		}
		if idx < 0 {
			break
		}
		o.scanSSEFrame(o.pending[:idx])
		o.pending = o.pending[idx+sep:]
		if o.terminal != nil {
			o.pending = nil
			return
		}
	}
	if len(o.pending) > o.maxBuffer {
		o.pending = nil
	}
}

func (o *forwardResponseObserver) scanSSEFrame(frame []byte) {
	var data bytes.Buffer
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	payload := bytes.TrimSpace(data.Bytes())
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var event map[string]any
	if decodeBridgeJSON(payload, &event) != nil {
		return
	}
	kind, _ := event["type"].(string)
	if kind != "response.completed" && kind != "response.incomplete" {
		return
	}
	resp, _ := event["response"].(map[string]any)
	if isCacheableTerminalResponse(resp) {
		o.terminal = resp
	}
}

func (o *forwardResponseObserver) terminalResponse() map[string]any {
	if o == nil {
		return nil
	}
	if o.sse {
		if o.terminal == nil && len(o.pending) > 0 {
			o.scanSSEFrame(o.pending)
			o.pending = nil
		}
		return o.terminal
	}
	if o.overflow || o.raw.Len() == 0 {
		return nil
	}
	var resp map[string]any
	if decodeBridgeJSON(o.raw.Bytes(), &resp) != nil {
		return nil
	}
	if resp["object"] != "response" || !isCacheableTerminalResponse(resp) {
		return nil
	}
	return resp
}

func isCacheableTerminalResponse(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	status, _ := resp["status"].(string)
	return status == "completed" || status == "incomplete"
}

func (g *Gateway) callRuntime(r *http.Request, body []byte, path string) (*http.Response, error) {
	runtime := g.requestRuntime(r)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, runtime.Endpoint()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+runtime.APIKey())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream, application/json")
	for _, key := range []string{"Session_id", "Session-Id", "Thread-Id", "X-Codex-Turn-Metadata", "X-Codex-Session-Id", "X-Grok-Conv-Id", "X-Claude-Code-Session-Id"} {
		if value := r.Header.Get(key); value != "" {
			request.Header.Set(key, value)
		}
	}
	return g.client.Do(request)
}

func isBearerToken(header, token string) bool {
	header = strings.TrimSpace(header)
	if len(header) < len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return false
	}
	candidate := strings.TrimSpace(header[len("Bearer "):])
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1
}

func (g *Gateway) serveNative(w http.ResponseWriter, r *http.Request, body []byte, path string) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" || isBearerToken(auth, g.token) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "native OpenAI models require Codex's own login", "invalid_api_key")
		return
	}
	target := g.options.OpenAIURL
	if target == "" {
		target = "https://api.openai.com/v1"
	}
	if r.Header.Get("Chatgpt-Account-Id") != "" {
		target = g.options.ChatGPTURL
		if target == "" {
			target = "https://chatgpt.com/backend-api/codex"
		}
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		writeError(w, http.StatusBadGateway, "api_error", "invalid native upstream", "server_error")
		return
	}
	proxy := &httputil.ReverseProxy{FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.URL.Path = strings.TrimRight(u.Path, "/") + path
		pr.Out.Host = ""
		pr.Out.Body = io.NopCloser(bytes.NewReader(body))
		pr.Out.ContentLength = int64(len(body))
		pr.Out.Header.Del("Content-Encoding")
		pr.Out.Header.Del("Content-Length")
	}, ModifyResponse: func(resp *http.Response) error {
		if resp != nil && resp.Body != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = &nativeReasoningObserverReader{inner: resp.Body}
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeError(w, http.StatusBadGateway, "api_error", "native OpenAI connection failed", "server_error")
	}}
	proxy.ServeHTTP(w, r)
}

func isRelayedGatewayHeader(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "content-type", "retry-after", "retry-after-ms", "x-request-id", "request-id":
		return true
	}
	return strings.HasPrefix(lower, "x-ratelimit-") ||
		strings.HasPrefix(lower, "anthropic-ratelimit-") ||
		strings.HasPrefix(lower, "x-codex-")
}

func relayResponse(w http.ResponseWriter, r *http.Request, response *http.Response) {
	relayResponseWithObserver(w, r, response, nil)
}

func relayResponseWithObserver(w http.ResponseWriter, r *http.Request, response *http.Response, observer *forwardResponseObserver) {
	for key, values := range response.Header {
		if !isRelayedGatewayHeader(key) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				w.Header().Set(key, value)
				break
			}
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := response.Body.Read(buf)
		if n > 0 {
			if observer != nil {
				observer.Observe(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flushErr := http.NewResponseController(w).Flush(); flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
				return
			}
		}
		if err != nil {
			if err != io.EOF && (r == nil || r.Context().Err() == nil) {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, kind, message, code string) {
	writeErrorParam(w, status, kind, message, code, "")
}

func writeErrorParam(w http.ResponseWriter, status int, kind, message, code, param string) {
	var codeValue any
	if code != "" {
		codeValue = code
	}
	var paramValue any
	if param != "" {
		paramValue = param
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message, "code": codeValue, "param": paramValue}})
}

func historyOwner(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:])
}
