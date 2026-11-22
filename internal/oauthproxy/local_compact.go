package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	localCompactionMaxBodyBytes        = int64(128 << 20)
	defaultCompactionToolMaxBytes      = 24 << 10
	defaultCompactionMessageMaxBytes   = 32 << 10
	compactionRetryMidBudgetBytes      = 96 << 10
	compactionRetryMidItemMaxBytes     = 8 << 10
	compactionRetryTightBudgetBytes    = 28 << 10
	compactionRetryTightItemMaxBytes   = 3 << 10
	compactionTruncationMarker         = "\n...[truncated for compaction]...\n"
	compactionOmittedMiddlePlaceholder = "[earlier turns omitted during compaction to fit context window]"
)

const localCompactionSystemPrefix = "Summarize the conversation for another coding assistant to continue. Retain the user's goals, constraints, decisions, tool results, changed files, and outstanding tasks. Do not perform the tasks or call tools.\n"

const localCompactionPrompt = "Summarize the conversation above for another assistant to continue. Preserve the goals, constraints, decisions, tool results, changed files, and outstanding tasks. Return only the summary; do not continue the task or call tools."

// ExcerptCompactionText truncates an oversized string on UTF-8 rune boundaries
// while retaining both head and tail context for compaction summaries.
func ExcerptCompactionText(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	if maxBytes <= len(compactionTruncationMarker)+16 {
		return utf8PrefixBytes(text, maxBytes)
	}
	usable := maxBytes - len(compactionTruncationMarker)
	headLimit := usable / 2
	tailLimit := usable - headLimit
	head := utf8PrefixBytes(text, headLimit)
	tail := utf8SuffixBytes(text, tailLimit)
	return head + compactionTruncationMarker + tail
}

func utf8PrefixBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func utf8SuffixBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// IsCompactionContextOverflow reports whether an error response from a
// compaction summarization call failed because the input exceeded the model's
// context window.
func IsCompactionContextOverflow(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge && status != http.StatusUnprocessableEntity {
		return false
	}
	if status == http.StatusRequestEntityTooLarge {
		return true
	}
	norm := normalizeUpstreamError("openai_responses", status, string(body), "")
	return norm.Code == "context_length_exceeded"
}

// localCompactionHandler implements POST /v1/responses/compact locally for
// runtimes whose upstream does not expose an OpenAI /responses/compact endpoint
// (Chat Completions, Anthropic Messages, Kiro, Qoder, Cursor, xAI, etc.).
func localCompactionHandler(apiKey string, responses http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !inferenceRequestAllowed(writer, request, apiKey) {
			return
		}
		raw, err := readInboundBody(writer, request, localCompactionMaxBodyBytes)
		if err != nil {
			writeInboundBodyError(writer, err)
			return
		}
		var payload map[string]any
		if decodeProtocolJSON(raw, &payload) != nil || payload == nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses compaction request JSON")
			return
		}
		if strings.TrimSpace(stringValue(payload["model"])) == "" {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "model is required")
			return
		}
		items, err := normalizeResponsesInputSlice(payload["input"])
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		instructions := strings.TrimSpace(stringValue(payload["instructions"]))
		if instructions != "" {
			instructions = localCompactionSystemPrefix + instructions
		} else {
			instructions = strings.TrimSpace(localCompactionSystemPrefix)
		}

		attempts := []struct {
			maxTotalBytes int
			maxToolBytes  int
			maxMsgBytes   int
		}{
			{0, defaultCompactionToolMaxBytes, defaultCompactionMessageMaxBytes},
			{compactionRetryMidBudgetBytes, compactionRetryMidItemMaxBytes, compactionRetryMidItemMaxBytes},
			{compactionRetryTightBudgetBytes, compactionRetryTightItemMaxBytes, compactionRetryTightItemMaxBytes},
		}

		var rec *compactBufferWriter
		for i, plan := range attempts {
			compacted := buildLocalCompactionItemsBudgeted(items, plan.maxTotalBytes, plan.maxToolBytes, plan.maxMsgBytes)
			reqBody := make(map[string]any, len(payload)+4)
			for k, v := range payload {
				reqBody[k] = v
			}
			reqBody["input"] = compacted
			reqBody["instructions"] = instructions
			reqBody["stream"] = false
			reqBody["store"] = false
			delete(reqBody, "previous_response_id")
			delete(reqBody, "tools")
			delete(reqBody, "tool_choice")
			delete(reqBody, "parallel_tool_calls")
			delete(reqBody, "text")
			delete(reqBody, "reasoning")

			encoded, encErr := json.Marshal(reqBody)
			if encErr != nil {
				writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "encode local compaction request: "+encErr.Error())
				return
			}

			inner := request.Clone(request.Context())
			inner.Body = io.NopCloser(bytes.NewReader(encoded))
			inner.ContentLength = int64(len(encoded))
			inner.URL.Path = "/v1/responses"
			rec = newCompactBufferWriter()
			responses(rec, inner)

			if rec.status >= 200 && rec.status < 300 {
				break
			}
			if i+1 < len(attempts) && IsCompactionContextOverflow(rec.status, rec.body.Bytes()) {
				LogWarnEvent("local_compaction_overflow_retry", "component", "oauthproxy",
					"attempt", i+1, "next_budget_bytes", attempts[i+1].maxTotalBytes)
				continue
			}
			break
		}

		if rec.status < 200 || rec.status >= 300 {
			copyProxyResponseHeaders(writer.Header(), rec.header)
			if ct := rec.header.Get("Content-Type"); ct != "" {
				writer.Header().Set("Content-Type", ct)
			}
			writer.WriteHeader(rec.status)
			_, _ = writer.Write(rec.body.Bytes())
			return
		}

		result, ok := parseLocalCompactionResult(rec.body.Bytes(), rec.header.Get("Content-Type"))
		if !ok {
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", "invalid Responses payload during local compaction")
			return
		}
		summary := extractLocalCompactionSummary(result)
		status := stringValue(result["status"])
		if summary == "" || (status != "completed" && status != "incomplete") {
			writeProxyErrorDetail(writer, http.StatusBadGateway, "api_error", "server_error", "", "provider did not complete the compaction summary")
			return
		}
		summaryItem := map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "Conversation summary:\n" + summary,
			}},
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id":         "cmp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			"object":     "response.compaction",
			"created_at": time.Now().Unix(),
			"output":     []any{summaryItem},
			"usage":      result["usage"],
		})
	}
}

func parseLocalCompactionResult(body []byte, contentType string) (map[string]any, bool) {
	var result map[string]any
	if decodeProtocolJSON(body, &result) == nil && stringValue(result["object"]) == "response" {
		return result, true
	}
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") && !bytes.Contains(body, []byte("data:")) {
		return nil, false
	}
	var terminal map[string]any
	_ = readSSEPayload(bytes.NewReader(body), "local_compact", func(raw []byte) error {
		var event map[string]any
		if decodeProtocolJSON(raw, &event) != nil {
			return nil
		}
		kind := stringValue(event["type"])
		if kind == "response.completed" || kind == "response.incomplete" {
			if resp := mapValue(event["response"]); resp != nil && stringValue(resp["object"]) == "response" {
				terminal = resp
			}
		}
		return nil
	})
	return terminal, terminal != nil
}

type compactBufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCompactBufferWriter() *compactBufferWriter {
	return &compactBufferWriter{
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (w *compactBufferWriter) Header() http.Header         { return w.header }
func (w *compactBufferWriter) WriteHeader(statusCode int)  { w.status = statusCode }
func (w *compactBufferWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func extractLocalCompactionSummary(result map[string]any) string {
	var parts []string
	for _, entry := range sliceValue(result["output"]) {
		item := mapValue(entry)
		if item == nil || stringValue(item["type"]) != "message" {
			continue
		}
		for _, rawPart := range sliceValue(item["content"]) {
			part := mapValue(rawPart)
			if part == nil || stringValue(part["type"]) != "output_text" {
				continue
			}
			if text := stringValue(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func buildLocalCompactionItems(items []any) []any {
	return buildLocalCompactionItemsBudgeted(items, 0, defaultCompactionToolMaxBytes, defaultCompactionMessageMaxBytes)
}

func buildLocalCompactionItemsBudgeted(items []any, maxTotalBytes, maxToolBytes, maxMsgBytes int) []any {
	result := make([]any, 0, len(items)+1)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := stringValue(item["type"])
		switch kind {
		case "compaction_trigger", "reasoning":
			continue
		case "compaction":
			if summary, ok := DecodeCompactionSummary(stringValue(item["encrypted_content"])); ok {
				result = append(result, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": "Previous conversation summary:\n" + ExcerptCompactionText(summary, maxMsgBytes),
				})
			}
		case "function_call", "custom_tool_call", "tool_search_call", "computer_call", "shell_call", "local_shell_call", "apply_patch_call":
			name := stringValue(item["name"])
			var input any = item["arguments"]
			switch kind {
			case "custom_tool_call":
				input = item["input"]
			case "tool_search_call":
				name = "tool_search"
				if item["arguments"] != nil {
					input = item["arguments"]
				} else {
					input = item["execution"]
				}
			case "computer_call":
				name = "computer"
				if item["actions"] != nil {
					input = item["actions"]
				} else {
					input = item["action"]
				}
			case "shell_call", "local_shell_call":
				name = "shell"
				input = item["action"]
			case "apply_patch_call":
				name = "apply_patch"
				if item["operation"] != nil {
					input = item["operation"]
				} else {
					input = item["patch"]
				}
			}
			result = append(result, map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": fmt.Sprintf("Previous tool call %s (%s):\n%s", name, builtinCallID(item), ExcerptCompactionText(sanitizeLocalCompactionText(input), maxToolBytes)),
			})
		case "function_call_output", "custom_tool_call_output", "tool_search_output", "computer_call_output", "shell_call_output", "local_shell_call_output", "apply_patch_call_output":
			output := item["output"]
			if kind == "tool_search_output" {
				output = item["tools"]
			}
			result = append(result, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": fmt.Sprintf("Previous tool result (%s):\n%s", builtinCallID(item), ExcerptCompactionText(sanitizeLocalCompactionText(output), maxToolBytes)),
			})
		case "", "message":
			result = append(result, sanitizeLocalCompactionMessage(item, maxMsgBytes))
		default:
			result = append(result, item)
		}
	}
	if maxTotalBytes > 0 {
		result = trimLocalCompactionHistory(result, maxTotalBytes)
	}
	return append(result, map[string]any{
		"type":    "message",
		"role":    "user",
		"content": localCompactionPrompt,
	})
}

func trimLocalCompactionHistory(items []any, maxTotalBytes int) []any {
	if len(items) == 0 || maxTotalBytes <= 0 {
		return items
	}
	sizes := make([]int, len(items))
	total := 0
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err == nil {
			sizes[i] = len(raw)
			total += len(raw)
		}
	}
	if total <= maxTotalBytes {
		return items
	}
	headBudget := maxTotalBytes / 4
	tailBudget := maxTotalBytes - headBudget
	headEnd := 0
	headUsed := 0
	for headEnd < len(items) && (headEnd == 0 || headUsed+sizes[headEnd] <= headBudget) {
		headUsed += sizes[headEnd]
		headEnd++
	}
	tailStart := len(items)
	tailUsed := 0
	for tailStart > headEnd && (tailStart == len(items) || tailUsed+sizes[tailStart-1] <= tailBudget) {
		tailStart--
		tailUsed += sizes[tailStart]
	}
	if tailStart <= headEnd {
		return items
	}
	trimmed := make([]any, 0, headEnd+(len(items)-tailStart)+1)
	trimmed = append(trimmed, items[:headEnd]...)
	trimmed = append(trimmed, map[string]any{
		"type":    "message",
		"role":    "user",
		"content": compactionOmittedMiddlePlaceholder,
	})
	trimmed = append(trimmed, items[tailStart:]...)
	return trimmed
}

func sanitizeLocalCompactionMessage(item map[string]any, maxMsgBytes int) map[string]any {
	switch content := item["content"].(type) {
	case string:
		next := content
		if isResponsesHistoryInlineImageString(next) {
			next = "[image omitted]"
		} else {
			next = ExcerptCompactionText(next, maxMsgBytes)
		}
		if next == content {
			return item
		}
		cloned := cloneMapShallow(item)
		cloned["content"] = next
		return cloned
	case []any:
		partType := "input_text"
		if stringValue(item["role"]) == "assistant" {
			partType = "output_text"
		}
		changed := false
		sanitized := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				sanitized = append(sanitized, raw)
				continue
			}
			if isResponsesHistoryImageMap(part) {
				changed = true
				sanitized = append(sanitized, map[string]any{"type": partType, "text": "[image omitted]"})
				continue
			}
			if isResponsesHistoryFileMap(part) {
				changed = true
				sanitized = append(sanitized, map[string]any{"type": partType, "text": "[file omitted]"})
				continue
			}
			if isResponsesHistoryAudioMap(part) {
				changed = true
				sanitized = append(sanitized, map[string]any{"type": partType, "text": "[audio omitted]"})
				continue
			}
			if text, ok := part["text"].(string); ok {
				next := text
				if isResponsesHistoryInlineImageString(next) {
					next = "[image omitted]"
				} else {
					next = ExcerptCompactionText(next, maxMsgBytes)
				}
				if next != text {
					changed = true
					clonedPart := cloneMapShallow(part)
					clonedPart["text"] = next
					sanitized = append(sanitized, clonedPart)
					continue
				}
			}
			sanitized = append(sanitized, raw)
		}
		if !changed {
			return item
		}
		cloned := cloneMapShallow(item)
		cloned["content"] = sanitized
		return cloned
	default:
		return item
	}
}

func sanitizeLocalCompactionText(value any) string {
	sanitized, _ := sanitizeLocalCompactionValue(value)
	if s, ok := sanitized.(string); ok {
		return s
	}
	return responsesTextValue(sanitized)
}

func sanitizeLocalCompactionValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		if isResponsesHistoryInlineImageString(v) {
			return "[image omitted]", true
		}
		return v, false
	case []any:
		changed := false
		out := make([]any, len(v))
		for i, elem := range v {
			next, c := sanitizeLocalCompactionValue(elem)
			changed = changed || c
			out[i] = next
		}
		return out, changed
	case map[string]any:
		if isResponsesHistoryImageMap(v) {
			return "[image omitted]", true
		}
		if isResponsesHistoryFileMap(v) {
			return "[file omitted]", true
		}
		if isResponsesHistoryAudioMap(v) {
			return "[audio omitted]", true
		}
		changed := false
		out := make(map[string]any, len(v))
		for k, elem := range v {
			switch k {
			case "image_url":
				if elem != nil {
					out[k] = "[image omitted]"
					changed = true
					continue
				}
			case "file_data":
				if elem != nil {
					out[k] = "[file omitted]"
					changed = true
					continue
				}
			case "input_audio":
				if elem != nil {
					out[k] = "[audio omitted]"
					changed = true
					continue
				}
			}
			next, c := sanitizeLocalCompactionValue(elem)
			changed = changed || c
			out[k] = next
		}
		return out, changed
	default:
		return value, false
	}
}

func cloneMapShallow(src map[string]any) map[string]any {
	cloned := make(map[string]any, len(src))
	for k, v := range src {
		cloned[k] = v
	}
	return cloned
}
