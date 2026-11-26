package protocol_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grokbuildx/gbx/internal/codexidentity"
	"github.com/grokbuildx/gbx/internal/protocol"
)

func TestGetOpenAIModelsContextAppendsModelsToRootEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer server.Close()

	models, err := protocol.GetOpenAIModelsContext(context.Background(), server.URL, "test-key")
	if err != nil {
		t.Fatalf("GetOpenAIModelsContext failed: %v", err)
	}
	if models != "gpt-4o" {
		t.Fatalf("unexpected models: %s", models)
	}
}

func TestGetOpenAIModelsContextPreservesVersionedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/models" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"v3-model"}]}`))
	}))
	defer server.Close()

	models, err := protocol.GetOpenAIModelsContext(context.Background(), server.URL+"/v3", "test-key")
	if err != nil {
		t.Fatalf("GetOpenAIModelsContext failed: %v", err)
	}
	if models != "v3-model" {
		t.Fatalf("unexpected models: %s", models)
	}
}

func TestNormalizeVersionedURLs(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "openai models from v3 base",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/models",
		},
		{
			name: "openai chat from v3 base",
			got:  protocol.NormalizeOpenAIChatCompletionsURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/chat/completions",
		},
		{
			name: "openai responses from v3 base",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com/api/v3"),
			want: "https://example.com/api/v3/responses",
		},
		{
			name: "openai models from v3 chat",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com/api/v3/chat/completions"),
			want: "https://example.com/api/v3/models",
		},
		{
			name: "openai responses from v3 chat",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com/api/v3/chat/completions"),
			want: "https://example.com/api/v3/responses",
		},
		{
			name: "unversioned openai models appends directly",
			got:  protocol.NormalizeOpenAIModelsURL("https://example.com"),
			want: "https://example.com/models",
		},
		{
			name: "unversioned openai chat appends directly",
			got:  protocol.NormalizeOpenAIChatCompletionsURL("https://example.com"),
			want: "https://example.com/chat/completions",
		},
		{
			name: "unversioned openai responses appends directly",
			got:  protocol.NormalizeOpenAIResponsesURL("https://example.com"),
			want: "https://example.com/responses",
		},
		{
			name: "empty openai uses official v1 default",
			got:  protocol.NormalizeOpenAIModelsURL(""),
			want: "https://api.openai.com/v1/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestResponsesURLsTreatCodexAsOrdinaryBasePath(t *testing.T) {
	base := "https://new.sharedchat.cc/codex/v1"
	if got := protocol.NormalizeOpenAIModelsURL(base); got != base+"/models" {
		t.Fatalf("models URL = %q", got)
	}
	if got := protocol.NormalizeOpenAIResponsesURL(base); got != base+"/responses" {
		t.Fatalf("responses URL = %q", got)
	}
}

func TestGetOpenAIModelInfosIncludesContextWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"big-model","token_limits":{"context_window":1000000}},{"id":"small-model","token_limits":{"context_window":128000}}]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetOpenAIModelInfos: %v", err)
	}
	byID := map[string]int{}
	for _, info := range infos {
		byID[info.ID] = info.ContextWindow
	}
	if byID["big-model"] != 1000000 || byID["small-model"] != 128000 {
		t.Fatalf("context windows = %#v", byID)
	}
	if !protocol.ContextWindowSuggests1M(1000000) || protocol.ContextWindowSuggests1M(128000) {
		t.Fatal("ContextWindowSuggests1M thresholds unexpected")
	}
}

func TestGetOpenAIModelInfosPreservesInputModalities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"direct","input_modalities":["text","image"]},{"id":"nested","modalities":{"input_modalities":["text","image"]}},{"id":"anthropic","supported_input_types":["text","image"]},{"id":"unknown"}]}`))
	}))
	t.Cleanup(server.Close)
	infos, err := protocol.GetOpenAIModelInfos(server.URL, "fake-key")
	if err != nil || len(infos) != 4 {
		t.Fatalf("infos=%+v err=%v", infos, err)
	}
	for _, info := range infos {
		if info.ID == "unknown" {
			if len(info.InputModalities) != 0 {
				t.Fatal("invented unknown model capabilities")
			}
		} else if strings.Join(info.InputModalities, ",") != "text,image" {
			t.Fatalf("image capability lost: %+v", info)
		}
	}
}

func TestGetOpenAIModelInfosAcceptsTopLevelContextWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"sdk-model","context_window":1000000},{"id":"nested","token_limits":{"context_window":200000}}]}`))
	}))
	t.Cleanup(server.Close)
	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, info := range infos {
		byID[info.ID] = info.ContextWindow
	}
	if byID["sdk-model"] != 1000000 {
		t.Fatalf("top-level context_window not parsed: %#v", byID)
	}
	if byID["nested"] != 200000 {
		t.Fatalf("nested context_window not parsed: %#v", byID)
	}
}

func TestGetOpenAIModelInfosKeepsRateMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"priced","rate_multiplier":0.29,"rate_unit":"credits","is_new":true,"promotion_available":true},{"id":"plain"}]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetOpenAIModelInfos: %v", err)
	}
	byID := map[string]protocol.ModelInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}
	priced := byID["priced"]
	if priced.RateMultiplier == nil || *priced.RateMultiplier != 0.29 || priced.RateUnit != "credits" || !priced.IsNew || !priced.PromotionAvailable {
		t.Fatalf("priced metadata = %+v", priced)
	}
	if plain := byID["plain"]; plain.RateMultiplier != nil || plain.RateUnit != "" {
		t.Fatalf("unpriced metadata = %+v", plain)
	}
}

func TestGetCodexClientModelInfosRequiresClientVersion(t *testing.T) {
	var sawClientVersion bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["client_version"]; !ok {
			// The plain catalog trims entries to identity fields only.
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol","object":"model"}]}`))
			return
		}
		sawClientVersion = true
		if r.Header.Get("Originator") != codexidentity.Originator || r.Header.Get("Version") != codexidentity.ClientVersion ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), codexidentity.Originator+"/") {
			t.Errorf("Codex catalog identity headers = %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","context_window":272000},
			{"id":"legacy-id","max_context_window":128000},
			{"slug":"no-window"}
		]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetCodexClientModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetCodexClientModelInfos: %v", err)
	}
	if !sawClientVersion {
		t.Fatal("request did not carry client_version, so the trimmed list would be parsed")
	}
	byID := map[string]int{}
	for _, info := range infos {
		byID[info.ID] = info.ContextWindow
	}
	if byID["gpt-5.6-sol"] != 272000 {
		t.Errorf("slug entry window = %d, want 272000", byID["gpt-5.6-sol"])
	}
	if byID["legacy-id"] != 128000 {
		t.Errorf("id entry falling back to max_context_window = %d, want 128000", byID["legacy-id"])
	}
	if window, ok := byID["no-window"]; !ok || window != 0 {
		t.Errorf("entry without a window = %d (present=%t), want 0", window, ok)
	}
}

func TestGetOpenAIModelInfosNormalizesModalitiesAndTokenLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"kiro-style","max_input_tokens":200000,"max_output_tokens":64000,"supported_input_types":["TEXT","IMAGE"]},
			{"id":"modelsdev-style","modalities":{"input":["IMAGE","TEXT"]},"token_limits":{"context_window":500000,"max_output_token_length":32768}},
			{"id":"plain-unknown"}
		]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetOpenAIModelInfos: %v", err)
	}
	byID := map[string]protocol.ModelInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}
	kiro := byID["kiro-style"]
	if kiro.ContextWindow != 200000 || kiro.MaxOutputTokens != 64000 || strings.Join(kiro.InputModalities, ",") != "text,image" {
		t.Fatalf("kiro-style metadata = %+v", kiro)
	}
	mdev := byID["modelsdev-style"]
	if mdev.ContextWindow != 500000 || mdev.MaxOutputTokens != 32768 || strings.Join(mdev.InputModalities, ",") != "text,image" {
		t.Fatalf("modelsdev-style metadata = %+v", mdev)
	}
	if unknown := byID["plain-unknown"]; len(unknown.InputModalities) != 0 || unknown.MaxOutputTokens != 0 || unknown.ReasoningKnown {
		t.Fatalf("invented metadata for unknown model = %+v", unknown)
	}
}

func TestGetOpenAIModelInfosDecodesGrokReasoningEffortsAndCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{
				"id":"grok-4.6",
				"supports_reasoning_effort":true,
				"reasoning_effort_server_default":"high",
				"reasoning_efforts":[
					{"value":"low","label":"Low Effort","description":"Quick"},
					{"value":"medium","label":"Medium Effort"},
					{"value":"high","label":"High Effort","default":true},
					{"value":"xhigh","label":"Extra High Effort"}
				]
			},
			{
				"id":"cap-model",
				"capabilities":{"default_reasoning_effort":"medium","reasoning_effort":["low","medium","high"]}
			},
			{
				"id":"disabled-reasoning",
				"supports_reasoning_effort":false,
				"reasoning_efforts":[{"value":"high"}]
			}
		]}`))
	}))
	t.Cleanup(server.Close)

	infos, err := protocol.GetOpenAIModelInfos(server.URL, "key")
	if err != nil {
		t.Fatalf("GetOpenAIModelInfos: %v", err)
	}
	byID := map[string]protocol.ModelInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}
	grok := byID["grok-4.6"]
	if !grok.ReasoningKnown || len(grok.ReasoningLevels) != 4 || grok.DefaultReasoningLevel != "high" || grok.ReasoningLevels[0].Description != "Quick" {
		t.Fatalf("grok-4.6 reasoning metadata = %+v", grok)
	}
	capModel := byID["cap-model"]
	if !capModel.ReasoningKnown || len(capModel.ReasoningLevels) != 3 || capModel.DefaultReasoningLevel != "medium" {
		t.Fatalf("cap-model reasoning metadata = %+v", capModel)
	}
	disabled := byID["disabled-reasoning"]
	if !disabled.ReasoningKnown || len(disabled.ReasoningLevels) != 0 || disabled.DefaultReasoningLevel != "" {
		t.Fatalf("disabled-reasoning metadata = %+v", disabled)
	}
}

func TestNormalizeOpenAIBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"  ", ""},
		{"https://www.yjmaas-api.10010.com/openapi/compatible-mode/v1/chat/completions", "https://www.yjmaas-api.10010.com/openapi/compatible-mode/v1"},
		{"https://example.com/api/v1/responses/", "https://example.com/api/v1"},
		{"https://example.com/api/v1/models", "https://example.com/api/v1"},
		{"https://example.com/codex", "https://example.com/codex"},
	}
	for _, tc := range tests {
		if got := protocol.NormalizeOpenAIBaseURL(tc.in); got != tc.want {
			t.Errorf("NormalizeOpenAIBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVerifyOpenAIGatewayModelContext(t *testing.T) {
	t.Run("accepts 200 chat completion and strips chat/completions suffix", func(t *testing.T) {
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			if r.Header.Get("Authorization") != "Bearer sk-good" {
				http.Error(w, `{"code":16,"msg":"unauthorized"}`, http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"}}]}`))
		}))
		t.Cleanup(server.Close)

		status, err := protocol.VerifyOpenAIGatewayModelContext(context.Background(), server.URL+"/openapi/compatible-mode/v1/chat/completions", "sk-good", "minimax-m2.5", "openai", 2*time.Second)
		if err != nil || status != http.StatusOK {
			t.Fatalf("status=%d err=%v, want 200 nil", status, err)
		}
		if gotPath != "/openapi/compatible-mode/v1/chat/completions" {
			t.Fatalf("path = %q, want /openapi/compatible-mode/v1/chat/completions", gotPath)
		}
	})

	t.Run("accepts 429 JSON rate or quota response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":5006,"msg":"quota exhausted"}`))
		}))
		t.Cleanup(server.Close)

		status, err := protocol.VerifyOpenAIGatewayModelContext(context.Background(), server.URL+"/v1", "sk-good", "deepseek-v3", "openai", 2*time.Second)
		if err != nil || status != http.StatusTooManyRequests {
			t.Fatalf("status=%d err=%v, want 429 nil", status, err)
		}
	})

	t.Run("rejects 403 auth failure and 200 JSON auth envelope", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"http_403", http.StatusForbidden, `{"code":16,"msg":"unauthorized"}`},
			{"envelope_200", http.StatusOK, `{"code":16,"msg":"unauthorized"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				t.Cleanup(server.Close)

				_, err := protocol.VerifyOpenAIGatewayModelContext(context.Background(), server.URL+"/v1", "sk-bad", "minimax-m2.5", "openai", 2*time.Second)
				if err == nil {
					t.Fatal("expected auth failure error, got nil")
				}
			})
		}
	})

	t.Run("rejects 404, 400 model_not_found, and 200 HTML fallback pages", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   string
		}{
			{"http_404", http.StatusNotFound, ``},
			{"model_not_found_400", http.StatusBadRequest, `{"error":{"code":"model_not_found","message":"The model bogus does not exist"}}`},
			{"spa_html_200", http.StatusOK, `<html><body>app</body></html>`},
			{"unrelated_json_200", http.StatusOK, `{"ok":true}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				t.Cleanup(server.Close)

				_, err := protocol.VerifyOpenAIGatewayModelContext(context.Background(), server.URL+"/v1", "sk-good", "bogus", "openai", 2*time.Second)
				if err == nil {
					t.Fatalf("%s: expected probe rejection, got nil", tc.name)
				}
			})
		}
	})
}
