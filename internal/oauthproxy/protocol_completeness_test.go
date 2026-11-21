package oauthproxy

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStreamsRequireTerminalEvent(t *testing.T) {
	cases := []struct {
		name, partial, terminal string
		process                 func(io.Reader, *responseAssembler) error
	}{
		{"responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n", processCodexResponsesStream},
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", processChatCompletionsStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []string{"", tc.partial} {
				a := newResponseAssembler(&adapterRequest{}, nil)
				err := tc.process(strings.NewReader(body), a)
				if !errors.Is(err, io.ErrUnexpectedEOF) || a.finished {
					t.Fatalf("truncated stream: err=%v finished=%v", err, a.finished)
				}
			}
			a := newResponseAssembler(&adapterRequest{}, nil)
			if err := tc.process(strings.NewReader(tc.partial+tc.terminal), a); err != nil || !a.finished {
				t.Fatalf("complete stream: err=%v finished=%v", err, a.finished)
			}
		})
	}
}

func TestResponsesDoesNotReplayOtherAdapterSignatures(t *testing.T) {
	for _, sig := range []string{"kiro", "gemini", "qoder", "gbx-openai-chat-signature-unavailable", "gbx-anthropic-signature-unavailable", "gbx-gemini-part-v1:abc", "legacy+/="} {
		turn := conversationTurn{
			Role: "assistant",
			Parts: []conversationPart{
				{Kind: partReasoning, Text: "x", Signature: sig},
				{Kind: partText, Text: "answer"},
			},
		}
		items, err := codexTurnItems(turn)
		if err != nil || len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
			t.Fatalf("foreign signature %q leaked: %v %v", sig, items, err)
		}
	}
	malformedTurn := conversationTurn{
		Role: "assistant",
		Parts: []conversationPart{
			{Kind: partReasoning, Text: "x", Signature: codexReasoningSignaturePrefix + "broken"},
		},
	}
	if _, err := codexTurnItems(malformedTurn); err == nil {
		t.Fatal("own malformed envelope must reach validation, not be silently dropped")
	}
}

func TestResponsesMultipleOutputMessagesAndParts(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"output":[{"id":"m1","type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":" extra"}]},{"id":"m2","type":"message","content":[{"type":"output_text","text":" second"}]}]}}`
	for _, prefix := range []string{
		"",
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"first\"}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"content_index\":0,\"delta\":\"first\"}\n\n",
	} {
		a := newResponseAssembler(&adapterRequest{}, nil)
		if err := processCodexResponsesStream(strings.NewReader(prefix+"data: "+terminal+"\n\n"), a); err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, b := range a.contentBlocks() {
			text.WriteString(b.Text)
		}
		if text.String() != "first extra second" {
			t.Fatalf("lost or duplicated text: %q", text.String())
		}
	}
}

func TestChatTemperaturePresence(t *testing.T) {
	for _, value := range []string{"", `,"temperature":0`, `,"temperature":0.7`} {
		req, err := convertResponsesToChatCompletions([]byte(`{"model":"test","input":[{"role":"user","content":"hello"}]` + value + `}`))
		if err != nil {
			t.Fatal(err)
		}
		got := gjson.GetBytes(req.body, "temperature")
		if got.Exists() != (value != "") {
			t.Fatalf("temperature presence changed: %s", req.body)
		}
		if value != "" && got.Float() != gjson.Get(`{`+strings.TrimPrefix(value, ",")+`}`, "temperature").Float() {
			t.Fatalf("temperature value changed: %s", req.body)
		}
	}
}
