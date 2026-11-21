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
	"sync"
	"testing"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/provider"
	"github.com/tidwall/gjson"
)

func seedCustomModelsDevCache(t *testing.T, rawJSON string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := modelsdev.CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rawJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackModelsPropagatesProviderIDForAmbiguousAliases(t *testing.T) {
	seedCustomModelsDevCache(t, `{
		"alpha": {"models": {"shared-model": {"id": "shared-model", "reasoning": true, "reasoning_options": [{"type": "effort", "values": ["low", "high"]}]}}},
		"beta":  {"models": {"shared-model": {"id": "shared-model", "reasoning": false}}}
	}`)

	reqAlpha := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqAlpha.Header.Set("Authorization", "Bearer k1")
	recAlpha := httptest.NewRecorder()
	serveModelListWithProvider(recAlpha, reqAlpha, "k1", []string{"shared-model"}, nil, nil, "alpha")
	if recAlpha.Code != http.StatusOK {
		t.Fatalf("alpha status = %d: %s", recAlpha.Code, recAlpha.Body.String())
	}
	alphaEntry := gjson.GetBytes(recAlpha.Body.Bytes(), `data.#(id=="shared-model")`)
	var alphaEfforts []string
	for _, item := range alphaEntry.Get("reasoning_efforts").Array() {
		alphaEfforts = append(alphaEfforts, item.Get("value").String())
	}
	if !alphaEntry.Get("supports_reasoning_effort").Bool() || !slices.Equal(alphaEfforts, []string{"low", "high"}) {
		t.Fatalf("alpha /v1/models = %s, want [low high]", alphaEntry.Raw)
	}

	reqBeta := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqBeta.Header.Set("Authorization", "Bearer k1")
	recBeta := httptest.NewRecorder()
	serveModelListWithProvider(recBeta, reqBeta, "k1", []string{"shared-model"}, nil, nil, "beta")
	if recBeta.Code != http.StatusOK {
		t.Fatalf("beta status = %d: %s", recBeta.Code, recBeta.Body.String())
	}
	betaEntry := gjson.GetBytes(recBeta.Body.Bytes(), `data.#(id=="shared-model")`)
	if betaEntry.Get("supports_reasoning_effort").Type != gjson.False || betaEntry.Get("reasoning_efforts").Exists() {
		t.Fatalf("beta /v1/models = %s, want supports_reasoning_effort=false", betaEntry.Raw)
	}
}

func TestXaiAndCodexDiscoveryMarkEmptyReasoningLevelsKnown(t *testing.T) {
	xaiModels, err := parseXaiModels([]byte(`{"data":[
		{"id":"grok-empty-list","api_backend":"responses","supported_reasoning_levels":[]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(xaiModels) != 1 || !xaiModels[0].ReasoningKnown || len(xaiModels[0].ReasoningLevels) != 0 {
		t.Fatalf("parseXaiModels explicit empty list = %+v, want ReasoningKnown=true and empty levels", xaiModels)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"models": []any{
			map[string]any{"slug": "gpt-plain", "visibility": "list", "supported_reasoning_levels": []any{}},
			map[string]any{"slug": "gpt-think", "visibility": "list", "default_reasoning_level": "high", "supported_reasoning_levels": []any{map[string]any{"effort": "low"}, map[string]any{"effort": "high"}}},
		}})
	}))
	defer server.Close()
	infos, status, err := requestCodexModelInfos(context.Background(), server.Client(), server.URL+"/v1", codexResponsesAuthorization{token: "fake-key"})
	if err != nil || status != http.StatusOK || len(infos) != 2 {
		t.Fatalf("requestCodexModelInfos = (%d, %v, %+v)", status, err, infos)
	}
	if !infos[0].ReasoningKnown || len(infos[0].ReasoningLevels) != 0 {
		t.Fatalf("gpt-plain should be ReasoningKnown=true with empty levels: %+v", infos[0])
	}
	if !infos[1].ReasoningKnown || len(infos[1].ReasoningLevels) != 2 {
		t.Fatalf("gpt-think should be ReasoningKnown=true with 2 levels: %+v", infos[1])
	}
}

func TestStaticProviderModelDetailsQoderFallbackAndKimiProviderID(t *testing.T) {
	kimi := provider.Provider{Name: "kimi-sub", OAuthProvider: ProviderKimi}
	if got := ModelsDevProviderID(kimi); got != "kimi-code-plan-global" {
		t.Fatalf("ModelsDevProviderID(kimi) = %q, want kimi-code-plan-global", got)
	}

	qoder := provider.Provider{Name: "qoder-sub", OAuthProvider: ProviderQoder, Model: "qmodel,custom-q"}
	models, _, details, _ := StaticProviderModelDetails(qoder)
	if len(models) != 2 || models[0] != "qmodel" {
		t.Fatalf("qoder static models = %v", models)
	}
	// Fallback models must still supply details even when p.Model is non-empty.
	fb := qoderFallbackModels()
	matchedAny := false
	for _, m := range fb {
		if info, ok := qoderModelDetail(m); ok {
			if _, present := details[strings.ToLower(m.ID)]; !present {
				t.Fatalf("missing Qoder fallback detail for %s (info=%+v)", m.ID, info)
			}
			matchedAny = true
		}
	}
	if !matchedAny && len(fb) > 0 {
		t.Log("qoder fallback catalog had no detailed models")
	}
}

func TestChatRuntimeClampsStripsAndRetriesRejectedReasoningEffort(t *testing.T) {
	seedCustomModelsDevCache(t, `{
		"mygw": {
			"models": {
				"plain-chat": {"id": "plain-chat", "reasoning": false},
				"three-tier": {"id": "three-tier", "reasoning": true, "reasoning_options": [{"type": "effort", "values": ["low", "medium", "high"]}]}
			}
		}
	}`)

	var mu sync.Mutex
	var captured []map[string]any
	var strictCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, body)
		model, _ := body["model"].(string)
		_, hasEffort := body["reasoning_effort"]
		if model == "unknown-strict" && hasEffort {
			strictCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`)
			return
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	rt, err := StartProvider(context.Background(), StartOptions{
		Protocol:          ProtocolOpenAIChat,
		Endpoint:          upstream.URL + "/v1",
		APIKey:            "sk-test",
		ModelSpec:         "plain-chat,three-tier,unknown-strict",
		ModelsDevProvider: "mygw",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(model, effort string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"model":     model,
			"stream":    false,
			"reasoning": map[string]any{"effort": effort},
			"input":     []map[string]any{{"role": "user", "content": "hi"}},
		})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("model=%s effort=%s status=%d body=%s", model, effort, resp.StatusCode, body)
		}
	}

	// 1. Non-reasoning model strips reasoning_effort up front.
	post("plain-chat", "high")
	// 2. Three-tier model clamps ultra -> high up front.
	post("three-tier", "ultra")
	// 3. Unknown strict model rejects reasoning_effort with HTTP 400, runtime retries once without it.
	post("unknown-strict", "max")

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 4 {
		t.Fatalf("upstream received %d requests, want 4 (1 plain + 1 three-tier + 2 unknown-strict): %+v", len(captured), captured)
	}
	if _, exists := captured[0]["reasoning_effort"]; exists {
		t.Fatalf("plain-chat should strip reasoning_effort: %+v", captured[0])
	}
	if got := captured[1]["reasoning_effort"]; got != "high" {
		t.Fatalf("three-tier reasoning_effort = %v, want high", got)
	}
	if strictCalls != 1 {
		t.Fatalf("unknown-strict rejection count = %d, want 1", strictCalls)
	}
	if _, exists := captured[3]["reasoning_effort"]; exists {
		t.Fatalf("unknown-strict fallback retry should omit reasoning_effort: %+v", captured[3])
	}
}

func TestAnthropicMessagesRuntimeGuardsThinkingAndRetriesOn400(t *testing.T) {
	seedCustomModelsDevCache(t, `{
		"anthropic": {
			"models": {
				"claude-plain":  {"id": "claude-plain", "reasoning": false},
				"claude-budget": {"id": "claude-budget", "reasoning": true, "reasoning_options": [{"type": "budget_tokens"}]}
			}
		}
	}`)

	var mu sync.Mutex
	var captured []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, body)
		model, _ := body["model"].(string)
		_, hasThinking := body["thinking"]
		mu.Unlock()
		if model == "claude-strict-unknown" && hasThinking {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking is not supported for this model"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE(
			`{"type":"message_start","message":{"id":"msg_ok","usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer upstream.Close()

	rt, err := StartProvider(context.Background(), StartOptions{
		Endpoint:  upstream.URL + "/v1",
		APIKey:    "sk-ant",
		ModelSpec: "claude-plain,claude-budget,claude-strict-unknown",
		ModelProtocols: map[string]string{
			"claude-plain":          "anthropic",
			"claude-budget":         "anthropic",
			"claude-strict-unknown": "anthropic",
		},
		ModelsDevProvider: "anthropic",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	post := func(model string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"model":     model,
			"stream":    false,
			"reasoning": map[string]any{"effort": "high"},
			"input":     []map[string]any{{"role": "user", "content": "hi"}},
		})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, rt.Endpoint()+"/responses", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer "+rt.APIKey())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("model=%s status=%d body=%s", model, resp.StatusCode, body)
		}
	}

	// 1. Non-reasoning model strips thinking up front.
	post("claude-plain")
	// 2. Budget-only reasoning model keeps thinking enabled despite empty Efforts().
	post("claude-budget")
	// 3. Unknown model whose upstream rejects thinking retries once without thinking.
	post("claude-strict-unknown")

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 4 {
		t.Fatalf("captured %d upstream calls, want 4: %+v", len(captured), captured)
	}
	if _, has := captured[0]["thinking"]; has {
		t.Fatalf("claude-plain should omit thinking: %+v", captured[0])
	}
	if _, has := captured[1]["thinking"]; !has {
		t.Fatalf("claude-budget must preserve thinking: %+v", captured[1])
	}
	if _, has := captured[2]["thinking"]; !has {
		t.Fatalf("claude-strict-unknown first attempt should include thinking: %+v", captured[2])
	}
	if _, has := captured[3]["thinking"]; has {
		t.Fatalf("claude-strict-unknown fallback retry should omit thinking: %+v", captured[3])
	}
}
