package oauthproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	responsesHistoryMaxEntries   = 64
	responsesHistoryMaxTotalSize = 64 << 20
	responsesHistoryMaxEntrySize = 16 << 20
	responsesHistoryMaxItems     = 1024
	responsesHistoryMaxItemSize  = 16 << 20
	responsesHistoryMaxPeekBytes = int64(128 << 20)
	responsesHistoryTTL          = 30 * time.Minute
)

type responsesHistoryEntry struct {
	id        string
	itemsJSON []byte
	expiresAt time.Time
}

// responsesHistoryCache stores recent Responses turns in memory per loopback
// runtime so clients using stateful `previous_response_id` or `item_reference`
// (such as Grok Build or OpenAI Responses SDKs) work seamlessly against
// stateless or converted providers without persisting conversation state
// upstream (`store=false`).
type responsesHistoryCache struct {
	mu            sync.Mutex
	entries       map[string]responsesHistoryEntry
	order         []string
	totalSize     int
	items         map[string]responsesHistoryEntry
	itemOrder     []string
	itemTotalSize int
}

func newResponsesHistoryCache() *responsesHistoryCache {
	return &responsesHistoryCache{
		entries: make(map[string]responsesHistoryEntry),
		items:   make(map[string]responsesHistoryEntry),
	}
}

const responsesHistoryMaxRetainedImages = 6

func (c *responsesHistoryCache) put(id string, items []any, now time.Time) {
	id = strings.TrimSpace(id)
	if id == "" || len(items) == 0 {
		return
	}
	encoded, ok := marshalBoundedResponsesHistory(items, responsesHistoryMaxEntrySize)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if existing, ok := c.entries[id]; ok {
		c.totalSize -= len(existing.itemsJSON)
		c.removeOrderLocked(id)
	}
	for (len(c.order) >= responsesHistoryMaxEntries || c.totalSize+len(encoded) > responsesHistoryMaxTotalSize) && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if victim, ok := c.entries[oldest]; ok {
			c.totalSize -= len(victim.itemsJSON)
			delete(c.entries, oldest)
		}
	}
	c.entries[id] = responsesHistoryEntry{
		id:        id,
		itemsJSON: encoded,
		expiresAt: now.Add(responsesHistoryTTL),
	}
	c.order = append(c.order, id)
	c.totalSize += len(encoded)
	c.indexItemsLocked(items, now)
}

func (c *responsesHistoryCache) indexItemsLocked(items []any, now time.Time) {
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemID := strings.TrimSpace(stringValue(item["id"]))
		if itemID == "" {
			continue
		}
		encoded, err := json.Marshal(item)
		if err != nil || len(encoded) == 0 || len(encoded) > responsesHistoryMaxEntrySize {
			continue
		}
		if existing, ok := c.items[itemID]; ok {
			c.itemTotalSize -= len(existing.itemsJSON)
			c.removeItemOrderLocked(itemID)
		}
		for (len(c.itemOrder) >= responsesHistoryMaxItems || c.itemTotalSize+len(encoded) > responsesHistoryMaxItemSize) && len(c.itemOrder) > 0 {
			oldest := c.itemOrder[0]
			c.itemOrder = c.itemOrder[1:]
			if victim, ok := c.items[oldest]; ok {
				c.itemTotalSize -= len(victim.itemsJSON)
				delete(c.items, oldest)
			}
		}
		c.items[itemID] = responsesHistoryEntry{
			id:        itemID,
			itemsJSON: encoded,
			expiresAt: now.Add(responsesHistoryTTL),
		}
		c.itemOrder = append(c.itemOrder, itemID)
		c.itemTotalSize += len(encoded)
	}
}

func (c *responsesHistoryCache) getItem(id string, now time.Time) (map[string]any, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	entry, ok := c.items[id]
	if !ok {
		return nil, false
	}
	entry.expiresAt = now.Add(responsesHistoryTTL)
	c.items[id] = entry
	c.removeItemOrderLocked(id)
	c.itemOrder = append(c.itemOrder, id)

	var item map[string]any
	if err := decodeProtocolJSON(entry.itemsJSON, &item); err != nil || item == nil {
		return nil, false
	}
	return item, true
}

func marshalBoundedResponsesHistory(items []any, limit int) ([]byte, bool) {
	totalImages := countResponsesHistoryImages(items)
	candidates := []int{responsesHistoryMaxRetainedImages, 2, 1, 0}
	if totalImages <= responsesHistoryMaxRetainedImages {
		encoded, err := json.Marshal(items)
		if err == nil && len(encoded) > 0 && len(encoded) <= limit {
			return encoded, true
		}
		candidates = []int{2, 1, 0}
	}
	for _, keep := range candidates {
		pruned := pruneResponsesHistoryImages(items, totalImages, keep)
		encoded, err := json.Marshal(pruned)
		if err == nil && len(encoded) > 0 && len(encoded) <= limit {
			return encoded, true
		}
	}
	return nil, false
}

func isResponsesHistoryImageMap(m map[string]any) bool {
	kind, _ := m["type"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "input_image", "computer_screenshot", "image", "image_url":
		return true
	}
	if urlVal, ok := m["image_url"].(string); ok && urlVal != "" {
		return true
	}
	if _, ok := m["image_url"].(map[string]any); ok {
		return true
	}
	return false
}

func isResponsesHistoryFileMap(m map[string]any) bool {
	kind := strings.ToLower(strings.TrimSpace(stringValue(m["type"])))
	if kind == "input_file" || kind == "file" {
		if strings.TrimSpace(stringValue(m["file_data"])) != "" {
			return true
		}
		if nested, ok := m["file"].(map[string]any); ok && strings.TrimSpace(stringValue(nested["file_data"])) != "" {
			return true
		}
	}
	return false
}

func isResponsesHistoryAudioMap(m map[string]any) bool {
	kind := strings.ToLower(strings.TrimSpace(stringValue(m["type"])))
	if kind == "input_audio" {
		if strings.TrimSpace(stringValue(m["data"])) != "" {
			return true
		}
		if nested, ok := m["input_audio"].(map[string]any); ok && strings.TrimSpace(stringValue(nested["data"])) != "" {
			return true
		}
	}
	return false
}

func isResponsesHistoryInlineImageString(value string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(trimmed, "data:image/") || strings.HasPrefix(trimmed, "data:;base64,")
}

func countResponsesHistoryImages(value any) int {
	switch v := value.(type) {
	case string:
		if isResponsesHistoryInlineImageString(v) {
			return 1
		}
		return 0
	case []any:
		n := 0
		for _, elem := range v {
			n += countResponsesHistoryImages(elem)
		}
		return n
	case map[string]any:
		if isResponsesHistoryImageMap(v) || isResponsesHistoryFileMap(v) || isResponsesHistoryAudioMap(v) {
			return 1
		}
		n := 0
		for _, elem := range v {
			n += countResponsesHistoryImages(elem)
		}
		return n
	default:
		return 0
	}
}

func pruneResponsesHistoryImages(items []any, totalImages, keepNewest int) []any {
	dropLimit := totalImages - keepNewest
	if dropLimit <= 0 {
		return items
	}
	dropped := 0
	demotedCalls := make(map[string]bool)
	out := make([]any, len(items))
	changed := false
	for i, raw := range items {
		if dropped >= dropLimit {
			out[i] = raw
			continue
		}
		if item, ok := raw.(map[string]any); ok && stringValue(item["type"]) == "computer_call_output" {
			if imgCount := countResponsesHistoryImages(item["output"]); imgCount > 0 {
				dropped += imgCount
				changed = true
				callID := builtinCallID(item)
				if callID != "" {
					demotedCalls[callID] = true
				}
				summary := "[historical screenshot omitted]"
				if currentURL := strings.TrimSpace(stringValue(item["current_url"])); currentURL != "" {
					summary = "Current URL: " + currentURL + "\n" + summary
				}
				if callID != "" {
					summary = "Previous computer call output (" + callID + "):\n" + summary
				}
				out[i] = map[string]any{
					"type":    "message",
					"role":    "user",
					"content": []any{map[string]any{"type": "input_text", "text": summary}},
				}
				continue
			}
		}
		before := dropped
		next := pruneResponsesHistoryValue(raw, &dropped, dropLimit)
		if dropped != before {
			changed = true
		}
		out[i] = next
	}
	if len(demotedCalls) > 0 {
		for i, raw := range out {
			item, ok := raw.(map[string]any)
			if !ok || stringValue(item["type"]) != "computer_call" {
				continue
			}
			callID := builtinCallID(item)
			if !demotedCalls[callID] {
				continue
			}
			args := string(builtinCallArguments("computer_call", item))
			out[i] = map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "Previous computer call (" + callID + "):\n" + args}},
			}
		}
	}
	if !changed {
		return items
	}
	return out
}

func pruneResponsesHistoryValue(value any, dropped *int, dropLimit int) any {
	if *dropped >= dropLimit {
		return value
	}
	switch v := value.(type) {
	case string:
		if isResponsesHistoryInlineImageString(v) {
			*dropped++
			return "[historical screenshot omitted]"
		}
		return v
	case []any:
		out := make([]any, len(v))
		changed := false
		for i, elem := range v {
			before := *dropped
			next := pruneResponsesHistoryValue(elem, dropped, dropLimit)
			if *dropped != before {
				changed = true
			}
			out[i] = next
		}
		if !changed {
			return v
		}
		return out
	case map[string]any:
		if isResponsesHistoryImageMap(v) {
			*dropped++
			return map[string]any{"type": "input_text", "text": "[historical screenshot omitted]"}
		}
		if isResponsesHistoryFileMap(v) {
			*dropped++
			return map[string]any{"type": "input_text", "text": "[file omitted]"}
		}
		if isResponsesHistoryAudioMap(v) {
			*dropped++
			return map[string]any{"type": "input_text", "text": "[audio omitted]"}
		}
		out := make(map[string]any, len(v))
		changed := false
		for k, elem := range v {
			before := *dropped
			next := pruneResponsesHistoryValue(elem, dropped, dropLimit)
			if *dropped != before {
				changed = true
			}
			out[k] = next
		}
		if !changed {
			return v
		}
		return out
	default:
		return value
	}
}

func (c *responsesHistoryCache) get(id string, now time.Time) ([]any, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	entry, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	entry.expiresAt = now.Add(responsesHistoryTTL)
	c.entries[id] = entry
	c.removeOrderLocked(id)
	c.order = append(c.order, id)

	var items []any
	if err := decodeProtocolJSON(entry.itemsJSON, &items); err != nil {
		return nil, false
	}
	return items, true
}

func (c *responsesHistoryCache) pruneLocked(now time.Time) {
	if len(c.order) > 0 {
		kept := c.order[:0]
		for _, id := range c.order {
			entry, ok := c.entries[id]
			if !ok {
				continue
			}
			if now.After(entry.expiresAt) {
				c.totalSize -= len(entry.itemsJSON)
				delete(c.entries, id)
				continue
			}
			kept = append(kept, id)
		}
		c.order = kept
	}
	if len(c.itemOrder) > 0 {
		kept := c.itemOrder[:0]
		for _, id := range c.itemOrder {
			entry, ok := c.items[id]
			if !ok {
				continue
			}
			if now.After(entry.expiresAt) {
				c.itemTotalSize -= len(entry.itemsJSON)
				delete(c.items, id)
				continue
			}
			kept = append(kept, id)
		}
		c.itemOrder = kept
	}
}

func (c *responsesHistoryCache) removeOrderLocked(target string) {
	for i, id := range c.order {
		if id == target {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *responsesHistoryCache) removeItemOrderLocked(target string) {
	for i, id := range c.itemOrder {
		if id == target {
			c.itemOrder = append(c.itemOrder[:i], c.itemOrder[i+1:]...)
			return
		}
	}
}

type statefulExpandError struct {
	code    string
	param   string
	message string
}

func expandResponsesStatefulRequest(cache *responsesHistoryCache, raw []byte, now time.Time) ([]byte, *statefulExpandError) {
	prev := gjson.GetBytes(raw, "previous_response_id")
	hasPrev := prev.Exists() && prev.Type != gjson.Null
	hasItemRef := bytes.Contains(raw, []byte(`"item_reference"`))
	if !hasPrev && !hasItemRef {
		return raw, nil
	}
	var payload map[string]any
	if err := decodeProtocolJSON(raw, &payload); err != nil {
		return raw, nil
	}
	changed := false
	var priorItems []any
	if hasPrev {
		if prev.Type != gjson.String {
			return nil, &statefulExpandError{param: "previous_response_id", message: "previous_response_id must be a string"}
		}
		prevID := strings.TrimSpace(prev.String())
		delete(payload, "previous_response_id")
		changed = true
		if prevID != "" {
			var ok bool
			priorItems, ok = cache.get(prevID, now)
			if !ok {
				return nil, &statefulExpandError{
					code:    "previous_response_not_found",
					param:   "previous_response_id",
					message: "previous_response_id is unknown or expired",
				}
			}
		}
	}

	currentItems, err := normalizeResponsesInputSlice(payload["input"])
	if err != nil {
		if hasPrev {
			return nil, &statefulExpandError{param: "input", message: err.Error()}
		}
		return raw, nil
	}
	if hasItemRef {
		expanded := make([]any, 0, len(currentItems))
		for _, elem := range currentItems {
			item, isMap := elem.(map[string]any)
			if isMap && stringValue(item["type"]) == "item_reference" {
				refID := strings.TrimSpace(stringValue(item["id"]))
				resolved, ok := cache.getItem(refID, now)
				if !ok {
					return nil, &statefulExpandError{
						code:    "item_not_found",
						param:   "input",
						message: "item_reference id is unknown or expired",
					}
				}
				expanded = append(expanded, resolved)
				changed = true
				continue
			}
			expanded = append(expanded, elem)
		}
		currentItems = expanded
	}
	if len(priorItems) > 0 {
		combined := make([]any, 0, len(priorItems)+len(currentItems))
		combined = append(combined, priorItems...)
		combined = append(combined, currentItems...)
		payload["input"] = combined
		changed = true
	} else if hasItemRef && changed {
		payload["input"] = currentItems
	}
	if !changed {
		return raw, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &statefulExpandError{message: "encode expanded Responses request"}
	}
	return encoded, nil
}

func withResponsesHistory(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	return withSharedResponsesHistory(apiKey, newResponsesHistoryCache(), next, true)
}

func withSharedResponsesHistory(apiKey string, cache *responsesHistoryCache, next http.HandlerFunc, recordOutput bool) http.HandlerFunc {
	if cache == nil {
		cache = newResponsesHistoryCache()
	}
	return func(writer http.ResponseWriter, request *http.Request) {
		if !loopbackAuthorized(request, apiKey) || request.Method != http.MethodPost || request.Body == nil {
			next(writer, request)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, responsesHistoryMaxPeekBytes+1))
		if err != nil {
			writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if int64(len(raw)) > responsesHistoryMaxPeekBytes {
			writeInboundBodyError(writer, &requestBodyTooLargeError{limitLabel: requestLimitLabel(responsesHistoryMaxPeekBytes)})
			return
		}

		storeField := gjson.GetBytes(raw, "store")
		shouldStore := recordOutput && !(storeField.Exists() && storeField.Type == gjson.False)

		requestBytes, expandErr := expandResponsesStatefulRequest(cache, raw, time.Now())
		if expandErr != nil {
			writeProxyErrorDetail(writer, http.StatusBadRequest, "invalid_request_error", expandErr.code, expandErr.param, expandErr.message)
			return
		}

		request.Body = io.NopCloser(bytes.NewReader(requestBytes))
		request.ContentLength = int64(len(requestBytes))
		if !shouldStore {
			next(writer, request)
			return
		}
		observer := &responsesHistoryObserver{target: writer}
		next(observer, request)
		if responseID, outputItems, ok := observer.completedResponse(); ok {
			inputItems := extractResponsesInputForCache(requestBytes)
			history := make([]any, 0, len(inputItems)+len(outputItems))
			history = append(history, inputItems...)
			history = append(history, outputItems...)
			cache.put(responseID, history, time.Now())
		}
	}
}

func normalizeResponsesInputSlice(raw any) ([]any, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "message", "role": "user", "content": value}}, nil
	case []any:
		return value, nil
	default:
		return nil, errInvalidResponsesInput
	}
}

var errInvalidResponsesInput = &proxyInputError{"Responses input must be a string or item array"}

type proxyInputError struct {
	msg string
}

func (e *proxyInputError) Error() string { return e.msg }

func extractResponsesInputForCache(raw []byte) []any {
	var payload map[string]any
	if decodeProtocolJSON(raw, &payload) != nil {
		return nil
	}
	items, _ := normalizeResponsesInputSlice(payload["input"])
	return items
}

func writeProxyErrorWithCode(writer http.ResponseWriter, status int, errorType, code, message string) {
	writeProxyErrorDetail(writer, status, errorType, code, "", message)
}

type responsesHistoryObserver struct {
	target        http.ResponseWriter
	status        int
	wroteHeader   bool
	streaming     bool
	overflow      bool
	body          bytes.Buffer
	pendingSSE    []byte
	completedBody []byte
}

func (o *responsesHistoryObserver) Header() http.Header {
	return o.target.Header()
}

func (o *responsesHistoryObserver) Unwrap() http.ResponseWriter {
	return o.target
}

func (o *responsesHistoryObserver) Flush() {
	if flusher, ok := o.target.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (o *responsesHistoryObserver) WriteHeader(statusCode int) {
	if !o.wroteHeader {
		o.status = statusCode
		o.wroteHeader = true
		o.streaming = strings.Contains(strings.ToLower(o.target.Header().Get("Content-Type")), "text/event-stream")
	}
	o.target.WriteHeader(statusCode)
}

func (o *responsesHistoryObserver) Write(p []byte) (int, error) {
	if !o.wroteHeader {
		o.WriteHeader(http.StatusOK)
	}
	n, err := o.target.Write(p)
	if n > 0 && o.status == http.StatusOK && !o.overflow {
		chunk := p[:n]
		if o.streaming {
			o.observeSSE(chunk)
		} else if o.body.Len()+len(chunk) <= responsesHistoryMaxEntrySize {
			_, _ = o.body.Write(chunk)
		} else {
			o.body.Reset()
			o.overflow = true
		}
	}
	return n, err
}

func (o *responsesHistoryObserver) observeSSE(chunk []byte) {
	o.pendingSSE = append(o.pendingSSE, chunk...)
	for {
		idx, sepLen := nextSSEFrameBoundary(o.pendingSSE)
		if idx < 0 {
			break
		}
		o.inspectSSEFrame(o.pendingSSE[:idx])
		o.pendingSSE = o.pendingSSE[idx+sepLen:]
	}
	if len(o.pendingSSE) > responsesHistoryMaxEntrySize {
		o.pendingSSE = nil
		o.overflow = true
	}
}

func (o *responsesHistoryObserver) inspectSSEFrame(frame []byte) {
	if !bytes.Contains(frame, []byte("response.completed")) && !bytes.Contains(frame, []byte("response.incomplete")) {
		return
	}
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if len(data) == 0 || len(data) > responsesHistoryMaxEntrySize {
		return
	}
	kind := gjson.GetBytes(data, "type").String()
	if kind != "response.completed" && kind != "response.incomplete" {
		return
	}
	if resp := gjson.GetBytes(data, "response"); resp.Exists() && resp.IsObject() {
		o.completedBody = append(o.completedBody[:0], []byte(resp.Raw)...)
	}
}

func (o *responsesHistoryObserver) completedResponse() (string, []any, bool) {
	if o.status != http.StatusOK || o.overflow {
		return "", nil, false
	}
	if o.streaming && len(o.pendingSSE) > 0 {
		o.inspectSSEFrame(o.pendingSSE)
		o.pendingSSE = nil
	}
	raw := o.completedBody
	if !o.streaming {
		raw = o.body.Bytes()
	}
	if len(raw) == 0 {
		return "", nil, false
	}
	var response map[string]any
	if decodeProtocolJSON(raw, &response) != nil {
		return "", nil, false
	}
	if !o.streaming {
		if stringValue(response["object"]) != "response" {
			return "", nil, false
		}
		status := stringValue(response["status"])
		if status != "completed" && status != "incomplete" {
			return "", nil, false
		}
	}
	id := strings.TrimSpace(stringValue(response["id"]))
	if id == "" {
		return "", nil, false
	}
	output, _ := response["output"].([]any)
	return id, output, true
}
