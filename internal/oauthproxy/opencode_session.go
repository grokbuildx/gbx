package oauthproxy

import (
	"context"
	"net/http"
	"strings"
)

// OpenCode Go rejects requests without a stable per-conversation ID in
// x-opencode-session (HTTP 400 MissingSessionID). The Chat Completions and
// Responses adapters build fresh upstream requests, so they carry the client's
// session ID over explicitly — and only to OpenCode, so the other gateways
// sharing these adapters keep their own wire identity.
const (
	claudeCodeSessionHeader = "X-Claude-Code-Session-Id"
	openCodeSessionHeader   = "X-Opencode-Session"
)

type clientSessionKey struct{}

// extractClientSessionID reads the conversation/session identifier from common
// client headers (Claude Code, Codex legacy/modern, Grok Build, generic).
func extractClientSessionID(incoming *http.Request) string {
	if incoming == nil {
		return ""
	}
	for _, name := range []string{
		claudeCodeSessionHeader,
		"Session_id",
		"Session-Id",
		"Thread-Id",
		"X-Grok-Conv-Id",
		"X-Codex-Session-Id",
		"X-Session-Id",
	} {
		if id := strings.TrimSpace(incoming.Header.Get(name)); id != "" && len(id) <= 256 {
			return id
		}
	}
	return ""
}

// withClientSession records the client session ID of an incoming request.
func withClientSession(ctx context.Context, incoming *http.Request) context.Context {
	id := extractClientSessionID(incoming)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, clientSessionKey{}, id)
}

func clientSession(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(clientSessionKey{}).(string)
	return id
}

// applyOpenCodeSession stamps an OpenCode upstream request with the client's
// session ID. A request that already carries one is left alone.
func applyOpenCodeSession(ctx context.Context, request *http.Request) {
	if request == nil || request.URL == nil || !isOpenCodeHost(request.URL.Hostname()) {
		return
	}
	if request.Header.Get(openCodeSessionHeader) != "" {
		return
	}
	if id := clientSession(ctx); id != "" {
		request.Header.Set(openCodeSessionHeader, id)
	}
}

func isOpenCodeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}
