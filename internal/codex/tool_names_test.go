package codex

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexMalformedToolNameDoesNotExposeReasoning(t *testing.T) {
	const malformed = "exec_f4d... wait typo private-reasoning <｜end▁of▁thinking｜>\n<｜DSML｜calls>"
	g := fixtureGateway(t, func(w http.ResponseWriter, r *http.Request) {
		writeChatTool(w, r, "bad-call", malformed, map[string]any{"input": "private-arguments"})
	}, "openai", Options{})
	response := postResponses(t, g, map[string]any{
		"model": "gbx/chosen/model-a", "stream": true, "input": "fixture",
		"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
	})
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "payload omitted") {
		t.Fatalf("missing malformed-call error: status=%d body=%s", response.StatusCode, raw)
	}
	for _, unwanted := range []string{"private-reasoning", "private-arguments", "DSML", "response.completed", "response.output_item.added"} {
		if strings.Contains(string(raw), unwanted) {
			t.Fatalf("malformed payload leaked or executed: %s", raw)
		}
	}
}
