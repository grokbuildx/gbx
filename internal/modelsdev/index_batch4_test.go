package modelsdev

import (
	"slices"
	"testing"
)

func TestBatch4ThinkingSuffixDoesNotDowngradeToNonReasoningBase(t *testing.T) {
	raw := []byte(`{
		"vendor": {
			"models": {
				"base-chat": {
					"id": "base-chat",
					"name": "Base Chat",
					"reasoning": false,
					"modalities": {"input": ["text", "image"]},
					"limit": {"output": 16384}
				},
				"base-reasoner": {
					"id": "base-reasoner",
					"name": "Base Reasoner",
					"reasoning": true,
					"reasoning_options": [{"type": "effort", "values": ["low", "high"]}],
					"modalities": {"input": ["text"]},
					"limit": {"output": 32768}
				}
			}
		}
	}`)
	idx, err := newIndex(raw)
	if err != nil {
		t.Fatalf("newIndex: %v", err)
	}

	// Querying a :thinking or -thinking variant whose base model has reasoning:false
	// must NOT downgrade reasoning lookups to found=true/non-reasoning, while still
	// inheriting modalities and max output limits.
	for _, variant := range []string{"custom/base-chat:thinking", "custom/base-chat-thinking", "custom/base-chat:thinking:free"} {
		if efforts, ok := idx.Efforts("", variant, ""); ok {
			t.Fatalf("Efforts(%q) = (%v, true), want (_, false) when base is reasoning:false", variant, efforts)
		}
		if supports, ok := idx.SupportsThinking("", variant, ""); ok {
			t.Fatalf("SupportsThinking(%q) = (%v, true), want (false, false) when base is reasoning:false", variant, supports)
		}
		if _, _, ok := idx.ThinkingMode("", variant, ""); ok {
			t.Fatalf("ThinkingMode(%q) unexpectedly matched non-reasoning base", variant)
		}
		if mod, ok := idx.InputModalities("", variant, ""); !ok || !slices.Equal(mod, []string{"text", "image"}) {
			t.Fatalf("InputModalities(%q) = (%v, %v), want ([text image], true)", variant, mod, ok)
		}
		if maxOut, ok := idx.MaxOutputTokens("", variant, ""); !ok || maxOut != 16384 {
			t.Fatalf("MaxOutputTokens(%q) = (%d, %v), want (16384, true)", variant, maxOut, ok)
		}
	}

	// Chained gateway colon tags (:free:nitro) strip cleanly to match base-reasoner.
	efforts, ok := idx.Efforts("", "custom/base-reasoner:free:nitro", "")
	if !ok || !slices.Equal(efforts, []string{"low", "high"}) {
		t.Fatalf("chained colon tags Efforts = (%v, %v), want ([low high], true)", efforts, ok)
	}
}
