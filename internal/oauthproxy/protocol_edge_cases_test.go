package oauthproxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResponsesReasoningTracksEverySummaryPart(t *testing.T) {
	for _, mode := range []string{"done-only", "mixed", "terminal-only"} {
		t.Run(mode, func(t *testing.T) {
			var stream strings.Builder
			emit := func(event map[string]any) { raw, _ := json.Marshal(event); fmt.Fprintf(&stream, "data: %s\n\n", raw) }
			item := map[string]any{"type": "reasoning", "id": "r1", "summary": []any{map[string]any{"type": "summary_text", "text": "first"}, map[string]any{"type": "summary_text", "text": "second"}}, "encrypted_content": "opaque"}
			if mode != "terminal-only" {
				emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "r1"}})
				if mode == "mixed" {
					for _, delta := range []string{"fi", "rst"} {
						emit(map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "r1", "summary_index": 0, "delta": delta})
					}
				}
				for index, text := range []string{"first", "second"} {
					emit(map[string]any{"type": "response.reasoning_summary_text.done", "item_id": "r1", "summary_index": index, "text": text})
					emit(map[string]any{"type": "response.reasoning_summary_part.done", "item_id": "r1", "summary_index": index, "part": map[string]any{"text": text}})
				}
				emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			}
			emit(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{item, map[string]any{"type": "reasoning", "id": "r2", "summary": []any{map[string]any{"type": "summary_text", "text": "third"}}}}}})
			a := newResponseAssembler(&adapterRequest{}, nil)
			if err := processCodexResponsesStream(strings.NewReader(stream.String()), a); err != nil {
				t.Fatal(err)
			}
			blocks := a.contentBlocks()
			if len(blocks) != 2 || blocks[0].Thinking != "first\n\nsecond" || blocks[1].Thinking != "third" {
				t.Fatalf("missing/duplicated summaries: %+v", blocks)
			}
			if blocks[0].Signature == "" || blocks[1].Signature == "" {
				t.Fatal("reasoning signatures missing")
			}
		})
	}
}
