package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tui "github.com/grindlemire/go-tui"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/protocol"
)

func TestProtocolDetectionRejectsFallbackPages(t *testing.T) {
	for _, body := range []string{"", "<html>app</html>", `{"ok":true}`, `{"data":[]}`, `{"name":"API gateway"}`, `{"id":"site-id"}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("protocol detection made a mutating request")
				}
				w.Write([]byte(body))
			}))
			defer s.Close()
			result := detectProtocolAndModelsDetailed(s.URL, "fake-key")
			if result.err == nil {
				t.Fatalf("generic fallback page detected: %+v", result)
			}
		})
	}
}

func TestAutoClawProviderIsNotUserSelectable(t *testing.T) {
	// An AutoClaw provider is created by gbx oauth/import and connects straight
	// to its fixed remote endpoint, so the manual protocol selector must not
	// offer to reinterpret it.
	p := providerFrom("autoclaw", "https://autoglm-api.autoglm.ai/autoclaw-proxy/proxy/autoclaw", "autoclaw")
	p.OAuthProvider = "autoclaw"
	m := NewAdvancedConfigModel(&p)
	if m.canSelectCustomProtocol() {
		t.Fatal("AutoClaw provider unexpectedly has a manual protocol selector")
	}
	if got := m.getProtocol(); got != "openai-chat / autoclaw" {
		t.Fatalf("protocol row = %q", got)
	}
	if got := m.getProtocolFamily(); got != "OpenAI" {
		t.Fatalf("protocol family = %q", got)
	}
}

// `gbx oauth autoclaw` persists the managed catalog into the provider, and a
// subscription has no probe that would fill the pool later, so the page has to
// open on the persisted pool. Skipping it for OAuth left the list empty.
func TestOAuthProviderOpensWithItsPersistedModelPool(t *testing.T) {
	catalog := oauthproxy.AutoClawModelIDs()
	p := providerFrom("autoclaw", "https://example.invalid/autoclaw-proxy/proxy/autoclaw", "autoclaw")
	p.OAuthProvider = "autoclaw"
	p.Model = strings.Join(catalog, ",")

	m := readyOAuthPage(NewAdvancedConfigModel(&p))
	if len(m.live().modelPool) != len(catalog) {
		t.Fatalf("model pool = %v, want the persisted AutoClaw catalog", m.live().modelPool)
	}

	m.width = 160
	view := renderView(t, m)
	for _, want := range []string{catalog[0], catalog[1], "(+2 more)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("models summary missing %q: %q", want, view)
		}
	}
	for _, want := range catalog {
		if label := m.modelDisplayLabel(want); strings.Contains(label, "(zai_auto)") {
			t.Fatalf("model display label renders the display alias: %q", label)
		}
	}
	if strings.Contains(view, "(zai_auto)") {
		t.Fatalf("models view still renders the display alias: %q", view)
	}
}

// Re-probing a provider on open must not invent [1m] markers on unmarked pool
// models, while preserving any [1m] suffixes the user explicitly configured.
func TestReopeningTheProviderPageKeepsTheOneMOptOut(t *testing.T) {
	p := providerFrom("sharedchat", "https://example.invalid/v1", "openai")
	p.Model = "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna"

	m := NewAdvancedConfigModel(&p)
	m.applyModelDetectionResult("openai", p.Model, m.live().probeEndpoint, nil)
	if len(oneMModelsFromProvider(*m.p)) != 0 {
		t.Fatalf("opening the page enabled 1M on unmarked models: %q", m.p.Model)
	}

	// A model explicitly marked [1m] in the pool preserves its marker across re-detection.
	m.p.Model = "gpt-5.6-sol[1m],gpt-5.6-terra,gpt-5.6-luna"
	m.applyModelDetectionResult("openai", "gpt-5.6-sol,gpt-5.6-terra,gpt-5.6-luna", m.live().probeEndpoint, nil)
	marked := oneMModelsFromProvider(*m.p)
	if !slices.Equal(marked, []string{"gpt-5.6-sol"}) {
		t.Fatalf("re-detection did not preserve explicit [1m] markers: %q (marked=%v)", m.p.Model, marked)
	}
}

// The panel opens before the subscription's runtime is up, so the Local Proxy
// row has to carry the wait: a spinner while the runtime starts, then the ready
// line. Starting it synchronously is what used to leave the terminal blank.
func TestOAuthLocalProxyRowTracksTheBackgroundRuntimeStart(t *testing.T) {
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider = "qoder"
	p.Model = "dmodel"
	m := NewAdvancedConfigModel(&p)

	m.runtimeLoading = true
	view := renderView(t, m)
	label := locale.T("本地代理", "Local Proxy")
	starting := locale.T("启动中…", "Starting...")
	if !strings.Contains(view, label) || !strings.Contains(view, starting) {
		t.Fatalf("loading Local Proxy row = %q", view)
	}
	if fixture := view; !strings.Contains(fixture, spinnerAt(m.runtimeFrame)) {
		t.Fatalf("loading row has no spinner frame: %q", fixture)
	}
	// The page must not be editable, nor saveable, while the catalog is pending.
	if m.connectionReady() || m.canSave() {
		t.Fatalf("page editable before the runtime answered: ready=%t save=%t", m.connectionReady(), m.canSave())
	}

	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint: "http://127.0.0.1:1234/v1",
		apiKey:   "session-key",
		models:   []string{"dmodel"},
		names:    map[string]string{"dmodel": "Runtime name"},
	})
	if m.runtimeLoading || !m.runtimeReady || m.runtimeErr != nil {
		t.Fatalf("runtime result not applied: loading=%t ready=%t err=%v", m.runtimeLoading, m.runtimeReady, m.runtimeErr)
	}
	if !m.connectionReady() || !m.canSave() {
		t.Fatalf("page still locked after the runtime came up: ready=%t save=%t", m.connectionReady(), m.canSave())
	}
	if !strings.Contains(m.modelDisplayLabel("dmodel"), "Runtime name") {
		t.Fatalf("runtime catalog names were dropped: %q", m.modelDisplayLabel("dmodel"))
	}
	if view := renderView(t, m); !strings.Contains(view, locale.T("已就绪（仅本次会话）", "Ready (this session only)")) {
		t.Fatalf("Local Proxy row did not switch to ready: %q", view)
	}

	// A failed start is reported and keeps the page unsaveable rather than
	// silently persisting a half-configured subscription.
	failed := readyOAuthPage(NewAdvancedConfigModel(&p))
	failed.runtimeLoading = true
	failed.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{err: errors.New("no qoder credentials found")})
	if failed.runtimeReady || failed.canSave() {
		t.Fatalf("failed start left the page saveable: ready=%t save=%t", failed.runtimeReady, failed.canSave())
	}
	if view := renderView(t, failed); !strings.Contains(view, locale.T("启动失败（仅本次会话）", "Start failed (this session only)")) {
		t.Fatalf("failure not surfaced on the Local Proxy row: %q", view)
	}
}

// The whole point of the change: the start runs on its own goroutine, reports
// back through the channel, and teardown does not hang on it. A credential-less
// qoder fails fast, which is what makes this hermetic.
func TestBeginOAuthRuntimeReportsBackAndTearsDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider = "qoder"
	m := NewAdvancedConfigModel(&p)
	m.beginOAuthRuntime(p)
	if !m.runtimeLoading {
		t.Fatal("the start did not register as in flight")
	}

	var msg oauthRuntimeDoneMsg
	select {
	case msg = <-m.oauthDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime start never reported back")
	}
	m.handleOAuthRuntimeDone(msg)
	if m.runtimeLoading {
		t.Fatal("the result did not clear the loading state")
	}
	if m.runtimeReady || m.runtimeErr == nil {
		t.Fatalf("a credential-less start reported ready: ready=%t err=%v", m.runtimeReady, m.runtimeErr)
	}

	// stopOAuthRuntime is deferred in RunProviderSet; it must return even when
	// the start already finished.
	done := make(chan struct{})
	go func() { m.stopOAuthRuntime(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stopOAuthRuntime did not return")
	}
	if m.oauthCancel == nil {
		t.Fatal("beginOAuthRuntime did not install a cancel func")
	}
}

// Qoder against a credential it cannot refresh: discovery fails and the backend
// substitutes a five-model compatibility list. Adopting that as the account's
// catalog is what replaced a saved 17-model pool with 5, and saving the page made
// it permanent. A fallback must not overwrite what is already there.
func TestFallbackCatalogDoesNotReplaceTheSavedModelPool(t *testing.T) {
	saved := []string{
		"auto", "ultimate", "performance", "efficient", "lite", "cmodel",
		"qmodel_38max", "qfmodel", "qmodel_latest", "qmodel", "kmodel_latest",
		"kmodel", "gmodel", "gfmodel", "dmodel", "dfmodel", "mmodel",
	}
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider, p.Model = "qoder", strings.Join(saved, ",")
	m := NewAdvancedConfigModel(&p)
	before := append([]string(nil), m.live().modelPool...)
	if len(before) != len(saved) {
		t.Fatalf("page did not open on the saved pool: %v", before)
	}

	m.runtimeLoading = true
	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint:        "http://127.0.0.1:1/v1",
		apiKey:          "session-key",
		models:          []string{"auto", "ultimate", "performance", "efficient", "lite"},
		catalogFallback: true,
	})

	if !m.runtimeReady || m.runtimeErr != nil {
		t.Fatalf("fallback runtime did not come up: ready=%t err=%v", m.runtimeReady, m.runtimeErr)
	}
	if !m.runtimeCatalogFallback {
		t.Fatal("fallback catalog was not recorded")
	}
	if !slices.Equal(m.live().modelPool, before) {
		t.Fatalf("fallback replaced the saved pool: %v", m.live().modelPool)
	}
	if p.Model != strings.Join(saved, ",") {
		t.Fatalf("provider catalog shrank to %q", p.Model)
	}
	// The page is still usable — the runtime is up, only its catalog is a guess.
	if !m.canSave() {
		t.Fatal("page unsaveable after the runtime came up")
	}
	view := renderView(t, m)
	if !strings.Contains(view, locale.T("已就绪 · 模型列表获取失败，沿用已保存的", "Ready · model list unavailable; saved models")) {
		t.Fatalf("fallback catalog was not surfaced: %q", view)
	}

	// A real catalog still lands: the fallback is a state, not a latch.
	m.handleOAuthRuntimeDone(oauthRuntimeDoneMsg{
		endpoint: "http://127.0.0.1:1/v1",
		apiKey:   "session-key",
		models:   []string{"auto", "ultimate", "mmodel"},
	})
	if m.runtimeCatalogFallback {
		t.Fatal("fallback state survived a successful catalog fetch")
	}
	// The page keeps its pool sorted, so compare against a sorted copy.
	if want := []string{"auto", "ultimate", "mmodel"}; !slices.Equal(m.live().modelPool, slices.Sorted(slices.Values(want))) {
		t.Fatalf("real catalog not adopted: %v", m.live().modelPool)
	}
}

func TestOAuthConfigShowsQoderRuntimeNameAndModelID(t *testing.T) {
	p := providerFrom("qoder", "https://example.invalid", "anthropic")
	p.OAuthProvider = "qoder"
	p.Model = "dmodel"
	m := NewAdvancedConfigModel(&p)
	m.setRuntimeModelNames(map[string]string{"dmodel": "Catalog display name"})
	if label := m.modelDisplayLabel("dmodel"); label != "Catalog display name (dmodel)" {
		t.Fatalf("row label = %q, want the catalog name and model ID", label)
	}
	if m.p.Model != "dmodel" {
		t.Fatal("display label replaced upstream ID")
	}
}

func TestOAuthRuntimeNamesSurviveCatalogRefresh(t *testing.T) {
	p := providerFrom("qoder", "oauth://qoder", "anthropic")
	p.OAuthProvider, p.Model = "qoder", "dmodel"
	m := NewAdvancedConfigModel(&p)
	m.configureOAuthRuntime("http://127.0.0.1:12345", "fake-local-key", nil, false)
	m.setRuntimeModelNames(map[string]string{"dmodel": "Runtime name"})
	for _, name := range []string{"", "Fresh catalog name"} {
		m.live().detecting = true
		m.handleFetchDone(modelFetchDoneMsg{endpoint: m.live().probeEndpoint, apiKey: m.live().probeAPIKey, discoveredModelsRaw: "dmodel", modelInfos: []protocol.ModelInfo{{ID: "dmodel", DisplayName: name, ContextWindow: 128000}}})
		want := name
		if want == "" {
			want = "Runtime name"
		}
		if got := m.modelDisplayLabel("dmodel"); got != want+" (dmodel)" {
			t.Fatalf("row label = %q, want the refreshed catalog name and model ID", got)
		}
		if p.Model != "dmodel" || p.Endpoint != "oauth://qoder" {
			t.Fatal("display refresh mutated stored pool or endpoint")
		}
		if m.live().modelContextWindows["dmodel"] != 128000 {
			t.Fatal("context metadata lost")
		}
	}
}

func TestProviderSelectorLabelUsesEditableColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	for _, selected := range []bool{false, true} {
		got := tui.Sprint(stepperRow("Provider", "‹ Select provider ›", selected), tui.WithPrintWidth(80))
		label := strings.Index(got, "Provider")
		color := "38;2;183;156;255m"
		if selected {
			color = "38;2;101;183;255m"
		}
		if label < 0 || !strings.Contains(got[:label], color) {
			t.Fatalf("editable color missing before label: %q", got)
		}
	}
}

// TestCustomGatewayWithoutModelsEndpointCanConfigureAndReopen covers gateways
// such as China Unicom Yuanjing MaaS (/openapi/compatible-mode/v1/chat/completions)
// that serve Chat Completions without exposing GET /models (returning 404).
func TestCustomGatewayWithoutModelsEndpointCanConfigureAndReopen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi/compatible-mode/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/openapi/compatible-mode/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-valid" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":16,"msg":"auth failed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	fullChatURL := server.URL + "/openapi/compatible-mode/v1/chat/completions"
	wantBaseURL := server.URL + "/openapi/compatible-mode/v1"

	p := providerFrom("yjmaas", "", "")
	p.Model = ""
	p.APIKey = ""
	m := NewAdvancedConfigModel(&p)
	m.urlText.Set(fullChatURL)
	m.keyText.Set("sk-valid")

	// 1. Running Auto Configure before entering models sees HTTP 404 on /models,
	// focuses rowModelPool automatically, and renders the manual-model hint.
	m.activateRow(rowTest)
	select {
	case msg := <-m.fetchDone:
		m.handleFetchDone(msg)
	case <-time.After(3 * time.Second):
		t.Fatal("Auto Configure without models timed out")
	}
	if m.canSave() {
		t.Fatal("gateway with 404 /models and empty pool must not be saveable")
	}
	if m.currentRow() != rowModelPool {
		t.Fatalf("cursor after 404 /models = %v, want rowModelPool (%v)", m.currentRow(), rowModelPool)
	}
	view := renderView(t, m)
	if !strings.Contains(view, "/models") {
		t.Fatalf("missing /models guidance in view:\n%s", view)
	}

	// 2. Typing manual model IDs (including [1m] and letters like q/k/j/h/l) and
	// pressing Enter verifies against POST /chat/completions and unlocks Save.
	for _, r := range "minimax-m2.5, qwen3-32b[1m]" {
		m.handleKey(keyPress(r))
	}
	if m.quitRequested {
		t.Fatal("typing 'q' inside Model Pool quit the TUI")
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyEnter})
	select {
	case msg := <-m.fetchDone:
		m.handleFetchDone(msg)
	case <-time.After(3 * time.Second):
		t.Fatal("manual model verification timed out")
	}
	if m.live().detectionError != nil || !m.canSave() {
		t.Fatalf("manual model verification failed: canSave=%t err=%v", m.canSave(), m.live().detectionError)
	}
	if m.p.Endpoint != wantBaseURL {
		t.Fatalf("normalized endpoint = %q, want %q", m.p.Endpoint, wantBaseURL)
	}
	if m.p.Type != "openai" {
		t.Fatalf("detected type = %q, want openai", m.p.Type)
	}
	if m.p.Model != "minimax-m2.5,qwen3-32b[1m]" {
		t.Fatalf("persisted Model = %q, want minimax-m2.5,qwen3-32b[1m]", m.p.Model)
	}

	// 3. Editing the model list on a /models-less gateway marks connection dirty
	// until re-verified.
	m.cursor = m.mainRowIndex(rowModelPool)
	m.handleKey(keyPress('x'))
	if !m.live().connectionDirty || m.canSave() {
		t.Fatalf("editing unverified model IDs must mark dirty: dirty=%t canSave=%t", m.live().connectionDirty, m.canSave())
	}
	m.handleKey(tui.KeyEvent{Key: tui.KeyBackspace})
	if m.live().connectionDirty || !m.canSave() {
		t.Fatalf("reverting model edit must restore saveable state: dirty=%t canSave=%t", m.live().connectionDirty, m.canSave())
	}

	// 4. An invalid API key is rejected even when manual models are supplied,
	// without wiping the typed model IDs.
	m.keyText.Set("sk-wrong")
	m.refreshConnectionDirty()
	m.activateRow(rowTest)
	select {
	case msg := <-m.fetchDone:
		m.handleFetchDone(msg)
	case <-time.After(3 * time.Second):
		t.Fatal("invalid key probe timed out")
	}
	if m.canSave() || m.live().detectionError == nil {
		t.Fatalf("invalid key unexpectedly passed: canSave=%t err=%v", m.canSave(), m.live().detectionError)
	}
	if m.modelsText.Get() != "minimax-m2.5,qwen3-32b[1m]" {
		t.Fatalf("failed probe wiped user-typed models: %q", m.modelsText.Get())
	}

	// 5. Reopening a saved Custom provider whose gateway has no /models verifies
	// the saved pool automatically instead of wiping it.
	savedProvider := *m.p
	savedProvider.Endpoint = wantBaseURL
	savedProvider.APIKey = "sk-valid"
	savedProvider.Model = "minimax-m2.5,qwen3-32b[1m]"
	reopened := NewAdvancedConfigModel(&savedProvider)
	reopened.startAutoDetect()
	select {
	case msg := <-reopened.fetchDone:
		reopened.handleFetchDone(msg)
	case <-time.After(3 * time.Second):
		t.Fatal("reopen auto-detect timed out")
	}
	if !reopened.canSave() || reopened.p.Model != "minimax-m2.5,qwen3-32b[1m]" {
		t.Fatalf("reopened provider lost models or save state: canSave=%t model=%q err=%v",
			reopened.canSave(), reopened.p.Model, reopened.live().detectionError)
	}
}
