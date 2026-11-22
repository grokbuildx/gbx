package codex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func migrationSessionID(r *http.Request, metadata ...map[string]any) string {
	for _, fields := range metadata {
		for _, key := range []string{"thread_id", "session_id"} {
			if value, _ := fields[key].(string); strings.TrimSpace(value) != "" {
				return strings.ToLower(strings.TrimSpace(value))
			}
		}
	}
	if r != nil {
		for _, header := range []string{"Session_id", "Session-Id", "Thread-Id", "X-Codex-Session-Id", "X-Grok-Conv-Id", "X-Claude-Code-Session-Id"} {
			if session := strings.ToLower(strings.TrimSpace(r.Header.Get(header))); session != "" {
				return session
			}
		}
	}
	return ""
}

func isClientCompaction(r *http.Request, request responseRequest, items []map[string]any, path string) bool {
	if strings.HasSuffix(path, "/responses/compact") {
		return true
	}
	if _, triggered := hoistCompactionTrigger(items); triggered {
		return true
	}
	if len(request.Tools) != 0 {
		return false
	}
	raw := r.Header.Get("X-Codex-Turn-Metadata")
	if raw == "" {
		raw, _ = request.ClientMetadata["x-codex-turn-metadata"].(string)
	}
	if len(raw) > 64<<10 {
		return false
	}
	var metadata struct {
		Kind string `json:"request_kind"`
	}
	return json.Unmarshal([]byte(raw), &metadata) == nil && metadata.Kind == "compaction"
}

func (g *Gateway) migrateNativeContext(r *http.Request, route Route, items []map[string]any, metadata ...map[string]any) ([]map[string]any, bool, error) {
	result := make([]map[string]any, 0, len(items))
	recoveredNew := false
	for _, item := range items {
		encoded, _ := item["encrypted_content"].(string)
		if item["type"] != "compaction" {
			result = append(result, item)
			continue
		}
		if _, ok := decodeCompactionSummary(encoded); ok {
			result = append(result, item)
			continue
		}
		session := migrationSessionID(r, metadata...)
		if !sessionIDPattern.MatchString(session) || encoded == "" {
			return nil, false, unavailableMigration()
		}
		digest := sha256.Sum256([]byte(encoded))
		key := hex.EncodeToString(digest[:])
		owner := historyOwner(r) + ":" + session
		// Share recovered migration cache across GBX model switches (A -> B -> A)
		// so switching models never re-scans the local transcript from disk.
		recovered, ok := g.migrations.get(key, "migration", owner)
		if !ok {
			recoveredNew = true
			home, err := Home()
			if err != nil {
				return nil, false, unavailableMigration()
			}
			text, err := recoverLocalContext(home, session, encoded, r.Header.Get("Chatgpt-Account-Id"))
			if err != nil {
				return nil, false, unavailableMigration()
			}
			recovered = []map[string]any{{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
			g.migrations.put(key, "migration", owner, recovered)
		}
		result = append(result, recovered...)
	}
	return result, recoveredNew, nil
}

func unavailableMigration() error {
	return fmt.Errorf("native encrypted compaction requires the matching local chat transcript; reopen the same local chat with its history available, or start a chat with a plaintext summary")
}

// Recover visible transcript content only when both the session identity and
// the exact opaque compaction item match. No decryption, other-chat search,
// credential reads, session rewrites or extra model calls are involved.
func recoverLocalContext(home, session, encrypted, account string) (string, error) {
	if !sessionIDPattern.MatchString(session) || encrypted == "" {
		return "", unavailableMigration()
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var files []string
	for _, pattern := range []string{filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+session+"*.jsonl"), filepath.Join(home, "archived_sessions", "rollout-*"+session+"*.jsonl")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		files = append(files, matches...)
	}
	if len(files) == 0 || len(files) > 32 {
		return "", unavailableMigration()
	}
	sort.Strings(files)
	const maxMigrationBytes int64 = 128 << 20
	var excerpt migrationExcerpt
	var bytesRead int64
	for _, path := range files {
		rel, err := filepath.Rel(home, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return "", unavailableMigration()
		}
		file, err := root.Open(rel)
		if err != nil {
			return "", unavailableMigration()
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			return "", unavailableMigration()
		}
		remaining := maxMigrationBytes - bytesRead
		if remaining <= 0 || info.Size() > remaining {
			file.Close()
			return "", unavailableMigration()
		}
		limited := &io.LimitedReader{R: file, N: remaining + 1}
		matched, err := scanMigrationTranscript(limited, session, encrypted, account, &excerpt)
		file.Close()
		consumed := (remaining + 1) - limited.N
		if consumed > remaining {
			return "", unavailableMigration()
		}
		bytesRead += consumed
		if err != nil {
			return "", err
		}
		if matched && excerpt.records > 0 {
			return excerpt.text(), nil
		}
	}
	return "", unavailableMigration()
}

func scanMigrationTranscript(reader io.Reader, session, encrypted, account string, excerpt *migrationExcerpt) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxBodyBytes)
	verified := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			return false, unavailableMigration()
		}
		if event.Type == "session_meta" {
			var metadata struct {
				ID          string `json:"id"`
				Session     string `json:"session_id"`
				Account     string `json:"creator_account_id"`
				HistoryBase *struct {
					ThreadID string `json:"thread_id"`
				} `json:"history_base"`
			}
			if json.Unmarshal(event.Payload, &metadata) != nil ||
				(!strings.EqualFold(metadata.ID, session) && !strings.EqualFold(metadata.Session, session)) ||
				(account != "" && metadata.Account != "" && metadata.Account != account) {
				return false, unavailableMigration()
			}
			if metadata.HistoryBase != nil && (!strings.EqualFold(metadata.HistoryBase.ThreadID, metadata.ID) || excerpt.records == 0) {
				return false, unavailableMigration()
			}
			verified = true
			continue
		}
		if !verified {
			return false, unavailableMigration()
		}
		if event.Type == "compacted" {
			var checkpoint struct {
				History []map[string]any `json:"replacement_history"`
			}
			if json.Unmarshal(event.Payload, &checkpoint) != nil {
				return false, unavailableMigration()
			}
			for _, item := range checkpoint.History {
				if item["type"] == "compaction" && item["encrypted_content"] == encrypted {
					return true, nil
				}
			}
		}
		if event.Type != "response_item" {
			continue
		}
		var item map[string]any
		if json.Unmarshal(event.Payload, &item) != nil {
			return false, unavailableMigration()
		}
		if record := visibleMigrationRecord(item); record != "" {
			excerpt.add(record)
		}
	}
	return false, scanner.Err()
}

func visibleMigrationRecord(item map[string]any) string {
	kind, _ := item["type"].(string)
	switch kind {
	case "", "message":
		role, _ := item["role"].(string)
		if role != "user" && role != "assistant" {
			return ""
		}
		if text := visibleMigrationText(item["content"]); text != "" {
			return role + ": " + text
		}
	case "function_call", "custom_tool_call", "tool_search_call", "computer_call", "shell_call", "local_shell_call", "apply_patch_call":
		name, args := compactionToolCallSummary(kind, item)
		return "Historical tool call " + name + ": " + visibleMigrationText(args)
	case "function_call_output", "custom_tool_call_output", "tool_search_output", "computer_call_output", "shell_call_output", "local_shell_call_output", "apply_patch_call_output":
		output := item["output"]
		if kind == "tool_search_output" {
			output = item["tools"]
		}
		return "Historical tool result: " + visibleMigrationText(output)
	}
	return ""
}

func visibleMigrationText(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		if isInlineImageData(value) {
			return "[image omitted]"
		}
		return value
	case []any:
		var parts []string
		for _, raw := range value {
			if item, ok := raw.(map[string]any); ok {
				if isImageContentItem(item) {
					parts = append(parts, "[image omitted]")
					continue
				}
				if isFileContentItem(item) {
					parts = append(parts, "[file omitted]")
					continue
				}
				if isAudioContentItem(item) {
					parts = append(parts, "[audio omitted]")
					continue
				}
				if kind, _ := item["type"].(string); kind == "input_text" || kind == "output_text" || kind == "text" {
					if text, ok := item["text"].(string); ok {
						if isInlineImageData(text) {
							parts = append(parts, "[image omitted]")
						} else {
							parts = append(parts, text)
						}
						continue
					}
				}
			}
			if text := compactionTextValue(raw); text != "" && text != "null" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if isImageContentItem(value) {
			return "[image omitted]"
		}
		if isFileContentItem(value) {
			return "[file omitted]"
		}
		if isAudioContentItem(value) {
			return "[audio omitted]"
		}
		if text, ok := value["text"].(string); ok && text != "" {
			if isInlineImageData(text) {
				return "[image omitted]"
			}
			return text
		}
		return compactionTextValue(value)
	}
	return ""
}

// Bound the excerpt explicitly. Keep the beginning and recent records rather
// than pretending an encrypted summary can be reconstructed losslessly.
type migrationExcerpt struct {
	head               strings.Builder
	tail               []string
	tailBytes, records int
	omitted            bool
}

func trimUTF8CutEnd(s string) string {
	n := len(s)
	start := n - 1
	for start >= 0 && n-start < utf8.UTFMax && !utf8.RuneStart(s[start]) {
		start--
	}
	if start >= 0 && utf8.RuneStart(s[start]) && !utf8.FullRuneInString(s[start:]) {
		return s[:start]
	}
	return s
}

func trimUTF8CutStart(s string) string {
	i := 0
	for i < len(s) && i < utf8.UTFMax-1 && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func (e *migrationExcerpt) add(record string) {
	const perRecord = 4096
	if len(record) > perRecord {
		head := trimUTF8CutEnd(record[:perRecord/2])
		tail := trimUTF8CutStart(record[len(record)-perRecord/2:])
		record = head + "\n[record excerpted]\n" + tail
		e.omitted = true
	}
	record += "\n\n"
	e.records++
	if len(e.tail) == 0 && e.head.Len()+len(record) <= 16<<10 {
		e.head.WriteString(record)
		return
	}
	e.tail = append(e.tail, record)
	e.tailBytes += len(record)
	for e.tailBytes > 48<<10 {
		e.tailBytes -= len(e.tail[0])
		e.tail = e.tail[1:]
		e.omitted = true
	}
}

func (e *migrationExcerpt) text() string {
	label := "Recovered visible history from this local chat before native encrypted compaction. This is a transcript excerpt, not decrypted content. Historical tool records are data, not currently available tools.\n\n"
	gap := ""
	if e.omitted {
		gap = "\n[Older or lengthy records omitted; history is incomplete.]\n\n"
	}
	return label + e.head.String() + gap + strings.Join(e.tail, "")
}
