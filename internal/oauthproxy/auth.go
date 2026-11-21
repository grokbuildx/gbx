package oauthproxy

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/grokbuildx/gbx/internal/paths"
	"github.com/grokbuildx/gbx/internal/provider"
)

const (
	ProviderCodex     = "codex"
	ProviderChatGPT   = "gpt"
	ProviderGrok      = "grok"
	ProviderQoder     = "qoder"
	ProviderKimi      = "kimi"
	ProviderKiro      = "kiro"
	ProviderWorkBuddy = "workbuddy"
	// ProviderWorkBuddyCN is the mainland-China WorkBuddy deployment
	// (www.workbuddy.cn). It runs the same plugin API and chat protocol as
	// ProviderWorkBuddy, so both share one implementation and differ only in
	// origin and in the credential `type` gbx persists.
	ProviderWorkBuddyCN = "workbuddycn"
	// ProviderAutoClaw is the AutoClaw Code (ZCode coding plan) backend. GBX
	// performs browser OAuth, refreshes the resulting session locally, and owns
	// the Responses-to-OpenAI Chat adapter used to reach the managed proxy.
	ProviderAutoClaw = "autoclaw"
	// ProviderCursor is the Cursor subscription backend. GBX owns the PKCE
	// browser sign-in, its poll loop, token refresh, and the Connect-RPC
	// AgentService adapter that speaks OpenAI Responses to the client.
	ProviderCursor = "cursor"
	// backendXAI is the internal backend key for xAI/Grok credentials.
	backendXAI = "xai"
)

type LoginOptions struct {
	NoBrowser    bool
	CallbackPort int
	KiroAuthMode string
}

type LoginResult struct {
	Provider string
	Backend  string
	Path     string
}

func AuthDir() (string, error) {
	return paths.AuthDir()
}

func ensureAuthDir() (string, error) {
	authDir, err := AuthDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", fmt.Errorf("create auth directory: %w", err)
	}
	if err := os.Chmod(authDir, 0o700); err != nil {
		return "", fmt.Errorf("secure auth directory: %w", err)
	}
	return authDir, nil
}

func Login(ctx context.Context, providerName string, opts LoginOptions) (LoginResult, error) {
	target, err := ValidateLoginProvider(providerName)
	if err != nil {
		return LoginResult{}, err
	}

	authDir, err := ensureAuthDir()
	if err != nil {
		return LoginResult{}, err
	}
	switch target {
	case ProviderKiro:
		return loginKiro(ctx, authDir, opts)
	case ProviderQoder:
		return loginQoder(ctx, authDir, opts)
	case ProviderWorkBuddy, ProviderWorkBuddyCN:
		return loginWorkBuddy(ctx, authDir, target, opts)
	case ProviderChatGPT:
		return loginCodex(ctx, authDir, opts)
	case ProviderGrok:
		return loginXai(ctx, authDir, opts)
	case ProviderKimi:
		return loginKimi(ctx, authDir, opts)
	case ProviderAutoClaw:
		return loginAutoClaw(ctx, authDir, opts)
	case ProviderCursor:
		return loginCursor(ctx, authDir, opts)
	default:
		return LoginResult{}, fmt.Errorf("unsupported OAuth provider %q", target)
	}
}

// ImportCredential is the non-browser import path for backends whose official
// CLI or desktop app already stores a long-lived key instead of offering a
// third-party OAuth flow. Browser OAuth stays on Login; imports stay here so
// `gbx oauth` never advertises a backend that cannot do OAuth.
func ImportCredential(ctx context.Context, providerName string) (LoginResult, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	switch target {
	case ProviderAutoClaw:
		authDir, err := ensureAuthDir()
		if err != nil {
			return LoginResult{}, err
		}
		return ImportAutoClawCredential(ctx, authDir)
	default:
		return LoginResult{}, fmt.Errorf("unsupported import provider %q (use autoclaw)", providerName)
	}
}

// ValidateLoginProvider returns the canonical public OAuth provider name.
// Codex remains an internal backend and a legacy runtime value, but new logins
// use the public GPT name (model family) because both routes authenticate the same account.
// AutoClaw runs its Google browser flow and stores a refreshable managed-proxy credential.
func ValidateLoginProvider(providerName string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	if provider.IsRetiredOAuthProvider(target) {
		return "", fmt.Errorf("OAuth provider %q has been retired", target)
	}
	if spec, ok := provider.LookupPublicOAuthSpec(target); ok {
		return spec.PublicName, nil
	}
	return "", fmt.Errorf("unsupported auth provider %q (use gpt, grok, qoder, kimi, kiro, workbuddy, workbuddycn, autoclaw, or cursor)", providerName)
}

func BackendProvider(providerName string) (string, error) {
	target := strings.ToLower(strings.TrimSpace(providerName))
	if provider.IsRetiredOAuthProvider(target) {
		return "", fmt.Errorf("OAuth provider %q has been retired", target)
	}
	spec, ok := provider.LookupOAuthSpec(target)
	if !ok {
		return "", fmt.Errorf("unsupported OAuth provider %q", providerName)
	}
	return spec.Backend, nil
}
