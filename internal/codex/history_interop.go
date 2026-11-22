package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/grokbuildx/gbx/internal/protocol"
	"github.com/tidwall/gjson"
)

const nativeOpenAIReasoningScope = "openai-native"

// nativeHistory removes adapter-only state and foreign-backend encrypted
// reasoning before a native OpenAI Responses request. Plain reasoning summaries
// cannot reference OpenAI state when store is false. Native encrypted reasoning
// and all conversation/tool items remain intact.
func nativeHistory(body []byte) []byte {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(request["input"], &items) != nil {
		return body
	}
	var store bool
	stateless := json.Unmarshal(request["store"], &store) == nil && !store
	changed := false
	result := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		var kind, id, encrypted string
		_ = json.Unmarshal(item["type"], &kind)
		_ = json.Unmarshal(item["id"], &id)
		_ = json.Unmarshal(item["encrypted_content"], &encrypted)
		if kind == "reasoning" && (strings.HasPrefix(id, "rs_gbx_") ||
			(encrypted == "" && stateless) ||
			protocol.IsGBXManagedSignature(encrypted) ||
			(encrypted != "" && !protocol.ReasoningScopeMatches(encrypted, nativeOpenAIReasoningScope))) {
			changed = true
			continue
		}
		if kind == "compaction" {
			if summary, ok := decodeCompactionSummary(encrypted); ok {
				raw, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Conversation summary:\n" + summary}}})
				item = make(map[string]json.RawMessage)
				_ = json.Unmarshal(raw, &item)
				changed = true
			}
		}
		result = append(result, item)
	}
	if !changed {
		return body
	}
	request["input"], _ = json.Marshal(result)
	raw, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return raw
}

// nativeReasoningObserverReader records encrypted_content produced by native
// OpenAI Responses streams/bodies under nativeOpenAIReasoningScope so switching
// from native OpenAI to xAI or third-party passthrough drops OpenAI ciphertext.
type nativeReasoningObserverReader struct {
	inner   io.ReadCloser
	pending []byte
}

func (r *nativeReasoningObserverReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.observe(p[:n])
	}
	if err == io.EOF && len(r.pending) > 0 {
		r.scanEvent(r.pending)
		r.pending = nil
	}
	return n, err
}

func (r *nativeReasoningObserverReader) Close() error {
	if len(r.pending) > 0 {
		r.scanEvent(r.pending)
		r.pending = nil
	}
	return r.inner.Close()
}

func (r *nativeReasoningObserverReader) observe(chunk []byte) {
	const maxPending = 4 << 20
	r.pending = append(r.pending, chunk...)
	for {
		idx := bytes.Index(r.pending, []byte("\n\n"))
		sep := 2
		if crlf := bytes.Index(r.pending, []byte("\r\n\r\n")); crlf >= 0 && (idx < 0 || crlf < idx) {
			idx = crlf
			sep = 4
		}
		if idx < 0 {
			break
		}
		r.scanEvent(r.pending[:idx])
		r.pending = r.pending[idx+sep:]
	}
	if len(r.pending) > maxPending {
		r.pending = nil
	}
}

func (r *nativeReasoningObserverReader) scanEvent(raw []byte) {
	if !bytes.Contains(raw, []byte(`"encrypted_content"`)) {
		return
	}
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimSpace(value)...)
		}
	}
	if len(data) == 0 {
		data = bytes.TrimSpace(raw)
	}
	if item := gjson.GetBytes(data, "item"); item.IsObject() && item.Get("type").String() == "reasoning" {
		if enc := strings.TrimSpace(item.Get("encrypted_content").String()); enc != "" {
			protocol.RecordReasoningScope(enc, nativeOpenAIReasoningScope)
		}
	}
	outputs := gjson.GetBytes(data, "response.output")
	if !outputs.IsArray() {
		outputs = gjson.GetBytes(data, "output")
	}
	if outputs.IsArray() {
		for _, item := range outputs.Array() {
			if item.Get("type").String() == "reasoning" {
				if enc := strings.TrimSpace(item.Get("encrypted_content").String()); enc != "" {
					protocol.RecordReasoningScope(enc, nativeOpenAIReasoningScope)
				}
			}
		}
	}
}
