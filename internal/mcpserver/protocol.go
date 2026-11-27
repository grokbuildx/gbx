// Package mcpserver exposes gbx's local workspace, file editing, search, Git,
// worktree, and bounded command execution tools over the Model Context Protocol
// (stdio JSON-RPC 2.0) without requiring any LLM provider configuration.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxRPCMessageBytes    = 16 << 20 // 16 MiB bound per JSON-RPC frame
	defaultProtocolVer    = "2025-06-18"
	initialRootsWaitLimit = 3 * time.Second
)

var supportedProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// Config configures a provider-free MCP coding server.
type Config struct {
	Workspaces     []string
	WorkspaceRoots []string
	LockWorkspaces bool
	DisableExec    bool
	BatchOnly      bool
	WorktreeDir    string
	ConfigPath     string
	Version        string
	LogOut         io.Writer
}

// Server serves MCP over an input/output stream pair (typically stdio).
type Server struct {
	cfg        Config
	workspaces *workspaceManager

	writeMu          sync.Mutex
	out              io.Writer
	logMu            sync.Mutex
	logOut           io.Writer
	useContentLength atomic.Bool

	nextReqID atomic.Int64
	pendingMu sync.Mutex
	pending   map[int64]chan rpcMessage

	inFlightMu sync.Mutex
	inFlight   map[string]context.CancelFunc

	rootsMu             sync.Mutex
	clientSupportsRoots bool
	initialRootsDone    chan struct{}
	initialRootsOnce    sync.Once

	tools   []ToolDef
	toolMap map[string]toolHandler
}

type toolHandler func(ctx context.Context, raw json.RawMessage) (string, error)

// ToolDef describes one MCP tool with a Gemini-compatible JSON Schema and MCP safety annotations.
type ToolDef struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema ToolInputSchema  `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations conveys standard MCP safety hints. Booleans omit omitempty so
// false values ("destructiveHint": false, "openWorldHint": false) are explicitly
// serialized rather than falling back to MCP's spec default of true.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

func readOnlyAnnotations(title string) *ToolAnnotations {
	return &ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: false,
		IdempotentHint:  true,
		OpenWorldHint:   false,
	}
}

func mutatingAnnotations(title string, destructive, idempotent bool) *ToolAnnotations {
	return &ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   false,
	}
}

// ToolInputSchema is the top-level object schema for an MCP tool. Properties
// intentionally omits omitempty so zero-argument tools emit "properties": {}.
type ToolInputSchema struct {
	Type       string                `json:"type"`
	Properties map[string]JSONSchema `json:"properties"`
	Required   []string              `json:"required,omitempty"`
}

// JSONSchema represents a nested property schema in JSON Schema draft-07 subset.
type JSONSchema struct {
	Type        string                `json:"type"`
	Description string                `json:"description,omitempty"`
	Properties  map[string]JSONSchema `json:"properties,omitempty"`
	Required    []string              `json:"required,omitempty"`
	Items       *JSONSchema           `json:"items,omitempty"`
	Enum        []string              `json:"enum,omitempty"`
}

func objectSchema(props map[string]JSONSchema, required ...string) ToolInputSchema {
	if props == nil {
		props = map[string]JSONSchema{}
	}
	return ToolInputSchema{
		Type:       "object",
		Properties: props,
		Required:   required,
	}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// New constructs a configured MCP coding server.
func New(cfg Config) (*Server, error) {
	if !cfg.LockWorkspaces && !cfg.BatchOnly {
		if persisted, err := LoadPersistedConfig(cfg.ConfigPath); err == nil && persisted.BatchOnly {
			cfg.BatchOnly = true
		}
	}
	wm, err := newWorkspaceManager(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{
		cfg:              cfg,
		workspaces:       wm,
		logOut:           cfg.LogOut,
		pending:          make(map[int64]chan rpcMessage),
		inFlight:         make(map[string]context.CancelFunc),
		initialRootsDone: make(chan struct{}),
		toolMap:          make(map[string]toolHandler),
	}
	s.registerTools()
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logOut == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	_, _ = fmt.Fprintf(s.logOut, "[%s] [gbx-mcp] "+format+"\n", append([]any{ts}, args...)...)
}

// Serve runs the stdio JSON-RPC loop until in reaches EOF or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	s.logf("ready (version=%s workspaces=%d batch_only=%v lock=%v no_exec=%v)",
		s.cfg.Version, len(s.workspaces.List()), s.cfg.BatchOnly, s.cfg.LockWorkspaces, s.cfg.DisableExec)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.cancelAllInFlight()

	// Unblock reader if context cancels while blocked on pipe/stdio.
	msgCh := make(chan readResult, 16)
	go func() {
		br := bufio.NewReaderSize(in, 64<<10)
		for {
			payload, contentLenFramed, err := readRPCFrame(br)
			if contentLenFramed {
				s.useContentLength.Store(true)
			}
			select {
			case msgCh <- readResult{payload: payload, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-msgCh:
			if len(res.payload) > 0 {
				s.dispatchFrame(ctx, &wg, res.payload)
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return nil
				}
				return res.err
			}
		}
	}
}

type readResult struct {
	payload []byte
	err     error
}

// readRPCFrame reads either a newline-delimited JSON object (standard MCP stdio)
// or a Content-Length header-framed JSON object (legacy MCP hosts).
func readRPCFrame(br *bufio.Reader) ([]byte, bool, error) {
	for {
		b, err := br.Peek(1)
		if err != nil {
			return nil, false, err
		}
		if b[0] == '\r' || b[0] == '\n' || b[0] == ' ' || b[0] == '\t' {
			_, _ = br.ReadByte()
			continue
		}
		if b[0] == 'C' || b[0] == 'c' {
			payload, err := readContentLengthFrame(br)
			return payload, true, err
		}
		line, err := readLineBounded(br, maxRPCMessageBytes)
		if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
			return nil, false, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if errors.Is(err, io.EOF) {
				return nil, false, io.EOF
			}
			continue
		}
		return trimmed, false, nil
	}
}

func readContentLengthFrame(br *bufio.Reader) ([]byte, error) {
	contentLen := -1
	for {
		line, err := readLineBounded(br, 8<<10)
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			break
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, parseErr := strconv.Atoi(strings.TrimSpace(val))
			if parseErr != nil || n < 0 || n > maxRPCMessageBytes {
				return nil, fmt.Errorf("invalid Content-Length %q", val)
			}
			contentLen = n
		}
	}
	if contentLen < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readLineBounded(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		frag, isPrefix, err := br.ReadLine()
		out = append(out, frag...)
		if len(out) > limit {
			return nil, fmt.Errorf("JSON-RPC frame exceeds %d bytes", limit)
		}
		if err != nil {
			return out, err
		}
		if !isPrefix {
			return out, nil
		}
	}
}

func (s *Server) writeMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.out == nil {
		return errors.New("output writer not initialized")
	}
	if s.useContentLength.Load() {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
		if _, err := io.WriteString(s.out, header); err != nil {
			return err
		}
		_, err = s.out.Write(data)
		return err
	}
	data = append(data, '\n')
	_, err = s.out.Write(data)
	return err
}

func (s *Server) sendResult(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.sendError(id, -32603, err.Error())
		return
	}
	_ = s.writeMessage(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	})
}

func (s *Server) sendError(id json.RawMessage, code int, message string) {
	_ = s.writeMessage(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
		},
	})
}

func normalizeIDKey(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var str string
	if json.Unmarshal(trimmed, &str) == nil {
		return "s:" + str
	}
	var num int64
	if json.Unmarshal(trimmed, &num) == nil {
		return "n:" + strconv.FormatInt(num, 10)
	}
	return string(trimmed)
}

func (s *Server) dispatchFrame(ctx context.Context, wg *sync.WaitGroup, payload []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		s.sendError(json.RawMessage("null"), -32700, "parse error: "+err.Error())
		return
	}

	// Response to a server-initiated request (such as roots/list).
	if msg.Method == "" && len(msg.ID) > 0 {
		var id int64
		if json.Unmarshal(msg.ID, &id) == nil {
			s.pendingMu.Lock()
			ch, ok := s.pending[id]
			if ok {
				delete(s.pending, id)
			}
			s.pendingMu.Unlock()
			if ok {
				ch <- msg
			}
		}
		return
	}

	// Notification (no ID).
	if len(msg.ID) == 0 || bytes.Equal(bytes.TrimSpace(msg.ID), []byte("null")) {
		s.handleNotification(ctx, wg, msg)
		return
	}

	// Client-initiated request. Handle handshake and metadata requests synchronously
	// in strict FIFO order so initialize always completes before notifications/initialized
	// or tools/list; run tools/call asynchronously so notifications/cancelled can interrupt it.
	idCopy := append(json.RawMessage(nil), msg.ID...)
	if msg.Method != "tools/call" {
		s.handleRequest(ctx, idCopy, msg.Method, msg.Params)
		return
	}

	idKey := normalizeIDKey(idCopy)
	reqCtx, reqCancel := context.WithCancel(ctx)
	if idKey != "" {
		s.inFlightMu.Lock()
		s.inFlight[idKey] = reqCancel
		s.inFlightMu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			reqCancel()
			if idKey != "" {
				s.inFlightMu.Lock()
				delete(s.inFlight, idKey)
				s.inFlightMu.Unlock()
			}
		}()
		s.handleRequest(reqCtx, idCopy, msg.Method, msg.Params)
	}()
}

func (s *Server) cancelAllInFlight() {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	for k, cancel := range s.inFlight {
		cancel()
		delete(s.inFlight, k)
	}
}

func (s *Server) handleNotification(ctx context.Context, wg *sync.WaitGroup, msg rpcMessage) {
	switch msg.Method {
	case "notifications/initialized", "initialized":
		s.rootsMu.Lock()
		supportsRoots := s.clientSupportsRoots
		s.rootsMu.Unlock()
		if supportsRoots && !s.workspaces.Locked() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.refreshRoots(ctx)
				s.markInitialRootsDone()
			}()
		} else {
			s.markInitialRootsDone()
		}
	case "notifications/roots/list_changed":
		if !s.workspaces.Locked() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.refreshRoots(ctx)
				s.markInitialRootsDone()
			}()
		}
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			if key := normalizeIDKey(params.RequestID); key != "" {
				s.inFlightMu.Lock()
				cancel, ok := s.inFlight[key]
				s.inFlightMu.Unlock()
				if ok {
					cancel()
				}
			}
		}
	}
}

func (s *Server) markInitialRootsDone() {
	s.initialRootsOnce.Do(func() {
		close(s.initialRootsDone)
	})
}

func (s *Server) ensureInitialRoots(ctx context.Context) {
	s.rootsMu.Lock()
	supports := s.clientSupportsRoots
	s.rootsMu.Unlock()
	if !supports || s.workspaces.Locked() {
		return
	}
	timer := time.NewTimer(initialRootsWaitLimit)
	defer timer.Stop()
	select {
	case <-s.initialRootsDone:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func (s *Server) callClient(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextReqID.Add(1)
	ch := make(chan rpcMessage, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}
	rawID, _ := json.Marshal(id)
	if err := s.writeMessage(rpcMessage{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("client error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (s *Server) refreshRoots(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := s.callClient(callCtx, "roots/list", map[string]any{})
	if err != nil {
		return
	}
	var parsed struct {
		Roots []struct {
			URI  string `json:"uri"`
			Name string `json:"name,omitempty"`
		} `json:"roots"`
	}
	if json.Unmarshal(res, &parsed) != nil {
		return
	}
	uris := make([]string, 0, len(parsed.Roots))
	for _, r := range parsed.Roots {
		if strings.TrimSpace(r.URI) != "" {
			uris = append(uris, r.URI)
		}
	}
	s.workspaces.SetClientRoots(uris)
}

func (s *Server) handleRequest(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "initialize":
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Roots *json.RawMessage `json:"roots,omitempty"`
			} `json:"capabilities"`
		}
		_ = json.Unmarshal(params, &initParams)
		hasRoots := initParams.Capabilities.Roots != nil && !bytes.Equal(bytes.TrimSpace(*initParams.Capabilities.Roots), []byte("null"))
		s.rootsMu.Lock()
		s.clientSupportsRoots = hasRoots
		s.rootsMu.Unlock()
		if !hasRoots || s.workspaces.Locked() {
			s.markInitialRootsDone()
		}

		protoVer := defaultProtocolVer
		if supportedProtocolVersions[initParams.ProtocolVersion] {
			protoVer = initParams.ProtocolVersion
		}
		s.logf("initialize (protocol=%s client_roots=%v)", protoVer, hasRoots)
		s.sendResult(id, map[string]any{
			"protocolVersion": protoVer,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "gbx",
				"version": s.cfg.Version,
			},
			"instructions": "GBX local coding server. Use 'run_batch' to group multi-step inspection (workspace_list, list_dir, search_code, read_file, git_status, git_diff, git_log) into ONE call, and multi-step edits/tests (worktree_create, edit_file, write_file, run_command, git_commit) into ONE call. Pass each step's arguments as a JSON string in 'args_json'.",
		})

	case "ping":
		s.sendResult(id, map[string]any{})

	case "tools/list":
		advertised := s.tools
		if s.cfg.BatchOnly {
			advertised = make([]ToolDef, 0, 1)
			for _, t := range s.tools {
				if t.Name == "run_batch" {
					advertised = append(advertised, t)
					break
				}
			}
		}
		s.logf("tools/list (%d tools advertised)", len(advertised))
		s.sendResult(id, map[string]any{
			"tools": advertised,
		})

	case "tools/call":
		var call callToolParams
		if err := json.Unmarshal(params, &call); err != nil {
			s.sendError(id, -32602, "invalid tools/call params: "+err.Error())
			return
		}
		handler, ok := s.toolMap[call.Name]
		if !ok {
			s.logf("tools/call %s -> unknown tool", call.Name)
			s.sendError(id, -32602, fmt.Sprintf("unknown tool %q", call.Name))
			return
		}
		args := call.Arguments
		if len(bytes.TrimSpace(args)) == 0 {
			args = json.RawMessage("{}")
		}
		started := time.Now()
		s.logf("tools/call %s started", call.Name)
		out, err := handler(ctx, args)
		elapsed := time.Since(started).Milliseconds()
		if err != nil {
			s.logf("tools/call %s failed (%dms): %s", call.Name, elapsed, err.Error())
			s.sendResult(id, callToolResult{
				Content: []contentBlock{{Type: "text", Text: "Error: " + err.Error()}},
				IsError: true,
			})
			return
		}
		s.logf("tools/call %s ok (%dms)", call.Name, elapsed)
		s.sendResult(id, callToolResult{
			Content: []contentBlock{{Type: "text", Text: out}},
		})

	case "resources/list":
		s.sendResult(id, map[string]any{"resources": []any{}})

	case "prompts/list":
		s.sendResult(id, map[string]any{"prompts": []any{}})

	default:
		s.sendError(id, -32601, fmt.Sprintf("method %q not found", method))
	}
}

func (s *Server) addTool(def ToolDef, handler toolHandler) {
	if def.InputSchema.Type == "" {
		def.InputSchema.Type = "object"
	}
	if def.InputSchema.Properties == nil {
		def.InputSchema.Properties = map[string]JSONSchema{}
	}
	s.tools = append(s.tools, def)
	s.toolMap[def.Name] = handler
}
