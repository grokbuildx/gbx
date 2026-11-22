package oauthproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

const (
	kiroMaxToolNameLength      = 63
	kiroMaxInlineMediaSegments = 100
)

var (
	kiroSessionUUIDPattern = regexp.MustCompile(`(?i)session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	kiroModernModelPattern = regexp.MustCompile(`(?i)^claude-(sonnet|opus|haiku|fable)-([0-9]+)[-.]([0-9]+)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
	kiroLegacyModelPattern = regexp.MustCompile(`(?i)^claude-([0-9]+)-([0-9]+)-(sonnet|opus|haiku|fable)(?:-[0-9]{8})?(?:-thinking|-latest)?$`)
)

type kiroConvertedRequest struct {
	adapterRequest
	body            map[string]any
	model           string
	inlineMedia     int
	droppedMedia    int
	dedupedMedia    int
	resizedMedia    int
	correctedMedia  int
	droppedToolUses int
	droppedToolRuns int
	emptyToolRuns   int
	truncatedTexts  int
	droppedText     int
	largestText     int
	originalBody    int
	finalBody       int
	originalTokens  int
	finalTokens     int
	budgetMedia     int
	budgetHistory   int
	budgetTexts     int
	budgetTextBytes int
	budgetTools     int
}

type kiroContent struct {
	text        string
	thinking    string
	images      []any
	toolResults []any
	toolUses    []any
	toolNames   []string
	// emptyResults counts tool results that arrived with no content at all.
	emptyResults int
}

// kiroEmptyToolResultText stands in for a tool result that carried no content.
//
// Forwarding an empty string makes a tool that legitimately produced no output
// indistinguishable from a result that was lost on the way, and the model then
// reports the tooling as broken instead of continuing.
const kiroEmptyToolResultText = "(no output)"

func convertResponsesToKiro(raw []byte) (*kiroConvertedRequest, error) {
	return convertResponsesToKiroWithSession(raw, "")
}

func convertResponsesToKiroWithSession(raw []byte, httpSessionID string) (*kiroConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	if request.MaxOutputTokens <= 0 {
		request.MaxOutputTokens = 32_000
	}

	messages := append([]conversationTurn(nil), request.Turns...)
	if len(messages) == 0 {
		return nil, fmt.Errorf("input must contain a message")
	}
	// Preserve a trailing assistant turn inside history instead of truncating it,
	// and synthesize a minimal continuation prompt as Kiro's currentMessage.
	if messages[len(messages)-1].Role == "assistant" {
		messages = append(messages, conversationTurn{
			Role:  "user",
			Parts: []conversationPart{textPart("Continue.")},
		})
	}
	model := mapKiroModel(request.Model)
	toolNameMap := make(map[string]string)
	tools, declaredTools, err := convertKiroTools(request.Tools, toolNameMap)
	if err != nil {
		return nil, err
	}

	thinkingEnabled := request.thinkingEnabled()
	history := make([]any, 0, len(messages)+3)
	systemText := strings.TrimSpace(request.System)
	if instruction := structuredOutputPromptInstruction(request.Format); instruction != "" {
		if systemText != "" {
			systemText += "\n\n"
		}
		systemText += instruction
	}
	if thinkingEnabled {
		prefix := kiroThinkingPrefix(request.Effort)
		systemText = strings.TrimSpace(prefix + "\n" + systemText)
	}
	if systemText != "" {
		history = append(history,
			map[string]any{"userInputMessage": map[string]any{
				"content": systemText,
				"modelId": model,
				"origin":  "AI_EDITOR",
			}},
			map[string]any{"assistantResponseMessage": map[string]any{
				"content": "I will follow these instructions.",
			}},
		)
		// If the real conversation begins with an assistant turn, insert a
		// synthetic user separator so it is never merged into the protected
		// system acknowledgment pair.
		if len(messages) > 1 && messages[0].Role == "assistant" {
			history = append(history, map[string]any{"userInputMessage": map[string]any{
				"content": "Continue.",
				"modelId": model,
				"origin":  "AI_EDITOR",
			}})
		}
	}

	historicalToolNames := make(map[string]bool)
	emptyToolRuns := 0
	for _, message := range messages[:len(messages)-1] {
		content, err := parseKiroContentChecked(message, toolNameMap)
		if err != nil {
			return nil, err
		}
		emptyToolRuns += content.emptyResults
		switch message.Role {
		case "user", "developer", "system":
			text := content.text
			if (message.Role == "developer" || message.Role == "system") && strings.TrimSpace(text) != "" {
				text = "[Developer instruction]\n" + text
			}
			userMessage := map[string]any{
				"content": text,
				"modelId": model,
				"origin":  "AI_EDITOR",
			}
			if len(content.images) > 0 {
				userMessage["images"] = content.images
			}
			if len(content.toolResults) > 0 {
				userMessage["userInputMessageContext"] = map[string]any{
					"envState":    kiroEnvironmentState(),
					"toolResults": content.toolResults,
				}
			}
			history = append(history, map[string]any{"userInputMessage": userMessage})
		case "assistant":
			assistantText := content.text
			if content.thinking != "" {
				assistantText = strings.TrimSpace("<thinking>" + content.thinking + "</thinking>\n\n" + assistantText)
			}
			if assistantText == "" && len(content.toolUses) > 0 {
				assistantText = " "
			}
			assistantMessage := map[string]any{"content": assistantText}
			if len(content.toolUses) > 0 {
				assistantMessage["toolUses"] = content.toolUses
			}
			for _, name := range content.toolNames {
				historicalToolNames[name] = true
			}
			history = append(history, map[string]any{"assistantResponseMessage": assistantMessage})
		}
	}
	history = normalizeKiroHistory(history)

	lastTurn := messages[len(messages)-1]
	currentContent, err := parseKiroContentChecked(lastTurn, toolNameMap)
	if err != nil {
		return nil, err
	}
	currentText := currentContent.text
	if (lastTurn.Role == "developer" || lastTurn.Role == "system") && strings.TrimSpace(currentText) != "" {
		currentText = "[Developer instruction]\n" + currentText
	}
	emptyToolRuns += currentContent.emptyResults
	for name := range historicalToolNames {
		if declaredTools[name] {
			continue
		}
		tools = append(tools, kiroToolDefinition(name, "Tool used in conversation history", map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"required":             []any{},
			"additionalProperties": true,
		}))
	}

	contextValue := map[string]any{
		"envState": kiroEnvironmentState(),
	}
	if len(tools) > 0 && request.ToolChoice.Mode != toolChoiceNone {
		contextValue["tools"] = tools
	}
	if len(currentContent.toolResults) > 0 {
		contextValue["toolResults"] = currentContent.toolResults
	}
	currentMessage := map[string]any{
		"content":                 currentText,
		"modelId":                 model,
		"origin":                  "AI_EDITOR",
		"userInputMessageContext": contextValue,
	}
	if len(currentContent.images) > 0 {
		currentMessage["images"] = currentContent.images
	}

	sessionKey := firstNonEmptyString(request.CacheKey, httpSessionID, request.SafetyIdentifier, request.User)
	conversationState := map[string]any{
		"agentContinuationId": uuid.NewString(),
		"agentTaskType":       "vibe",
		"chatTriggerType":     "MANUAL",
		"conversationId":      kiroConversationID(sessionKey),
		"currentMessage":      map[string]any{"userInputMessage": currentMessage},
	}
	if len(history) > 0 {
		conversationState["history"] = history
	}
	droppedToolUses, droppedToolRuns := normalizeKiroToolPairing(conversationState)
	textStats := limitKiroTextFields(conversationState, kiroMaxTextFieldBytes)
	dedupedMedia := deduplicateKiroInlineMedia(conversationState)
	_, droppedMedia := limitKiroInlineMedia(conversationState, kiroMaxInlineMediaSegments)
	resizedMedia, correctedMedia := processKiroInlineMedia(conversationState)
	// Always drop empty history messages, restore strict role alternation, and
	// ensure currentMessage is non-empty even when below the 160K token budget.
	droppedEmpty := normalizeKiroBudgetHistory(conversationState)
	body := map[string]any{"conversationState": conversationState}
	if additional := kiroReasoningFields(request, model); additional != nil {
		body["additionalModelRequestFields"] = additional
	}
	protectedHistoryPrefix := 0
	if systemText != "" {
		protectedHistoryPrefix = 2
	}
	budgetStats := enforceKiroContentBudget(body, protectedHistoryPrefix, kiroMaxEstimatedContentTokens)
	if err := validateKiroConversationState(conversationState); err != nil {
		return nil, err
	}
	inlineMedia := countKiroInlineMedia(conversationState)
	adapter := kiroAdapter(request, model, toolNameMap)
	if budgetStats.finalTokens > 0 {
		adapter.inputTokens = budgetStats.finalTokens
	}
	return &kiroConvertedRequest{
		adapterRequest:  adapter,
		body:            body,
		model:           model,
		inlineMedia:     inlineMedia,
		droppedMedia:    droppedMedia,
		dedupedMedia:    dedupedMedia,
		resizedMedia:    resizedMedia,
		correctedMedia:  correctedMedia,
		droppedToolUses: droppedToolUses,
		droppedToolRuns: droppedToolRuns,
		emptyToolRuns:   emptyToolRuns,
		truncatedTexts:  textStats.truncated,
		droppedText:     textStats.droppedBytes,
		largestText:     textStats.largestBytes,
		originalBody:    budgetStats.originalBytes,
		finalBody:       budgetStats.finalBytes,
		originalTokens:  budgetStats.originalTokens,
		finalTokens:     budgetStats.finalTokens,
		budgetMedia:     budgetStats.droppedImages,
		budgetHistory:   budgetStats.droppedHistoryMessages + droppedEmpty,
		budgetTexts:     budgetStats.truncatedTexts,
		budgetTextBytes: budgetStats.droppedTextBytes,
		budgetTools:     budgetStats.droppedTools,
	}, nil
}

func countKiroInlineMedia(conversationState map[string]any) int {
	count := 0
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		count += len(kiroAnySlice(message["images"]))
	}
	return count
}

// limitKiroInlineMedia applies Kiro's request-wide inline media limit. Images in
// the current message are most relevant, followed by images in the newest
// historical user messages. Within one message, the newest images are retained.
func limitKiroInlineMedia(conversationState map[string]any, limit int) (kept, dropped int) {
	remaining := max(limit, 0)
	for _, message := range kiroUserMessagesNewestFirst(conversationState) {
		images, ok := message["images"].([]any)
		if !ok || len(images) == 0 {
			delete(message, "images")
			continue
		}
		switch {
		case remaining == 0:
			dropped += len(images)
			delete(message, "images")
			if strings.TrimSpace(metadataString(message, "content")) == "" {
				context, _ := message["userInputMessageContext"].(map[string]any)
				if len(kiroAnySlice(context["toolResults"])) == 0 {
					message["content"] = "[earlier image omitted]"
				}
			}
		case len(images) > remaining:
			dropped += len(images) - remaining
			message["images"] = append([]any(nil), images[len(images)-remaining:]...)
			kept += remaining
			remaining = 0
		default:
			kept += len(images)
			remaining -= len(images)
		}
	}
	return kept, dropped
}

func mapKiroModel(model string) string {
	model = stripContextModelSuffix(strings.TrimSpace(model))
	model = strings.TrimPrefix(strings.ToLower(model), "kiro-")
	if match := kiroModernModelPattern.FindStringSubmatch(model); len(match) == 4 {
		return fmt.Sprintf("claude-%s-%s.%s", strings.ToLower(match[1]), match[2], match[3])
	}
	if match := kiroLegacyModelPattern.FindStringSubmatch(model); len(match) == 4 {
		return fmt.Sprintf("claude-%s-%s.%s", strings.ToLower(match[3]), match[1], match[2])
	}
	model = strings.TrimSuffix(model, "-thinking")
	model = strings.TrimSuffix(model, "-latest")
	if len(model) > 9 {
		if suffix := model[len(model)-8:]; allKiroDigits(suffix) && model[len(model)-9] == '-' {
			model = model[:len(model)-9]
		}
	}
	return model
}

func allKiroDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}

func kiroAdapter(request *conversationRequest, model string, toolNameMap map[string]string) adapterRequest {
	adapter := request.adapter(model)
	adapter.toolNameMap = toolNameMap
	return adapter
}

// kiroThinkingPrefix asks for adaptive reasoning at the requested effort.
func kiroThinkingPrefix(effort string) string {
	return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + normalizeKiroEffort(effort) + "</thinking_effort>"
}

func kiroReasoningFields(request *conversationRequest, model string) map[string]any {
	if !request.thinkingEnabled() {
		return nil
	}
	if !isKiroAdaptiveClaudeModel(model) {
		return nil
	}
	return map[string]any{"output_config": map[string]any{"effort": normalizeKiroEffort(request.Effort)}}
}

func isKiroAdaptiveClaudeModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.ReplaceAll(lower, "-", ".")
	for _, prefix := range []string{
		"claude.opus.4.6",
		"claude.opus.4.7",
		"claude.opus.4.8",
		"claude.opus.5",
		"claude.sonnet.4.6",
		"claude.sonnet.5",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func normalizeKiroEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	case "minimal":
		return "low"
	case "ultra":
		return "max"
	case "xhigh", "x-high", "x_high":
		return "high"
	default:
		return "high"
	}
}

func convertKiroTools(input []conversationTool, nameMap map[string]string) ([]any, map[string]bool, error) {
	tools := make([]any, 0, len(input))
	declared := make(map[string]bool, len(input))
	for _, tool := range input {
		name := shortenKiroToolName(strings.TrimSpace(tool.Name), nameMap)
		if name == "" {
			continue
		}
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			description = tool.Name
		}
		if len(description) > 10_000 {
			description = truncateKiroUTF8(description, 10_000)
		}
		var schema any
		if len(tool.Parameters) == 0 || json.Unmarshal(tool.Parameters, &schema) != nil {
			schema = map[string]any{}
		}
		normalized := normalizeKiroSchemaDepth(schema, true, 0)
		tools = append(tools, kiroToolDefinition(name, description, normalized))
		declared[name] = true
	}
	return tools, declared, nil
}

func truncateKiroUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xc0) == 0x80 {
		cut--
	}
	return s[:cut]
}

const kiroSchemaMaxDepth = 32

func kiroToolDefinition(name, description string, schema any) map[string]any {
	return map[string]any{"toolSpecification": map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{"json": schema},
	}}
}

func normalizeKiroSchema(value any, root bool) any {
	return normalizeKiroSchemaDepth(value, root, 0)
}

func normalizeKiroSchemaDepth(value any, root bool, depth int) any {
	if depth > kiroSchemaMaxDepth {
		if root {
			return map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"required":             []any{},
				"additionalProperties": true,
			}
		}
		return map[string]any{}
	}
	object, ok := value.(map[string]any)
	if !ok {
		if root {
			return map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"required":             []any{},
				"additionalProperties": true,
			}
		}
		return value
	}
	delete(object, "$schema")
	if root {
		if _, hasProperties := object["properties"]; !hasProperties {
			for _, key := range []string{"oneOf", "anyOf", "allOf"} {
				variants, _ := object[key].([]any)
				for _, variant := range variants {
					candidate, _ := variant.(map[string]any)
					if candidate["type"] != "object" {
						continue
					}
					for _, field := range []string{"properties", "required", "additionalProperties", "description"} {
						if value, exists := candidate[field]; exists {
							object[field] = value
						}
					}
					break
				}
				if _, hasProperties := object["properties"]; hasProperties {
					break
				}
			}
		}
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			delete(object, key)
		}
		object["type"] = "object"
		if _, ok := object["properties"].(map[string]any); !ok {
			object["properties"] = map[string]any{}
		}
		if _, ok := object["required"].([]any); !ok {
			object["required"] = []any{}
		}
		if _, ok := object["additionalProperties"]; !ok {
			object["additionalProperties"] = true
		}
	} else {
		for _, key := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
			if _, isNumber := object[key].(float64); isNumber {
				delete(object, key)
			}
		}
		for _, key := range []string{"minimum", "maximum"} {
			if value, isNumber := object[key].(float64); isNumber &&
				(value > 2_147_483_647 || value < -2_147_483_648) {
				delete(object, key)
			}
		}
	}
	if properties, ok := object["properties"].(map[string]any); ok {
		for key, property := range properties {
			properties[key] = normalizeKiroSchemaDepth(property, false, depth+1)
		}
	}
	if items, ok := object["items"]; ok {
		if list, isSlice := items.([]any); isSlice {
			for i, elem := range list {
				list[i] = normalizeKiroSchemaDepth(elem, false, depth+1)
			}
		} else {
			object["items"] = normalizeKiroSchemaDepth(items, false, depth+1)
		}
	}
	if additional, ok := object["additionalProperties"].(map[string]any); ok {
		object["additionalProperties"] = normalizeKiroSchemaDepth(additional, false, depth+1)
	}
	for _, combinator := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := object[combinator].([]any); ok {
			for i, variant := range variants {
				variants[i] = normalizeKiroSchemaDepth(variant, false, depth+1)
			}
		}
	}
	return object
}

func shortenKiroToolName(name string, nameMap map[string]string) string {
	if len(name) <= kiroMaxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:4])
	prefixLength := kiroMaxToolNameLength - len(suffix) - 1
	short := name[:prefixLength] + "_" + suffix
	nameMap[short] = name
	return short
}

func parseKiroContent(turn conversationTurn, nameMap map[string]string) kiroContent {
	content, _ := parseKiroContentChecked(turn, nameMap)
	return content
}

func parseKiroContentChecked(turn conversationTurn, nameMap map[string]string) (kiroContent, error) {
	var result kiroContent
	var textParts []string
	var thinkingParts []string
	for _, part := range turn.Parts {
		switch part.Kind {
		case partText, partRefusal:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		case partReasoning:
			if part.Text != "" {
				thinkingParts = append(thinkingParts, part.Text)
			}
		case partImage:
			if turn.Role == "assistant" {
				continue
			}
			image, err := kiroImageFromPartChecked(part)
			if err != nil {
				return kiroContent{}, err
			}
			if image != nil {
				result.images = append(result.images, image)
			}
		case partFile:
			if turn.Role != "assistant" {
				return kiroContent{}, fmt.Errorf("this provider does not support input_file content parts")
			}
		case partAudio:
			if turn.Role != "assistant" {
				return kiroContent{}, fmt.Errorf("this provider does not support input_audio content parts")
			}
		case partToolResult:
			if part.CallID == "" {
				continue
			}
			resultText := part.outputText()
			var resultImages []any
			for _, piece := range part.Output {
				switch piece.Kind {
				case partImage:
					image, err := kiroImageFromPartChecked(piece)
					if err != nil {
						return kiroContent{}, err
					}
					if image != nil {
						resultImages = append(resultImages, image)
					}
				case partFile:
					return kiroContent{}, fmt.Errorf("this provider does not support input_file in tool results")
				case partAudio:
					return kiroContent{}, fmt.Errorf("this provider does not support input_audio in tool results")
				}
			}
			if len(resultImages) > 0 {
				if resultText == "" {
					resultText = "[image attached]"
				} else if !strings.Contains(resultText, "[image attached]") {
					resultText += "\n[image attached]"
				}
			}
			if resultText == "" && len(resultImages) == 0 {
				resultText = kiroEmptyToolResultText
				result.emptyResults++
			}
			result.images = append(result.images, resultImages...)
			result.toolResults = append(result.toolResults, map[string]any{
				"toolUseId": part.CallID,
				"content":   []any{map[string]any{"text": resultText}},
				"status":    "success",
			})
		case partToolCall:
			name := shortenKiroToolName(part.Name, nameMap)
			if part.CallID == "" || name == "" {
				continue
			}
			var input any = map[string]any{}
			if len(part.Arguments) > 0 {
				var decoded map[string]any
				if json.Unmarshal(part.Arguments, &decoded) == nil && decoded != nil {
					input = decoded
				}
			}
			result.toolUses = append(result.toolUses, map[string]any{
				"toolUseId": part.CallID,
				"name":      name,
				"input":     input,
			})
			result.toolNames = append(result.toolNames, name)
		}
	}
	result.text = strings.Join(textParts, "\n")
	result.thinking = strings.Join(thinkingParts, "")
	return result, nil
}

func kiroImageFromPart(part conversationPart) any {
	img, _ := kiroImageFromPartChecked(part)
	return img
}

func kiroImageFromPartChecked(part conversationPart) (any, error) {
	if part.Data == "" {
		if part.FileID != "" {
			return nil, fmt.Errorf("this provider does not support image file_id references")
		}
		if part.URL != "" {
			return nil, fmt.Errorf("this provider only supports inline base64 data URL images, not remote image URLs")
		}
		return nil, fmt.Errorf("image input is missing base64 data")
	}
	data := stripASCIIWhitespace(part.Data)
	mediaType := strings.ToLower(strings.TrimSpace(part.MediaType))
	if semi := strings.IndexByte(mediaType, ';'); semi >= 0 {
		mediaType = strings.TrimSpace(mediaType[:semi])
	}
	format := strings.TrimPrefix(mediaType, "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	if format != "jpeg" && format != "png" && format != "gif" && format != "webp" {
		if sniffed := sniffBase64ImageMediaType(data); sniffed != "" {
			format = strings.TrimPrefix(sniffed, "image/")
		}
	}
	if format != "jpeg" && format != "png" && format != "gif" && format != "webp" {
		return nil, fmt.Errorf("this provider only supports jpeg, png, gif, and webp images (got %q)", mediaType)
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": data},
	}, nil
}

// kiroConversationID keeps one upstream conversation per client session. A
// session key that is not itself a UUID is hashed into a stable one.
func kiroConversationID(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return uuid.NewString()
	}
	if match := kiroSessionUUIDPattern.FindStringSubmatch(sessionKey); len(match) == 2 {
		return match[1]
	}
	if parsed, err := uuid.Parse(sessionKey); err == nil {
		return parsed.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("gbx-kiro-session:"+sessionKey)).String()
}

func kiroWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return cwd
}

func kiroOperatingSystem() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func kiroEnvironmentState() map[string]any {
	return map[string]any{
		"operatingSystem":         kiroOperatingSystem(),
		"currentWorkingDirectory": kiroWorkingDirectory(),
	}
}

// estimateApproxTokens preserves the ASCII text-budget estimate but charges
// non-ASCII bytes more conservatively, avoiding the old CJK undercount without
// halving the retained English history. Upstream usage remains authoritative.
func estimateApproxTokens(text string) int {
	return estimateUnknownTextTokens(text)
}

// estimateApproxTokensBytes is estimateApproxTokens without forcing a []byte to
// string copy of the payload.
func estimateApproxTokensBytes(raw []byte) int {
	return estimateUnknownTextTokens(raw)
}

func estimateUnknownTextTokens[T ~string | ~[]byte](text T) int {
	ascii := 0
	for i := 0; i < len(text); i++ {
		if text[i] < 128 {
			ascii++
		}
	}
	return (ascii+3)/4 + (len(text)-ascii+1)/2
}
