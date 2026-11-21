package modelsdev

import (
	"os"
	"slices"
	"testing"
)

func TestReasoningIndexResolvesProviderCanonicalAndReferenceEntries(t *testing.T) {
	index, err := newIndex([]byte(`{
		"owner":{"models":{"formal-id":{"id":"formal-id","name":"Shared Name","canonical_model_id":"owner/shared","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}]}}},
		"gateway":{"models":{"shared":{"id":"shared","canonical_model_id":"owner/shared","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high"]}]}}},
		"opencode-go":{"models":{"reference":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["high","max"]}]}}},
		"opencode":{"models":{"reference":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}]}}},
		"ambiguous-a":{"models":{"conflict":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["low"]}]}}},
		"ambiguous-b":{"models":{"conflict":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["high"]}]}}},
		"controls":{"models":{
			"toggle":{"reasoning":true,"reasoning_options":[{"type":"toggle"}]},
			"budget":{"reasoning":true,"reasoning_options":[{"type":"budget_tokens"}]},
			"plain":{"reasoning":false},
			"ordered":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["max","high","high","unknown","none"," LOW "]}]},
			"retired":{"status":"deprecated","reasoning":true,"reasoning_options":[{"type":"effort","values":["high"]}]}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		provider, id, name string
		efforts            []string
		found              bool
	}{
		{"gateway", "shared", "", []string{"low", "medium", "high"}, true},
		{"", "owner/shared", "", []string{"low", "high", "max"}, true},
		{"", "shared", "", []string{"low", "high", "max"}, true},
		{"", "unknown-revision", "Shared Name", []string{"low", "high", "max"}, true},
		{"", " FORMAL-ID[1m] ", "", []string{"low", "high", "max"}, true},
		{"", "reference", "", []string{"high", "max"}, true},
		{"", "conflict", "", nil, false},
		{"ambiguous-a", "conflict", "", []string{"low"}, true},
		{"", "toggle", "", nil, true},
		{"", "budget", "", nil, true},
		{"", "plain", "", nil, true},
		{"", "ordered", "", []string{"low", "high", "max"}, true},
		{"", "retired", "", []string{"high"}, true},
		{"", "shared-v2", "", nil, false},
		{"", "owner/shared:free", "", []string{"low", "high", "max"}, true},
		{"", "shared:thinking", "", []string{"low", "high", "max"}, true},
		{"", "shared-thinking", "", []string{"low", "high", "max"}, true},
		{"", "azure/shared", "", []string{"low", "high", "max"}, true},
		{"", "custom-gw/plain:free", "", nil, true},
	} {
		efforts, found := index.Efforts(test.provider, test.id, test.name)
		if found != test.found || !slices.Equal(efforts, test.efforts) {
			t.Errorf("%s/%s (%s): %v found=%t, want %v found=%t", test.provider, test.id, test.name, efforts, found, test.efforts, test.found)
		}
	}

	// Digit separator normalization (4.6 <-> 4-6) resolves without fuzzy version stripping.
	sepIndex, err := newIndex([]byte(`{
		"anthropic":{"models":{"claude-opus-4-6":{"id":"claude-opus-4-6","canonical_model_id":"anthropic/claude-opus-4-6","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","max"]}]}}},
		"openai":{"models":{"gpt-5.4":{"id":"gpt-5.4","canonical_model_id":"openai/gpt-5.4","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh"]}]}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if efforts, found := sepIndex.Efforts("", "anthropic/claude-opus-4.6:free", ""); !found || !slices.Equal(efforts, []string{"low", "medium", "high", "max"}) {
		t.Fatalf("claude-opus-4.6 dotted variant = %v found=%t", efforts, found)
	}
	if efforts, found := sepIndex.Efforts("", "gpt-5-4", ""); !found || !slices.Equal(efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("gpt-5-4 hyphen variant = %v found=%t", efforts, found)
	}
	if _, found := sepIndex.Efforts("", "claude-opus-4-7", ""); found {
		t.Fatal("must not fuzzy-strip version numbers")
	}

	// SupportsThinking distinguishes budget/toggle thinking models from non-reasoning models.
	if supports, ok := index.SupportsThinking("", "budget", ""); !ok || !supports {
		t.Fatalf("SupportsThinking(budget) = (%t, %t), want (true, true)", supports, ok)
	}
	if supports, ok := index.SupportsThinking("", "toggle", ""); !ok || !supports {
		t.Fatalf("SupportsThinking(toggle) = (%t, %t), want (true, true)", supports, ok)
	}
	if supports, ok := index.SupportsThinking("", "plain", ""); !ok || supports {
		t.Fatalf("SupportsThinking(plain) = (%t, %t), want (false, true)", supports, ok)
	}
	if _, ok := index.SupportsThinking("", "nonexistent", ""); ok {
		t.Fatal("SupportsThinking(nonexistent) should be unknown")
	}
}

func TestOfflineReasoningSnapshot(t *testing.T) {
	index, err := newIndex(snapshotBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id      string
		efforts []string
	}{
		{"deepseek-v4.1-flash", []string{"low", "high", "max"}},
		{"space-bunny", []string{"low", "medium", "high", "xhigh", "max"}},
		{"glm-5.3-flash", []string{"low", "high", "max"}},
		{"hy4-preview", []string{"high"}},
	} {
		efforts, found := index.Efforts("workbuddycn", test.id, "")
		if !found || !slices.Equal(efforts, test.efforts) {
			t.Errorf("%s: %v found=%t, want %v", test.id, efforts, found, test.efforts)
		}
		if len(efforts) > 0 {
			efforts[0] = "modified"
			fresh, _ := index.Efforts("workbuddycn", test.id, "")
			if !slices.Equal(fresh, test.efforts) {
				t.Error("caller mutated the shared index")
			}
		}
	}
}

func TestReasoningRejectsBrokenSnapshot(t *testing.T) {
	for _, data := range []string{`not json`, `{}`} {
		if _, err := newIndex([]byte(data)); err == nil {
			t.Fatalf("accepted unusable snapshot %q", data)
		}
	}
}

func TestIndexInputModalitiesAndMaxOutputTokens(t *testing.T) {
	catalog := `{
		"anthropic": {
			"id": "anthropic",
			"models": {
				"claude-sonnet-4-6": {
					"id": "claude-sonnet-4-6",
					"name": "Claude Sonnet 4.6",
					"modalities": {"input": ["TEXT", "IMAGE"], "output": ["text"]},
					"limit": {"context": 200000, "output": 64000}
				}
			}
		},
		"textonly": {
			"id": "textonly",
			"models": {
				"plain-coder": {
					"id": "plain-coder",
					"name": "Plain Coder",
					"modalities": {"input": ["text"], "output": ["text"]},
					"limit": {"context": 128000, "output": 16384}
				}
			}
		},
		"other": {
			"id": "other",
			"models": {
				"shared-name-a": {
					"id": "shared-name",
					"modalities": {"input": ["text", "image"]},
					"limit": {"output": 8192}
				},
				"shared-name-b": {
					"id": "shared-name",
					"modalities": {"input": ["text"]},
					"limit": {"output": 32768}
				}
			}
		}
	}`
	index, err := newIndex([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	mod, ok := index.InputModalities("anthropic", "claude-sonnet-4-6", "")
	if !ok || !slices.Equal(mod, []string{"text", "image"}) {
		t.Fatalf("claude modalities = %v ok=%t", mod, ok)
	}
	mod[0] = "mutated"
	fresh, _ := index.InputModalities("anthropic", "claude-sonnet-4-6", "")
	if !slices.Equal(fresh, []string{"text", "image"}) {
		t.Fatalf("caller mutated index modalities: %v", fresh)
	}
	maxOut, ok := index.MaxOutputTokens("anthropic", "claude-sonnet-4-6", "")
	if !ok || maxOut != 64000 {
		t.Fatalf("claude maxOutput = %d ok=%t", maxOut, ok)
	}
	plainMod, ok := index.InputModalities("textonly", "plain-coder", "")
	if !ok || !slices.Equal(plainMod, []string{"text"}) {
		t.Fatalf("plain-coder modalities = %v ok=%t", plainMod, ok)
	}
	if _, ok := index.InputModalities("anthropic", "completely-unknown-model", ""); ok {
		t.Fatal("invented modalities for unknown model")
	}
	if _, ok := index.MaxOutputTokens("anthropic", "completely-unknown-model", ""); ok {
		t.Fatal("invented max output tokens for unknown model")
	}
	if _, ok := index.InputModalities("other", "shared-name", ""); ok {
		t.Fatal("ambiguous modality alias should not guess")
	}
	if _, ok := index.MaxOutputTokens("other", "shared-name", ""); ok {
		t.Fatal("ambiguous maxOutput alias should not guess")
	}
}

func BenchmarkReasoningIndex(b *testing.B) {
	data := snapshotBytes(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := newIndex(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReasoningLookup(b *testing.B) {
	index, err := newIndex(snapshotBytes(b))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, found := index.Efforts("workbuddycn", "deepseek-v4.1-flash", ""); !found {
			b.Fatal("missing model")
		}
	}
}

func snapshotBytes(t testing.TB) []byte {
	t.Helper()
	data, err := os.ReadFile("modelsdev.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
