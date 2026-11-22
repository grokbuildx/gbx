package oauthproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grokbuildx/gbx/internal/modelsdev"
	"github.com/grokbuildx/gbx/internal/protocol"
)

// Cursor's agent API is a bidirectional Connect stream: GBX sends one Run
// request and then keeps writing on the same request body while reading
// interactions, exec requests and KV lookups back. The local adapter speaks OpenAI
// Responses, so this runtime translates in both directions and, when Cursor asks
// for a tool, parks the open stream so the tool result can continue it on the
// next request instead of replaying the whole conversation.

const (
	cursorConnectRunPath    = "/agent.v1.AgentService/Run"
	cursorConnectModelsPath = "/agent.v1.AgentService/GetUsableModels"
	cursorCLIVersion        = "cli-2026.02.13-41ac335"

	cursorMaxInboundRequestBytes = int64(128 << 20)
	cursorMaxUpstreamErrorBytes  = int64(1 << 20)
	cursorMaxBlobs               = 512
	cursorHeartbeatInterval      = 20 * time.Second
	// cursorParkedLifetime bounds how long a stream waits for the tool result
	// that resumes it before it is closed and the next turn starts fresh.
	cursorParkedLifetime = 30 * time.Minute
)

var (
	// cursorConnectBaseURL is the agent data plane. A var so tests can point it
	// at a stub.
	cursorConnectBaseURL = "https://api2.cursor.sh"
	// cursorFallbackModels is the compatibility list used when the account's
	// catalog cannot be read. Cursor publishes no stable catalog endpoint, so a
	// failed discovery must not leave gbx with no models at all.
	cursorFallbackModels = []string{
		"composer-2", "claude-4-sonnet", "claude-3.5-sonnet", "gpt-4o", "cursor-small", "gemini-2.5-pro",
	}
)

type cursorOAuthAuthorizer struct {
	path   string
	client *http.Client
}

type cursorOAuthCredential struct {
	metadata     map[string]any
	accessToken  string
	refreshToken string
	email        string
	expiresAt    time.Time
	disabled     bool
}

// authorize returns a usable access token, refreshing it under the credential
// lock when it is missing or expired. force only refreshes when the rejected
// token is still the current one, so a delayed 401 cannot refresh twice.
func (a *cursorOAuthAuthorizer) authorize(ctx context.Context, force bool) (string, error) {
	release, err := lockOAuthCredential(ctx, a.path)
	if err != nil {
		return "", err
	}
	defer release()
	credential, err := a.load()
	if err != nil {
		return "", err
	}
	if credential.disabled {
		return "", fmt.Errorf("Cursor credential %s is disabled", filepath.Base(a.path))
	}
	force = force && oauthTokenStillRejected(ctx, credential.accessToken)
	if force || credential.accessToken == "" || (!credential.expiresAt.IsZero() && time.Now().Add(time.Minute).After(credential.expiresAt)) {
		credential, err = a.refresh(ctx, credential)
		if err != nil {
			return "", err
		}
	}
	return credential.accessToken, nil
}

func (*cursorOAuthAuthorizer) isOAuth() bool { return true }

func (a *cursorOAuthAuthorizer) listAuths() []*AuthInfo {
	credential, err := a.load()
	if err != nil {
		return nil
	}
	status := StatusActive
	if credential.disabled {
		status = StatusDisabled
	}
	return []*AuthInfo{{
		ID: filepath.Base(a.path), Provider: ProviderCursor, FileName: a.path, Label: credential.email,
		Status: status, Disabled: credential.disabled, Metadata: safeAuthMetadata(credential.metadata),
	}}
}

func (a *cursorOAuthAuthorizer) load() (*cursorOAuthCredential, error) {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Cursor credentials found; run `gbx oauth cursor` first")
		}
		return nil, fmt.Errorf("read Cursor credential %s: %w", filepath.Base(a.path), err)
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("decode Cursor credential %s: %w", filepath.Base(a.path), err)
	}
	credentialType := strings.ToLower(strings.TrimSpace(stringValue(metadata["type"])))
	if credentialType != ProviderCursor {
		return nil, fmt.Errorf("credential %s is type %q, not Cursor", filepath.Base(a.path), credentialType)
	}
	disabled, _ := metadata["disabled"].(bool)
	email := firstMetadataString(metadata, "email")
	if email == "" {
		// The credential file carries no display label for every account shape;
		// fall back to the token's own claims without ever rewriting the file.
		_, email = cursorJWTIdentity(firstMetadataString(metadata, "access_token"))
	}
	return &cursorOAuthCredential{
		metadata: metadata, accessToken: firstMetadataString(metadata, "access_token"),
		refreshToken: firstMetadataString(metadata, "refresh_token"), email: email,
		expiresAt: parseCodexExpiry(firstMetadataString(metadata, "expired")), disabled: disabled,
	}, nil
}

// refresh exchanges the refresh token for a new access token. A refused refresh
// that means the credential is dead is terminal and is never persisted: the
// file keeps its last known state so the next launch reports the same need to
// sign in again.
func (a *cursorOAuthAuthorizer) refresh(ctx context.Context, credential *cursorOAuthCredential) (*cursorOAuthCredential, error) {
	if credential.refreshToken == "" {
		return nil, fmt.Errorf("Cursor credential %s has no refresh token; run `gbx oauth cursor` again", filepath.Base(a.path))
	}
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     cursorOAuthClientID,
		"refresh_token": credential.refreshToken,
	})
	if err != nil {
		return nil, err
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, cursorOAuthToken, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := doCredentialRequest(a.client, request)
	if err != nil {
		return nil, fmt.Errorf("refresh Cursor token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cursorMaxErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Cursor token refresh: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if cursorShouldLogout(response.StatusCode, body) {
			return nil, fmt.Errorf("Cursor credential %s was rejected (HTTP %d); run `gbx oauth cursor` again",
				filepath.Base(a.path), response.StatusCode)
		}
		return nil, fmt.Errorf("refresh Cursor token: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var tokens struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		// Cursor has answered with snake_case in some deployments; accept both.
		AccessTokenSnake  string `json:"access_token"`
		RefreshTokenSnake string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("decode Cursor token refresh: %w", err)
	}
	accessToken := firstNonEmptyString(tokens.AccessToken, tokens.AccessTokenSnake)
	if accessToken == "" {
		return nil, fmt.Errorf("refresh Cursor token: response has no access token")
	}
	credential.accessToken = accessToken
	if refresh := firstNonEmptyString(tokens.RefreshToken, tokens.RefreshTokenSnake); refresh != "" {
		credential.refreshToken = refresh
	}
	credential.expiresAt = cursorTokenExpiry(credential.accessToken)
	credential.metadata["type"] = ProviderCursor
	credential.metadata["access_token"] = credential.accessToken
	credential.metadata["refresh_token"] = credential.refreshToken
	if sub, email := cursorJWTIdentity(credential.accessToken); email != "" {
		credential.email = email
		credential.metadata["email"] = email
		_ = sub
	}
	if !credential.expiresAt.IsZero() {
		credential.metadata["expired"] = credential.expiresAt.UTC().Format(time.RFC3339)
	}
	credential.metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeCodexCredentialAtomic(a.path, credential.metadata); err != nil {
		return nil, err
	}
	LogInfof("credential refreshed component=cursor credential_file=%s expires_at=%s",
		filepath.Base(a.path), credential.expiresAt.UTC().Format(time.RFC3339))
	return credential, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// cursorUpstreamError carries a non-2xx agent answer, including the Retry-After
// hint, so the shared fast-retry loop and the client can both act on it.
type cursorUpstreamError struct {
	status     int
	body       string
	retryAfter string
	headers    http.Header
}

func (e *cursorUpstreamError) Error() string {
	message := strings.TrimSpace(e.body)
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("Cursor upstream returned HTTP %d: %s", e.status, message)
}

func (e *cursorUpstreamError) upstreamStatus() int          { return e.status }
func (e *cursorUpstreamError) upstreamRetryAfter() string   { return e.retryAfter }
func (e *cursorUpstreamError) upstreamErrorBody() string    { return e.body }
func (e *cursorUpstreamError) upstreamHeaders() http.Header { return e.headers }

type cursorPendingCall struct {
	msgID  uint32
	execID string
}

// cursorBeat is one round of keep-alive ticks. A stream is read by exactly one
// goroutine at a time: the beating goroutine owns it while the stream is
// parked, the request goroutine owns it while a turn runs.
type cursorBeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func (b *cursorBeat) cancel() {
	if b == nil {
		return
	}
	b.once.Do(func() { close(b.stop) })
}

type cursorSession struct {
	response   *http.Response
	reader     *bufio.Reader
	writer     cursorStreamWriter
	cancel     context.CancelFunc
	sessionKey string
	systemHash string
	token      string
	initDone   chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	pending map[string]cursorPendingCall
	beat    *cursorBeat
}

// cursorStreamWriter is the request body side of a Connect stream: the pipe a
// live session writes to, or a test double.
type cursorStreamWriter interface {
	io.Writer
	CloseWithError(error) error
}

func newCursorSession(response *http.Response, writer cursorStreamWriter, cancel context.CancelFunc) *cursorSession {
	session := &cursorSession{
		response: response, writer: writer, cancel: cancel,
		pending: make(map[string]cursorPendingCall),
	}
	if response != nil && response.Body != nil {
		// One reader for the whole stream: a resumed turn must not lose frames
		// the previous turn already buffered.
		session.reader = bufio.NewReader(response.Body)
	}
	return session
}

// send writes one Connect frame on the request body without holding s.mu across
// blocking pipe I/O so close() can always abort a stalled writer.
func (s *cursorSession) send(frame []byte) error {
	if s.initDone != nil {
		<-s.initDone
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("Cursor stream is closed")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("Cursor stream is closed")
	}
	_, err := s.writer.Write(frame)
	return err
}

func (s *cursorSession) addPending(callID string, call cursorPendingCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[callID] = call
}

func (s *cursorSession) takePending(callID string) (cursorPendingCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.pending[callID]
	if ok {
		delete(s.pending, callID)
	}
	return call, ok
}

func (s *cursorSession) matches(callIDs []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return false
	}
	answered := make(map[string]struct{}, len(callIDs))
	for _, id := range callIDs {
		answered[id] = struct{}{}
	}
	for id := range s.pending {
		if _, ok := answered[id]; !ok {
			return false
		}
	}
	return true
}

// startHeartbeat keeps the parked stream warm while the client runs the tool.
// The beating goroutine owns the stream until handoff, so the request goroutine
// is the only reader while a turn runs.
func (s *cursorSession) startHeartbeat() {
	s.mu.Lock()
	if s.closed || s.beat != nil {
		s.mu.Unlock()
		return
	}
	beat := &cursorBeat{stop: make(chan struct{}), done: make(chan struct{})}
	s.beat = beat
	s.mu.Unlock()
	go func() {
		defer close(beat.done)
		ticker := time.NewTicker(cursorHeartbeatInterval)
		defer ticker.Stop()
		for ticks := 1; ; ticks++ {
			select {
			case <-beat.stop:
				return
			case <-ticker.C:
				if ticks > int(cursorParkedLifetime/cursorHeartbeatInterval) {
					// the client abandoned the tool call; do not keep the
					// upstream stream open forever.
					s.close()
					return
				}
				if err := s.send(cursorFrameConnect(encodeCursorHeartbeat(), 0)); err != nil {
					return
				}
			}
		}
	}()
}

// handoff stops the heartbeat and waits for it. Only the request path calls it,
// so a request never reads a stream another goroutine is still using.
func (s *cursorSession) handoff() {
	s.mu.Lock()
	beat := s.beat
	s.beat = nil
	s.mu.Unlock()
	beat.cancel()
	if beat != nil {
		<-beat.done
	}
}

// close tears the stream down without waiting for the heartbeat: cancellation
// must stay cheap on the launch path (Close/Stop never block on it).
func (s *cursorSession) close() {
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	beat := s.beat
	s.beat = nil
	s.mu.Unlock()
	beat.cancel()
	if closed {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	_ = s.writer.CloseWithError(fmt.Errorf("cursor stream closed"))
	if s.response != nil && s.response.Body != nil {
		_ = s.response.Body.Close()
	}
}

const (
	cursorMaxParkedSessions = 16
	cursorMaxBlobBytes      = 64 << 20
)

type cursorService struct {
	apiKey       string
	models       []string
	modelDetails map[string]protocol.ModelInfo
	modelRoute   map[string]string
	authorizer   *cursorOAuthAuthorizer
	client       *http.Client
	usage        *UsageTracker
	ctx          context.Context

	mu        sync.Mutex
	parked    []*cursorSession
	blobs     map[string][]byte
	blobOrder []string
	blobBytes int
}

func (s *cursorService) handler() http.Handler {
	return loopbackFrontMuxForContext(s.ctx, s.apiKey, s.handleModels, s.handleResponses)
}

func (s *cursorService) authorized(request *http.Request) bool {
	return loopbackAuthorized(request, s.apiKey)
}

func (s *cursorService) handleModels(writer http.ResponseWriter, request *http.Request) {
	serveModelListWithRoutes(writer, request, s.apiKey, s.models, s.modelDetails, s.modelRoute)
}

func (s *cursorService) handleResponses(writer http.ResponseWriter, request *http.Request) {
	requestCtx, requestID := withRequestLogID(request.Context())
	started := time.Now()
	if !s.authorized(request) {
		LogWarnEvent("request_rejected", "component", "cursor", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusUnauthorized, "reason", "invalid_local_api_key")
		writeProxyErrorDetail(writer, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "", "Invalid API key")
		return
	}
	if request.Method != http.MethodPost {
		writeProxyError(writer, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	raw, err := readInboundBody(writer, request, cursorMaxInboundRequestBytes)
	if err != nil {
		writeInboundBodyError(writer, err)
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", "invalid Responses request: request body is empty")
		return
	}
	converted, err := convertResponsesToCursor(raw)
	if err != nil {
		LogWarnEvent("request_rejected", "component", "cursor", "request_id", requestID,
			"path", request.URL.Path, "status", http.StatusBadRequest, "reason", "request_conversion", "error", err)
		writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if converted.sessionKey == "" {
		if value := extractClientSessionID(request); value != "" {
			converted.sessionKey = value
			converted.conversationID = kiroConversationID(value)
		} else if converted.anonSessionKey != "" {
			converted.sessionKey = converted.anonSessionKey
			converted.conversationID = kiroConversationID(converted.anonSessionKey)
		}
	}
	var gate *cursorGateWriter
	var assemblerWriter http.ResponseWriter
	if converted.stream {
		gate = &cursorGateWriter{response: writer}
		assemblerWriter = gate
	}
	assembler, err := s.serve(requestCtx, converted, assemblerWriter, gate)
	if err != nil {
		if errors.Is(err, context.Canceled) && request.Context().Err() != nil {
			if assembler != nil && ((converted.stream && gate != nil && gate.opened()) || !converted.stream) {
				s.recordUsage(converted, assembler)
			}
			LogDebugEvent("request_canceled", "component", "cursor", "request_id", requestID,
				"model", converted.clientModel, "stream", converted.stream, "duration", logDuration(started))
			return
		}
		norm, headers, retryAfter := cursorNormalizedError(err)
		LogErrorEvent("request_failed", "component", "cursor", "request_id", requestID,
			"model", converted.clientModel, "returned_status", norm.Status, "stream", converted.stream,
			"duration", logDuration(started), "error", err)
		if converted.stream && gate != nil && gate.opened() {
			// The message already started; a status line can no longer carry the
			// failure, so report it as a stream error exactly like an upstream drop.
			if assembler != nil {
				s.recordUsage(converted, assembler)
				assembler.fail(firstNonEmptyString(norm.Code, norm.Type, "upstream_stream_error"), norm.Message)
			}
			return
		}
		copyProxyResponseHeaders(writer.Header(), headers)
		if retryAfter != "" {
			writer.Header().Set("Retry-After", retryAfter)
		}
		writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
		return
	}
	s.recordUsage(converted, assembler)
	if converted.stream {
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(assembler.response())
	LogDebugEvent("request_complete", "component", "cursor", "request_id", requestID,
		"model", converted.clientModel, "status", http.StatusOK, "duration", logDuration(started))
}

func (s *cursorService) recordUsage(converted *cursorConvertedRequest, assembler *responseAssembler) {
	if s.usage == nil || assembler == nil {
		return
	}
	input, output := assembler.tokenTotals()
	s.usage.Add(converted.clientModel, int64(input), int64(output), int64(assembler.cacheReadTokens), int64(assembler.cacheWriteTokens))
}

func cursorNormalizedError(err error) (normalizedProxyError, http.Header, string) {
	var authErr *oauthAuthError
	if errors.As(err, &authErr) {
		status := authErr.status
		if status == 0 {
			status = http.StatusUnauthorized
		}
		return normalizedProxyError{
			Status:  status,
			Type:    proxyErrorType(status),
			Code:    "invalid_api_key",
			Message: truncateErrorMessage(authErr.Error(), maxClientErrorMessageBytes),
		}, nil, ""
	}
	var upstream *cursorUpstreamError
	if errors.As(err, &upstream) {
		status := upstream.status
		if status < 400 {
			status = http.StatusBadGateway
		}
		return normalizeUpstreamError("cursor", status, upstream.body, upstream.Error()), upstream.headers, upstream.retryAfter
	}
	var connectErr *cursorConnectError
	if errors.As(err, &connectErr) {
		status := connectErr.upstreamStatus()
		if status < 400 {
			status = http.StatusBadGateway
		}
		rawBody := connectErr.Message
		if connectErr.Code != "" {
			if encoded, marshalErr := json.Marshal(map[string]string{"code": connectErr.Code, "message": connectErr.Message}); marshalErr == nil {
				rawBody = string(encoded)
			}
		}
		return normalizeUpstreamError("cursor", status, rawBody, connectErr.Error()), nil, ""
	}
	status, _ := cursorClientError(err)
	return normalizeUpstreamError("cursor", status, "", err.Error()), nil, ""
}

// cursorClientError maps a turn failure onto the status and proxy error
// type the client understands: 429 stays 429 so it backs off, 401 so it
// re-authenticates, 413 so it shrinks the request.
func cursorClientError(err error) (int, string) {
	var authErr *oauthAuthError
	if errors.As(err, &authErr) {
		status := authErr.status
		if status == 0 {
			status = http.StatusUnauthorized
		}
		return status, proxyErrorType(status)
	}
	status := 0
	var upstream *cursorUpstreamError
	if errors.As(err, &upstream) {
		status = upstream.status
	}
	var connectErr *cursorConnectError
	if status == 0 && errors.As(err, &connectErr) {
		status = connectErr.upstreamStatus()
	}
	switch {
	case status >= 400 && status < 500:
		return status, proxyErrorType(status)
	case status >= 500:
		return status, proxyErrorType(status)
	default:
		return http.StatusBadGateway, "api_error"
	}
}

// serve runs one Responses turn: resume the parked Cursor stream when the
// client is answering a tool call it owns, otherwise start a fresh Run.
func (s *cursorService) serve(ctx context.Context, converted *cursorConvertedRequest, assemblerWriter http.ResponseWriter, gate *cursorGateWriter) (*responseAssembler, error) {
	// Cursor's ExecMcpResult protobuf frame carries tool text only. When the
	// current turn attaches new screenshots, adds steering user text, or changes
	// the system prompt, close any parked stream for this conversation and start
	// a fresh Run so the full updated context reaches Cursor.
	if converted.hasCurrentImages || converted.current != "" {
		s.closeParkedForSession(converted.sessionKey)
	} else if session := s.takeParked(converted.sessionKey, converted.resumeToolUseIDs()); session != nil {
		if session.systemHash != "" && converted.systemHash != "" && session.systemHash != converted.systemHash {
			session.close()
		} else {
			// A resumed stream may emit another tool call and re-park itself.
			// Release only if it is not parked again when continueTurn returns.
			defer s.releaseOwned(session)
			LogDebugEvent("cursor_resume", "call_count", len(converted.resume))
			assembler := newResponseAssembler(&converted.adapterRequest, assemblerWriter)
			return assembler, s.continueTurn(ctx, session, converted, assembler, gate, true)
		}
	}
	refreshed401 := false
	fastRetryIdx := 0
	for {
		gate.reset()
		assembler := newResponseAssembler(&converted.adapterRequest, assemblerWriter)
		session, err := s.startRun(ctx, converted)
		if err != nil {
			return assembler, err
		}
		err = s.continueTurn(ctx, session, converted, assembler, gate, false)
		s.releaseOwned(session)
		if err == nil {
			return assembler, nil
		}
		// Never retry once any output has been emitted to the client or assembled.
		if (gate != nil && gate.opened()) || (!converted.stream && (len(assembler.blocks) > 0 || len(assembler.tools) > 0)) {
			return assembler, err
		}
		var connectErr *cursorConnectError
		if !errors.As(err, &connectErr) {
			return assembler, err
		}
		status := connectErr.upstreamStatus()
		if status == http.StatusUnauthorized && !refreshed401 && s.authorizer != nil && s.authorizer.isOAuth() {
			refreshed401 = true
			rejected := withRejectedOAuthToken(ctx, session.token)
			if _, authErr := s.authorizer.authorize(rejected, true); authErr != nil {
				return assembler, &oauthAuthError{
					status:  http.StatusUnauthorized,
					message: fmt.Sprintf("%v (refresh Cursor OAuth token failed: %v)", connectErr, authErr),
					err:     authErr,
				}
			}
			continue
		}
		if isFastRetryStatus(status) && fastRetryIdx < len(upstreamFastRetryBackoff) && !isNonRetryableQuotaBody(connectErr.Error()) {
			delay := jitterBackoffDelay(upstreamFastRetryBackoff[fastRetryIdx])
			fastRetryIdx++
			LogWarnEvent("upstream_retry", "component", "cursor", "request_id", requestLogID(ctx),
				"status", status, "retry", fastRetryIdx, "retry_limit", len(upstreamFastRetryBackoff),
				"wait", delay, "action", "fast_retry_trailer")
			if waitErr := sleepContext(ctx, delay); waitErr != nil {
				return assembler, err
			}
			continue
		}
		return assembler, err
	}
}

// takeParked hands a parked stream back to the request that answers its tool
// call. Unrelated parked streams belonging to the same conversation are dropped
// and closed, while streams owned by other concurrent conversations stay parked.
func (s *cursorService) takeParked(sessionKey string, callIDs []string) *cursorSession {
	var matched *cursorSession
	var toClose []*cursorSession

	s.mu.Lock()
	remaining := s.parked[:0]
	for _, candidate := range s.parked {
		if candidate == nil {
			continue
		}
		if matched == nil && candidate.sessionKey == sessionKey && candidate.matches(callIDs) {
			matched = candidate
			continue
		}
		remaining = append(remaining, candidate)
	}
	if matched == nil && len(remaining) > 0 {
		kept := remaining[:0]
		for _, candidate := range remaining {
			if candidate.sessionKey == sessionKey {
				toClose = append(toClose, candidate)
				continue
			}
			kept = append(kept, candidate)
		}
		remaining = kept
	}
	clear(s.parked[len(remaining):])
	s.parked = remaining
	s.mu.Unlock()

	for _, stale := range toClose {
		stale.close()
	}
	if matched == nil {
		return nil
	}
	matched.handoff()
	return matched
}

func (s *cursorService) park(session *cursorSession) {
	if session == nil {
		return
	}
	var toClose []*cursorSession
	s.mu.Lock()
	kept := s.parked[:0]
	for _, existing := range s.parked {
		if existing == nil || existing == session {
			continue
		}
		if existing.sessionKey == session.sessionKey {
			toClose = append(toClose, existing)
			continue
		}
		kept = append(kept, existing)
	}
	for len(kept) >= cursorMaxParkedSessions {
		toClose = append(toClose, kept[0])
		kept = kept[1:]
	}
	clear(s.parked[len(kept):])
	s.parked = append(kept, session)
	s.mu.Unlock()
	for _, previous := range toClose {
		previous.close()
	}
}

// releaseOwned closes a stream the turn is done with, unless parking it handed
// it to a later request.
func (s *cursorService) releaseOwned(session *cursorSession) {
	s.mu.Lock()
	parked := false
	for _, existing := range s.parked {
		if existing == session {
			parked = true
			break
		}
	}
	s.mu.Unlock()
	if !parked {
		session.close()
	}
}

func (s *cursorService) closeParkedForSession(sessionKey string) {
	var toClose []*cursorSession
	s.mu.Lock()
	kept := s.parked[:0]
	for _, existing := range s.parked {
		if existing == nil {
			continue
		}
		if existing.sessionKey == sessionKey {
			toClose = append(toClose, existing)
			continue
		}
		kept = append(kept, existing)
	}
	clear(s.parked[len(kept):])
	s.parked = kept
	s.mu.Unlock()
	for _, session := range toClose {
		session.close()
	}
}

func (s *cursorService) closeParked() {
	s.mu.Lock()
	toClose := s.parked
	s.parked = nil
	s.mu.Unlock()
	for _, session := range toClose {
		if session != nil {
			session.close()
		}
	}
}

// startRun opens the Connect stream and writes the initial Run frame. The 401
// refresh inside the attempt and the shared 429/5xx fast retry are the same
// treatment every other OAuth backend gets.
func (s *cursorService) startRun(ctx context.Context, converted *cursorConvertedRequest) (*cursorSession, error) {
	runID := uuidString()
	params := cursorRunParams{
		ModelID:        s.upstreamModel(converted),
		SystemPrompt:   converted.system,
		UserText:       converted.userText,
		MessageID:      runID,
		ConversationID: converted.conversationID,
		Images:         converted.images,
		Tools:          converted.tools,
		Thinking:       converted.thinkingEnabled,
	}
	encoded := encodeCursorRunRequest(&params)
	s.storeBlobs(params.Blobs)
	// The stream outlives this HTTP request: a parked stream is resumed by a
	// later one, so it is owned by the runtime, not by ctx. Each stream derives
	// its own context from the runtime, while runCtx also observes ctx so client
	// disconnects abort dialing, retry backoff, and token refreshes immediately.
	runCtx, cancel := context.WithCancel(s.serviceContext())
	defer cancel()
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()

	var opened *cursorSession
	attempt := func() (*http.Response, error) {
		session, err := s.openStream(s.serviceContext(), runCtx, encoded)
		if err != nil {
			return nil, err
		}
		response := session.response
		if response.StatusCode == http.StatusUnauthorized && s.authorizer.isOAuth() {
			// A stale token 401s once; force-refresh and retry the request once
			// before surfacing the failure.
			rejected := rejectedResponseContext(runCtx, response)
			rejectedBody, _ := io.ReadAll(io.LimitReader(response.Body, cursorMaxUpstreamErrorBytes))
			session.close()
			if _, authErr := s.authorizer.authorize(rejected, true); authErr != nil {
				detail := strings.TrimSpace(string(rejectedBody))
				if detail == "" {
					detail = http.StatusText(http.StatusUnauthorized)
				}
				return nil, &oauthAuthError{
					status:  http.StatusUnauthorized,
					message: fmt.Sprintf("Cursor upstream returned HTTP 401: %s (refresh Cursor OAuth token failed: %v)", detail, authErr),
					err:     authErr,
				}
			}
			session, err = s.openStream(s.serviceContext(), runCtx, encoded)
			if err != nil {
				return nil, err
			}
			response = session.response
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			retryAfter := response.Header.Get("Retry-After")
			headers := extractProxyResponseHeaders(response.Header)
			body, _ := io.ReadAll(io.LimitReader(response.Body, cursorMaxUpstreamErrorBytes))
			session.close()
			return nil, &cursorUpstreamError{
				status:     response.StatusCode,
				body:       strings.TrimSpace(string(body)),
				retryAfter: retryAfter,
				headers:    headers,
			}
		}
		session.sessionKey = converted.sessionKey
		session.systemHash = converted.systemHash
		opened = session
		return response, nil
	}
	response, err := retryUpstream(runCtx, "cursor", attempt)
	if opened != nil {
		// retryUpstream returns the last outcome undrained; a 2xx stream is the
		// success path, and its body belongs to the session.
		return opened, nil
	}
	if err != nil {
		return nil, err
	}
	if response != nil {
		drainAndClose(response)
	}
	return nil, fmt.Errorf("Cursor agent stream returned no response")
}

// upstreamModel resolves a configured alias to the technical model id Cursor
// accepts, falling back to the requested model itself.
func (s *cursorService) upstreamModel(converted *cursorConvertedRequest) string {
	if route := s.modelRoute[strings.ToLower(strings.TrimSpace(converted.clientModel))]; route != "" {
		return route
	}
	return converted.model
}

func (s *cursorService) serviceContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// openStream sends the Run frame and holds the request body open. Each attempt
// gets its own child context derived from streamCtx (so parked streams outlive
// the first turn), while dialCtx cancels dialing/header waits immediately if
// the caller disconnects before the stream opens.
func (s *cursorService) openStream(streamCtx, dialCtx context.Context, frame []byte) (*cursorSession, error) {
	token, err := s.authorizer.authorize(dialCtx, false)
	if err != nil {
		return nil, wrapOAuthAuthorizeError("authorize Cursor agent request", err)
	}
	attemptCtx, cancel := context.WithCancel(streamCtx)
	stopDialCancel := context.AfterFunc(dialCtx, cancel)
	pipeReader, pipeWriter := io.Pipe()
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, cursorConnectBaseURL+cursorConnectRunPath, pipeReader)
	if err != nil {
		stopDialCancel()
		cancel()
		_ = pipeWriter.Close()
		return nil, err
	}
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/connect+proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Cursor-Client-Version", cursorCLIVersion)
	request.Header.Set("User-Agent", "gbx/"+cursorCLIVersion)
	initDone := make(chan struct{})
	go func() {
		defer close(initDone)
		if _, writeErr := pipeWriter.Write(cursorFrameConnect(frame, 0)); writeErr != nil {
			_ = pipeWriter.CloseWithError(writeErr)
		}
	}()
	started := time.Now()
	response, err := doCredentialRequest(s.client, request)
	stopDialCancel()
	if err != nil {
		cancel()
		_ = pipeWriter.CloseWithError(err)
		return nil, fmt.Errorf("call Cursor agent stream: %w", err)
	}
	if dialCtx.Err() != nil {
		cancel()
		_ = pipeWriter.CloseWithError(dialCtx.Err())
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, dialCtx.Err()
	}
	LogUpstreamEvent(response.StatusCode, "upstream_response", "component", "cursor",
		"status", response.StatusCode, "content_type", response.Header.Get("Content-Type"),
		"duration", logDuration(started))
	session := newCursorSession(response, pipeWriter, cancel)
	session.initDone = initDone
	session.token = token
	return session, nil
}

// continueTurn relays one stream. A tool request from Cursor ends the Responses
// output with a function_call item and parks the stream; the next request that
// carries matching tool results resumes it here.
func (s *cursorService) continueTurn(ctx context.Context, session *cursorSession, converted *cursorConvertedRequest, assembler *responseAssembler, gate *cursorGateWriter, resuming bool) error {
	stopTurnCancel := context.AfterFunc(ctx, session.close)
	defer stopTurnCancel()
	// message_start is emitted while the gate still holds every event, so a turn
	// that fails before its first block still answers with a status code.
	if err := assembler.start(); err != nil {
		return err
	}
	if resuming {
		for _, result := range converted.resume {
			call, ok := session.takePending(result.ToolUseID)
			if !ok {
				continue
			}
			if err := session.send(cursorFrameConnect(
				encodeCursorExecMcpResult(call.msgID, call.execID, result.Text, result.IsError), 0)); err != nil {
				return err
			}
		}
	}
	for {
		flags, payload, err := cursorReadFrame(session.reader)
		if err != nil {
			if ctx.Err() != nil || s.serviceContext().Err() != nil {
				return context.Canceled
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("Cursor agent stream truncated: %w", err)
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("Cursor agent stream ended before terminal event: %w", io.ErrUnexpectedEOF)
			}
			return fmt.Errorf("read Cursor agent stream: %w", err)
		}
		if flags&cursorConnectCompression != 0 {
			return fmt.Errorf("Cursor agent stream used unsupported compression")
		}
		if flags&cursorConnectEndStream != 0 {
			if trailerErr := cursorParseEndStream(payload); trailerErr != nil {
				return trailerErr
			}
			break
		}
		events := decodeCursorASM(payload)
		// Validate every exec_mcp tool call in the frame before emitting even
		// the first call or opening the HTTP 200 SSE gate.
		for _, event := range events {
			if event.Kind != "exec_mcp" {
				continue
			}
			if _, _, _, err := assembler.validateToolUse(event.MCPName, cursorMCPArgsJSON(event.MCPArgs)); err != nil {
				return err
			}
		}
		sawExecMCP := false
		for _, event := range events {
			switch event.Kind {
			case "text":
				if event.Text == "" {
					continue
				}
				if err := assembler.addText(event.Text); err != nil {
					return err
				}
				gate.open()
			case "thinking":
				if event.Text == "" {
					continue
				}
				if err := assembler.addThinking(event.Text); err != nil {
					return err
				}
				gate.open()
			case "thinking_done":
				if err := assembler.closeActive(); err != nil {
					return err
				}
			case "token":
				if event.TokenDelta > 0 {
					assembler.outputTokens += int(event.TokenDelta)
					assembler.outputUsageKnown = true
				}
			case "kv_get":
				if err := session.send(cursorFrameConnect(encodeCursorKvGetResult(event.KvID, s.blob(event.BlobID)), 0)); err != nil {
					return err
				}
			case "kv_set":
				s.storeBlob(event.BlobID, event.BlobData)
				if err := session.send(cursorFrameConnect(encodeCursorKvSetResult(event.KvID), 0)); err != nil {
					return err
				}
			case "checkpoint":
				s.storeBlob(sha256SumBytes(event.BlobData), event.BlobData)
			case "exec_mcp":
				callID := firstNonEmptyString(event.MCPCallID, event.ExecID, uuidString())
				if err := assembler.addToolUse(callID, event.MCPName, cursorMCPArgsJSON(event.MCPArgs)); err != nil {
					return err
				}
				session.addPending(callID, cursorPendingCall{msgID: event.ExecMsgID, execID: event.ExecID})
				gate.open()
				sawExecMCP = true
			case "exec_context":
				if err := session.send(cursorFrameConnect(encodeCursorRequestContext(event.ExecMsgID, event.ExecID, converted.tools), 0)); err != nil {
					return err
				}
			case "exec_other", "exec_shell", "exec_write", "exec_delete", "exec_read", "exec_ls",
				"exec_grep", "exec_fetch", "exec_diagnostics", "exec_bgshell", "exec_stdin":
				// Cursor's own built-in tools never run here: Claude Code owns the
				// tool loop, so every builtin is rejected immediately.
				if err := session.send(cursorFrameConnect(encodeCursorRejectExec(event), 0)); err != nil {
					return err
				}
			case "turn_end":
				if sawExecMCP {
					break
				}
				gate.open()
				return assembler.finish()
			}
		}
		if sawExecMCP {
			gate.open()
			if err := assembler.finish(); err != nil {
				return err
			}
			// Detach this HTTP request's cancellation before parking the stream
			// so the parked stream survives after the response completes.
			if !stopTurnCancel() {
				return context.Canceled
			}
			// The stream stays open: the next request carries the tool result(s)
			// and continues it instead of replaying the turn.
			session.startHeartbeat()
			s.park(session)
			return nil
		}
	}
	gate.open()
	return assembler.finish()
}

func cursorReadFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [cursorConnectHeaderSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	flags := header[0]
	length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])
	if length > cursorMaxConnectFrame {
		return 0, nil, fmt.Errorf("Cursor agent frame exceeds %d bytes", cursorMaxConnectFrame)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return flags, payload, nil
}

// storeBlobs keeps the prompt blobs a Run refers to. The map is bounded because
// a long session would otherwise retain every checkpoint it ever received.
func (s *cursorService) storeBlobs(blobs map[string][]byte) {
	for key, value := range blobs {
		s.storeBlob([]byte(key), value)
	}
}

// blobKey normalizes an identity to lower-case hex of the raw 32-byte hash, so
// a raw hash and its hex form resolve to the same blob.
func blobKey(identity []byte) string {
	if len(identity) == 64 {
		if decoded, err := hex.DecodeString(string(identity)); err == nil {
			return hex.EncodeToString(decoded)
		}
	}
	return hex.EncodeToString(identity)
}

func (s *cursorService) storeBlob(identity, data []byte) {
	if len(identity) == 0 || len(data) > cursorMaxBlobBytes {
		return
	}
	key := blobKey(identity)
	copied := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobs == nil {
		s.blobs = make(map[string][]byte)
	}
	if old, exists := s.blobs[key]; exists {
		s.blobBytes -= len(old)
		delete(s.blobs, key)
		for i, k := range s.blobOrder {
			if k == key {
				s.blobOrder = append(s.blobOrder[:i], s.blobOrder[i+1:]...)
				break
			}
		}
	}
	for len(s.blobOrder) > 0 && (len(s.blobOrder) >= cursorMaxBlobs || s.blobBytes+len(copied) > cursorMaxBlobBytes) {
		dropped := s.blobOrder[0]
		s.blobOrder = s.blobOrder[1:]
		if old, ok := s.blobs[dropped]; ok {
			s.blobBytes -= len(old)
			delete(s.blobs, dropped)
		}
	}
	s.blobs[key] = copied
	s.blobBytes += len(copied)
	s.blobOrder = append(s.blobOrder, key)
}

// blob resolves a KV lookup by either the raw 32-byte hash or its hex form.
func (s *cursorService) blob(identity []byte) []byte {
	if len(identity) == 0 {
		return nil
	}
	key := blobKey(identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blobs[key]
}

// cursorGateWriter withholds the SSE response until the turn produces its first
// block, so an upstream failure still reaches Claude Code as an HTTP status
// instead of a broken 200 stream.
type cursorGateWriter struct {
	response http.ResponseWriter
	mu       sync.Mutex
	openedOK bool
	pending  bytes.Buffer
	header   bool
	hdr      http.Header
}

func (g *cursorGateWriter) Header() http.Header {
	if g == nil {
		return http.Header{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.openedOK {
		return g.response.Header()
	}
	if g.hdr == nil {
		g.hdr = make(http.Header)
	}
	return g.hdr
}

func (g *cursorGateWriter) WriteHeader(status int) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.openedOK {
		return
	}
	if !g.header {
		g.header = true
		g.response.WriteHeader(status)
	}
}

func (g *cursorGateWriter) Write(p []byte) (int, error) {
	if g == nil {
		return len(p), nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.openedOK {
		if g.pending.Len()+len(p) > assemblerMaxRetainedBytes {
			return 0, fmt.Errorf("Cursor response buffering exceeds %d bytes", assemblerMaxRetainedBytes)
		}
		return g.pending.Write(p)
	}
	return g.flushLocked(p)
}

func (g *cursorGateWriter) flushLocked(p []byte) (int, error) {
	if !g.header {
		g.header = true
		for key, values := range g.hdr {
			dst := g.response.Header()
			dst[key] = append([]string(nil), values...)
		}
		g.hdr = nil
		g.response.WriteHeader(http.StatusOK)
	}
	written, err := g.response.Write(p)
	if err == nil {
		if flusher, ok := g.response.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return written, err
}

// reset discards buffered pre-gate events and headers before a pre-gate retry.
func (g *cursorGateWriter) reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.openedOK {
		return
	}
	g.pending.Reset()
	g.hdr = nil
}

// open releases the buffered events. It is safe to call more than once and on a
// nil gate (non-streaming requests have none).
func (g *cursorGateWriter) open() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.openedOK {
		return
	}
	g.openedOK = true
	if g.pending.Len() == 0 {
		return
	}
	buffered := g.pending.Bytes()
	g.pending.Reset()
	_, _ = g.flushLocked(buffered)
}

func (g *cursorGateWriter) opened() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.openedOK
}

// discoverCursorModels reads the account's model list. A failure is not fatal:
// the runtime keeps a compatibility catalog and marks it as a fallback so
// callers do not overwrite a catalog they already have with a guess.
func discoverCursorModels(ctx context.Context, authorizer *cursorOAuthAuthorizer, client *http.Client) ([]string, error) {
	send := func(token string) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, cursorConnectBaseURL+cursorConnectModelsPath, bytes.NewReader(nil))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/proto")
		request.Header.Set("Connect-Protocol-Version", "1")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Cursor-Client-Version", cursorCLIVersion)
		return doCredentialRequest(client, request)
	}
	token, err := authorizer.authorize(ctx, false)
	if err != nil {
		return nil, err
	}
	response, err := send(token)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && authorizer != nil && authorizer.isOAuth() {
		rejected := rejectedResponseContext(ctx, response)
		drainAndClose(response)
		token, err = authorizer.authorize(rejected, true)
		if err != nil {
			return nil, wrapOAuthAuthorizeError("refresh Cursor OAuth token", err)
		}
		response, err = send(token)
		if err != nil {
			return nil, err
		}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cursorMaxErrorBytes))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &cursorUpstreamError{
			status:     response.StatusCode,
			body:       strings.TrimSpace(string(body)),
			retryAfter: response.Header.Get("Retry-After"),
			headers:    extractProxyResponseHeaders(response.Header),
		}
	}
	models := decodeCursorUsableModels(body)
	if len(models) == 0 {
		return nil, fmt.Errorf("Cursor model catalog returned no models")
	}
	return models, nil
}

// cursorEffectiveModels publishes configured aliases first, then whatever the
// account reported, so a route the user picked always resolves. fallbackOnly
// reports that nothing but the built-in list is known.
func cursorEffectiveModels(modelSpec string, discovered []string) (models []string, fallbackOnly bool) {
	aliases := runtimeModelAliases(modelSpec)
	if len(discovered) > 0 {
		models = make([]string, 0, len(aliases)+len(discovered))
		seen := make(map[string]bool, len(aliases)+len(discovered))
		for _, model := range append(append([]string(nil), aliases...), discovered...) {
			id := strings.TrimSpace(model)
			key := strings.ToLower(id)
			if id == "" || seen[key] {
				continue
			}
			seen[key] = true
			models = append(models, id)
		}
		return models, false
	}
	if len(aliases) > 0 {
		return aliases, false
	}
	return append([]string(nil), cursorFallbackModels...), true
}

func cursorModelRoutes(modelSpec string, models []string) map[string]string {
	routes := make(map[string]string, len(models))
	for _, route := range runtimeModelRoutes(modelSpec) {
		routes[strings.ToLower(route.Alias)] = route.Name
	}
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			continue
		}
		if _, ok := routes[key]; !ok {
			routes[key] = model
		}
	}
	return routes
}

func cursorModelDetails(models []string, routes map[string]string) map[string]protocol.ModelInfo {
	index, _ := modelsdev.CachedIndex()
	details := make(map[string]protocol.ModelInfo, len(models))
	for _, alias := range models {
		trimmed := strings.TrimSpace(alias)
		if trimmed == "" {
			continue
		}
		target := routes[strings.ToLower(trimmed)]
		if target == "" {
			target = trimmed
		}
		var info protocol.ModelInfo
		matched := false
		if index != nil {
			if mod, ok := index.InputModalities("", target, trimmed); ok {
				info.InputModalities = mod
				matched = true
			}
			if maxOut, ok := index.MaxOutputTokens("", target, trimmed); ok {
				info.MaxOutputTokens = maxOut
				matched = true
			}
			if efforts, ok := index.Efforts("", target, trimmed); ok {
				info.ReasoningLevels, info.DefaultReasoningLevel = reasoningLevelsFromEfforts(efforts, "")
				info.ReasoningKnown = true
				matched = true
			}
		}
		if len(info.InputModalities) == 0 && cursorKnownVisionModel(target) {
			info.InputModalities = []string{"text", "image"}
			matched = true
		}
		if matched {
			info.ID = trimmed
			details[strings.ToLower(trimmed)] = info
		}
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func cursorKnownVisionModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripContextModelSuffix(model)))
	for _, prefix := range []string{
		"claude-3-5-sonnet", "claude-3.5-sonnet", "claude-3-7-sonnet", "claude-3.7-sonnet",
		"claude-4", "claude-sonnet-4", "claude-opus-4", "claude-haiku-4",
		"gpt-4o", "gpt-4.1", "gpt-5",
		"gemini-2", "gemini-3", "gemini-4",
		"grok-4", "composer-2",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func startCursorOAuth(parent context.Context, modelSpec, credentialFile string) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	authDir, err := ensureAuthDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(authDir, filepath.Base(credentialFile))
	authorizer := &cursorOAuthAuthorizer{
		path:   path,
		client: newCredentialHTTPClient(60 * time.Second),
	}
	credential, err := authorizer.load()
	if err != nil {
		return nil, err
	}
	if credential.disabled {
		return nil, fmt.Errorf("Cursor credential %s is disabled", filepath.Base(path))
	}
	if credential.accessToken == "" && credential.refreshToken == "" {
		return nil, fmt.Errorf("Cursor credential %s has no access or refresh token; run `gbx oauth cursor` again", filepath.Base(path))
	}
	discoveryClient := newCredentialHTTPClient(20 * time.Second)
	discoveryCtx, cancelDiscovery := context.WithTimeout(parent, 20*time.Second)
	discovered, discoverErr := discoverCursorModels(discoveryCtx, authorizer, discoveryClient)
	cancelDiscovery()
	catalogFallback := discoverErr != nil
	if catalogFallback {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		LogWarnf("Cursor model discovery failed; keeping the compatibility catalog error=%v", discoverErr)
	}
	models, fallbackOnly := cursorEffectiveModels(modelSpec, discovered)
	catalogFallback = catalogFallback || fallbackOnly
	routes := cursorModelRoutes(modelSpec, models)
	if len(routes) == 0 {
		return nil, fmt.Errorf("Cursor runtime requires at least one model")
	}
	apiKey, err := sessionAPIKey()
	if err != nil {
		return nil, err
	}
	details := cursorModelDetails(models, routes)
	serviceCtx, serviceCancel := context.WithCancel(parent)
	usage := NewUsageTracker()
	service := &cursorService{
		apiKey:       apiKey,
		models:       models,
		modelDetails: details,
		modelRoute:   routes,
		authorizer:   authorizer,
		client:       newCredentialHTTPClient(60 * time.Second),
		usage:        usage,
		ctx:          serviceCtx,
	}
	proxyRuntime, err := startLoopbackServer(parent, "Cursor runtime", service.handler(), apiKey, usage)
	if err != nil {
		serviceCancel()
		return nil, err
	}
	proxyRuntime.listAuths = authorizer.listAuths
	proxyRuntime.models = models
	proxyRuntime.modelDetails = details
	proxyRuntime.catalogFallback = catalogFallback
	proxyRuntime.cleanup = append(proxyRuntime.cleanup,
		serviceCancel,
		closeIdleClients(service.client, authorizer.client, discoveryClient),
		service.closeParked,
	)
	LogInfof("runtime start oauth provider=cursor backend=cursor protocol=responses port=%s credential_file=%s model_count=%d fallback_catalog=%t",
		strings.TrimPrefix(strings.TrimSuffix(proxyRuntime.endpoint, "/v1"), "http://"), filepath.Base(path), len(models), catalogFallback)
	return proxyRuntime, nil
}

// cursorConvertedRequest is one Responses turn expressed as a Cursor Run: the
// system prompt, the flattened history plus this turn's text, the images, the
// tools the client declared, and any tool results that answer a parked stream.
type cursorConvertedRequest struct {
	adapterRequest
	model            string
	system           string
	systemHash       string
	history          string
	current          string
	userText         string
	images           []cursorImage
	hasCurrentImages bool
	tools            []cursorMCPTool
	resume           []cursorToolResult
	sessionKey       string
	anonSessionKey   string
	conversationID   string
}

func (c *cursorConvertedRequest) resumeToolUseIDs() []string {
	ids := make([]string, 0, len(c.resume))
	for _, result := range c.resume {
		if id := strings.TrimSpace(result.ToolUseID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func cursorLastTurnHasImages(turns []conversationTurn) bool {
	if len(turns) == 0 {
		return false
	}
	last := turns[len(turns)-1]
	if strings.EqualFold(strings.TrimSpace(last.Role), "assistant") {
		return false
	}
	for _, part := range last.Parts {
		if part.Kind == partImage {
			return true
		}
		if part.Kind == partToolResult && len(part.outputImages()) > 0 {
			return true
		}
	}
	return false
}

func cursorAnonymousSessionKey(turns []conversationTurn) string {
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.Kind == partText && strings.TrimSpace(part.Text) != "" {
				return fmt.Sprintf("anon:%x", sha256SumBytes([]byte(turn.Role + "\x00" + strings.TrimSpace(part.Text)))[:12])
			}
		}
	}
	return ""
}

func convertResponsesToCursor(raw []byte) (*cursorConvertedRequest, error) {
	request, err := parseResponsesRequest(raw)
	if err != nil {
		return nil, err
	}
	history, current, images, resume, err := flattenCursorMessages(request.Turns)
	if err != nil {
		return nil, err
	}
	var tools []cursorMCPTool
	if request.ToolChoice.Mode != toolChoiceNone {
		tools, err = cursorToolsFromResponses(request.Tools)
		if err != nil {
			return nil, err
		}
	}
	model := stripContextModelSuffix(request.Model)
	adapter := request.adapter(model)
	sessionKey := firstNonEmptyString(request.CacheKey, request.SafetyIdentifier, request.User)
	system := strings.TrimSpace(request.System)
	if instruction := structuredOutputPromptInstruction(request.Format); instruction != "" {
		if system != "" {
			system += "\n\n"
		}
		system += instruction
	}
	converted := &cursorConvertedRequest{
		adapterRequest:   adapter,
		model:            model,
		system:           system,
		systemHash:       string(sha256SumBytes([]byte(system))),
		history:          history,
		current:          current,
		images:           images,
		hasCurrentImages: cursorLastTurnHasImages(request.Turns),
		tools:            tools,
		resume:           resume,
		sessionKey:       sessionKey,
		anonSessionKey:   cursorAnonymousSessionKey(request.Turns),
		conversationID:   kiroConversationID(sessionKey),
	}
	converted.userText = cursorTurnUserText(history, current, resume)
	return converted, nil
}

// cursorTurnUserText folds history and any tool results into the single user
// text Cursor's Run request carries. Tool results always precede follow-up user
// text so they stay adjacent to the preceding ASSISTANT_TOOL_CALL.
func cursorTurnUserText(history, current string, results []cursorToolResult) string {
	if len(results) == 0 {
		return cursorComposeUserText(history, current)
	}
	var turn strings.Builder
	for _, result := range results {
		if turn.Len() > 0 {
			turn.WriteString("\n\n")
		}
		turn.WriteString("TOOL_RESULT:\n")
		turn.WriteString(result.Text)
	}
	if trimmed := strings.TrimSpace(current); trimmed != "" {
		if turn.Len() > 0 {
			turn.WriteString("\n\n")
		}
		turn.WriteString(trimmed)
	}
	return cursorComposeUserText(history, turn.String())
}

func cursorToolsFromResponses(tools []conversationTool) ([]cursorMCPTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]cursorMCPTool, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		schema := bytesTrimSpace(tool.Parameters)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("tool %s has an invalid schema", name)
		}
		out = append(out, cursorMCPTool{Name: name, Description: tool.Description, InputSchema: schema})
	}
	return out, nil
}
