package oauthproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/tidwall/gjson"
)

func TestBatch4ProviderHintInferenceAndXaiProtocolBoundary(t *testing.T) {
	// 1. Host-first matching never misclassifies URL paths like /v1beta/openai or /inference/openai/v1.
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"custom", "https://generativelanguage.googleapis.com/v1beta/openai", "google"},
		{"custom", "https://opencode.ai/inference/openai/v1", ""},
		{"custom", "https://api.x.ai/v1", "xai"},
		{"myxai-proxy", "https://proxy.example.com/v1", ""},
		{"xai-prod", "https://proxy.example.com/v1", "xai"},
		{"gemini-lab", "https://proxy.example.com/v1", "google"},
	}
	for _, tc := range cases {
		if got := inferModelsDevProviderFromHint(tc.name, tc.endpoint); got != tc.want {
			t.Errorf("inferModelsDevProviderFromHint(%q, %q) = %q, want %q", tc.name, tc.endpoint, got, tc.want)
		}
	}

	// 2. IsXaiAPIProvider never allows free-form provider names to hijack Responses protocol routing.
	if IsXaiAPIProvider(provider.Provider{Name: "my-grok-gateway", Type: "openai_responses", Endpoint: "https://proxy.example.com/v1"}) {
		t.Fatal("free-form provider name must not trigger IsXaiAPIProvider")
	}
	if !IsXaiAPIProvider(provider.Provider{Name: "any", Type: "openai_responses", Endpoint: "https://api.x.ai/v1"}) {
		t.Fatal("official api.x.ai endpoint must trigger IsXaiAPIProvider")
	}
	if !IsXaiAPIProvider(provider.Provider{Name: "any", Type: "openai_responses", Endpoint: "https://proxy.example.com/v1", ModelsDevProvider: "xai"}) {
		t.Fatal("explicit ModelsDevProvider=xai must trigger IsXaiAPIProvider")
	}
}

func TestBatch4MixedXaiFlattenToolsAndModelNotFound404(t *testing.T) {
	// 1. Mixed-protocol router enables flattenTools on xAI Responses bucket, handles [1m],
	// validates malformed JSON, and returns 404 model_not_found for unknown models.
	var sawFlattened atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(b, "tools.0.type").String() == "function" {
			sawFlattened.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer upstream.Close()

	rt, err := startMixedProtocolAPIKeyRuntimeWithProvider(
		context.Background(),
		upstream.URL+"/v1",
		"k",
		"grok-4,claude-sonnet",
		map[string]string{"GROK-4": "openai-responses", "CLAUDE-SONNET": "anthropic"},
		map[string]string{"CLAUDE-SONNET": upstream.URL + "/v1"},
		"xai",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(payload string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	// Custom tool on xAI Responses route in mixed router must be flattened!
	resp, b := post(`{"model":"grok-4[1m]","stream":false,"tools":[{"type":"custom","name":"exec","description":"run"}],"input":"hi"}`)
	if resp.StatusCode != http.StatusOK || !sawFlattened.Load() {
		t.Fatalf("expected mixed xAI route to flatten custom tools and honor [1m], status=%d flattened=%v body=%s", resp.StatusCode, sawFlattened.Load(), b)
	}

	// Malformed JSON -> 400 invalid JSON.
	resp, b = post(`{"model":`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "invalid Responses request JSON") {
		t.Fatalf("expected 400 invalid JSON, got status=%d body=%s", resp.StatusCode, b)
	}

	// Unknown model -> 404 model_not_found.
	resp, b = post(`{"model":"does-not-exist","input":"hi"}`)
	if resp.StatusCode != http.StatusNotFound || gjson.GetBytes(b, "error.code").String() != "model_not_found" {
		t.Fatalf("expected 404 model_not_found, got status=%d body=%s", resp.StatusCode, b)
	}

	// Anthropic messagesURL falls back to stripped [1m] key.
	anthSvc := newAnthropicMessagesService("k", "https://default.example/v1", []runtimeModelRoute{{Name: "claude-sonnet", Alias: "claude-sonnet"}}, map[string]string{"claude-sonnet": "https://custom-anth.example/v1"}, "uk", NewUsageTracker())
	if got := anthSvc.messagesURL("claude-sonnet[1m]"); !strings.HasPrefix(got, "https://custom-anth.example/v1") {
		t.Fatalf("messagesURL([1m]) = %q, want custom endpoint", got)
	}
}

func TestBatch4GetSingleModelEndpointAndWorkBuddyQoderModalities(t *testing.T) {
	// 1. GET /v1/models/{id...} works for exact, case-insensitive, [1m], and slash-containing IDs,
	// enforces auth, and returns 404 model_not_found for unknown models.
	yes := true
	details := map[string]protocol.ModelInfo{
		"target-a": {
			ID:                         "target-a",
			DisplayName:                "Target A",
			ContextWindow:              256000,
			SupportsParallelToolCalls:  &yes,
			SupportsReasoningSummaries: &yes,
		},
	}
	routes := map[string]string{"alias-a": "target-a"}
	modelsHandler := func(w http.ResponseWriter, r *http.Request) {
		serveModelListWithProvider(w, r, "secret", []string{"alias-a", "ALIAS-A", " ", "google/gemini-2.5-pro-high"}, details, routes, "")
	}
	mux := loopbackFrontMux("secret", modelsHandler, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// List deduplicates case-insensitively, skips blank, and resolves target detail via routes.
	listReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listReq.Header.Set("Authorization", "Bearer secret")
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	listData := gjson.GetBytes(listRec.Body.Bytes(), "data").Array()
	if len(listData) != 2 {
		t.Fatalf("expected 2 deduplicated non-empty models, got %d: %s", len(listData), listRec.Body.String())
	}
	if listData[0].Get("context_window").Int() != 256000 || !listData[0].Get("supports_parallel_tool_calls").Bool() {
		t.Fatalf("expected target detail lookup fallback & capability fields: %s", listData[0].Raw)
	}
	if listData[1].Get("supports_reasoning_effort").Bool() {
		t.Fatalf("expected google/gemini-2.5-pro-high to have no effort slider: %s", listData[1].Raw)
	}

	// Single model lookup including slash-containing ID and [1m] fallback.
	for _, path := range []string{"/v1/models/alias-a", "/models/ALIAS-A[1m]", "/v1/models/google/gemini-2.5-pro-high"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || gjson.GetBytes(rec.Body.Bytes(), "object").String() != "model" {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// Missing model returns 404 model_not_found.
	missReq := httptest.NewRequest(http.MethodGet, "/v1/models/nope", nil)
	missReq.Header.Set("Authorization", "Bearer secret")
	missRec := httptest.NewRecorder()
	mux.ServeHTTP(missRec, missReq)
	if missRec.Code != http.StatusNotFound || gjson.GetBytes(missRec.Body.Bytes(), "error.code").String() != "model_not_found" {
		t.Fatalf("expected 404 model_not_found, got %d %s", missRec.Code, missRec.Body.String())
	}

	// Unauthorized returns 401.
	unauthReq := httptest.NewRequest(http.MethodGet, "/v1/models/alias-a", nil)
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthRec.Code)
	}

	// 2. WorkBuddy explicit supportsImages:false produces ["text"], while omitted stays nil.
	var wbModels []workbuddyModel
	if err := json.Unmarshal([]byte(`[
		{"id":"text-only","name":"Text Only","supportsImages":false,"credits":"x1.00"},
		{"id":"unset-vis","name":"Unset","credits":"x1.00"},
		{"id":"has-vis","name":"Has Vis","supportsImages":true,"credits":"x1.00"}
	]`), &wbModels); err != nil {
		t.Fatal(err)
	}
	_, _, _, wbDetails := buildWorkBuddyRoutes("", wbModels)
	if !slices.Equal(wbDetails["text-only"].InputModalities, []string{"text"}) {
		t.Fatalf("explicit supportsImages:false must emit [text], got %+v", wbDetails["text-only"])
	}
	if len(wbDetails["unset-vis"].InputModalities) != 0 {
		t.Fatalf("omitted supportsImages must stay empty for models.dev backfill, got %+v", wbDetails["unset-vis"])
	}
	if !slices.Equal(wbDetails["has-vis"].InputModalities, []string{"text", "image"}) {
		t.Fatalf("supportsImages:true must emit [text image], got %+v", wbDetails["has-vis"])
	}

	// 3. Qoder explicit is_vl:false sets VisionKnown and emits ["text"].
	qInfo, ok := qoderModelDetail(qoderModel{ID: "q-text", Vision: false, VisionKnown: true})
	if !ok || !slices.Equal(qInfo.InputModalities, []string{"text"}) {
		t.Fatalf("Qoder explicit is_vl:false must emit [text], got ok=%v info=%+v", ok, qInfo)
	}
}

func TestBatch4KiroCursorAutoClawAndXaiDiscoveryHardening(t *testing.T) {
	// 1. Kiro rejects empty catalog and honors stale-cache failure backoff without re-hitting upstream.
	var hits atomic.Int32
	var returnEmpty atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if returnEmpty.Load() {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4-6"}]}`)
	}))
	defer upstream.Close()

	authDir := t.TempDir()
	credPath := filepath.Join(authDir, "kiro-b4.json")
	if err := os.WriteFile(credPath, []byte(`{"type":"kiro","access_token":"tok","expires_at":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := newKiroModelCatalog(func(region string) string { return upstream.URL + "/" + region })
	cat.ttl = time.Millisecond
	svc := &kiroService{pool: newKiroCredentialPool(authDir, ""), client: upstream.Client(), modelCatalog: cat}

	// Empty catalog must error (never cached).
	returnEmpty.Store(true)
	if _, err := svc.availableModels(context.Background()); err == nil {
		t.Fatal("expected empty Kiro model catalog to return error")
	}

	// Valid catalog caches; after TTL expires and upstream returns empty/error, stale cache serves with backoff.
	returnEmpty.Store(false)
	models, err := svc.availableModels(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("expected 1 model, got %+v err=%v", models, err)
	}
	time.Sleep(5 * time.Millisecond)
	returnEmpty.Store(true)
	beforeHits := hits.Load()
	models, err = svc.availableModels(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("expected stale cache fallback, got %+v err=%v", models, err)
	}
	afterFirstFailure := hits.Load()
	if afterFirstFailure <= beforeHits {
		t.Fatal("expected expired cache to attempt refresh once")
	}
	// Immediate second call must use backoff and NOT hit upstream again.
	if _, err := svc.availableModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != afterFirstFailure {
		t.Fatalf("expected stale-cache backoff to avoid immediate retry, hits went %d -> %d", afterFirstFailure, hits.Load())
	}

	// 2. Cursor effective models deduplicates case-insensitively.
	eff, fb := cursorEffectiveModels("Claude-Sonnet", []string{"claude-sonnet", "gpt-5"})
	if fb || !slices.Equal(eff, []string{"Claude-Sonnet", "gpt-5"}) {
		t.Fatalf("cursorEffectiveModels case-insensitive dedup failed: %v fb=%v", eff, fb)
	}

	// 3. addXaiDisplayAliases does not overwrite an existing technical model route.
	xaiSvc := newCodexResponsesService("k", "https://api.x.ai/v1", []runtimeModelRoute{{Name: "grok-4", Alias: "grok-4"}}, &codexStaticAuthorizer{token: "t"}, NewUsageTracker())
	addXaiDisplayAliases(xaiSvc, []xaiModel{{ID: "grok-other", Name: "grok-4"}})
	if xaiSvc.modelRoute["grok-4"] != "grok-4" {
		t.Fatalf("addXaiDisplayAliases overwrote technical ID route: %+v", xaiSvc.modelRoute)
	}

	// 4. AutoClaw image reroute re-runs normalizeChatReasoning for the rerouted non-reasoning image model.
	acDetails := autoClawModelDetailsWithCatalog(nil, []autoclawModelDefinition{
		{id: "text-reasoner", name: "Text Reasoner", reasoning: true, input: []string{"text"}},
		{id: "vision-plain", name: "Vision Plain", reasoning: false, input: []string{"text", "image"}},
	})
	if acDetails["text-reasoner"].ReasoningKnown {
		t.Fatalf("AutoClaw reasoning model should keep ReasoningKnown=false when not in models.dev so models.dev can refine tiers: %+v", acDetails["text-reasoner"])
	}
	if !acDetails["vision-plain"].ReasoningKnown {
		t.Fatalf("AutoClaw non-reasoning model should stay ReasoningKnown=true: %+v", acDetails["vision-plain"])
	}
	chatSvc := newChatCompletionsService("k", "https://example.invalid/v1", []runtimeModelRoute{{Name: "text-reasoner", Alias: "text-reasoner"}}, "uk", NewUsageTracker())
	chatSvc.modelDetails = acDetails
	chatSvc.normalizeRequest = func(c *chatCompletionsConvertedRequest) error {
		c.model = "vision-plain"
		c.upstreamModel = "vision-plain"
		return nil
	}
	conv, err := chatSvc.convertRequest([]byte(`{"model":"text-reasoner","reasoning":{"effort":"high"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(conv.body, "reasoning_effort").Exists() {
		t.Fatalf("expected reasoning_effort stripped after AutoClaw rerouted to non-reasoning vision-plain: %s", conv.body)
	}

	// 5. countChatInputTokens uses o200k_base for gpt-6 family.
	if _, est := countChatInputTokens("gpt-6.1-sol", []byte(`{"messages":[]}`)); est != "o200k_base" {
		t.Fatalf("countChatInputTokens(gpt-6.1-sol) estimator = %q, want o200k_base", est)
	}
}
