package protocol

import (
	"crypto/sha256"
	"strings"
	"sync"
)

const reasoningScopeRegistryMax = 4096

type reasoningScopeRegistry struct {
	mu      sync.Mutex
	entries map[[32]byte]string
	order   [][32]byte
}

var sharedReasoningScopes = &reasoningScopeRegistry{
	entries: make(map[[32]byte]string, 256),
}

// RecordReasoningScope associates an opaque encrypted reasoning payload with
// the backend/endpoint/model scope that produced it so cross-backend switches
// can drop incompatible ciphertext before hitting upstream 400 errors.
func RecordReasoningScope(encryptedContent, scope string) {
	encryptedContent = strings.TrimSpace(encryptedContent)
	scope = strings.TrimSpace(scope)
	if encryptedContent == "" || scope == "" {
		return
	}
	key := sha256.Sum256([]byte(encryptedContent))
	sharedReasoningScopes.mu.Lock()
	defer sharedReasoningScopes.mu.Unlock()
	if existing, ok := sharedReasoningScopes.entries[key]; ok {
		if existing == scope {
			return
		}
		sharedReasoningScopes.entries[key] = scope
		return
	}
	if len(sharedReasoningScopes.order) >= reasoningScopeRegistryMax {
		evict := sharedReasoningScopes.order[0]
		sharedReasoningScopes.order = sharedReasoningScopes.order[1:]
		delete(sharedReasoningScopes.entries, evict)
	}
	sharedReasoningScopes.entries[key] = scope
	sharedReasoningScopes.order = append(sharedReasoningScopes.order, key)
}

// ReasoningScopeMatches reports whether encryptedContent is compatible with
// expectedScope. Payloads whose origin was never recorded in this process
// return true so persisted native transcripts keep working across restarts.
func ReasoningScopeMatches(encryptedContent, expectedScope string) bool {
	encryptedContent = strings.TrimSpace(encryptedContent)
	expectedScope = strings.TrimSpace(expectedScope)
	if encryptedContent == "" || expectedScope == "" {
		return true
	}
	key := sha256.Sum256([]byte(encryptedContent))
	sharedReasoningScopes.mu.Lock()
	recorded, ok := sharedReasoningScopes.entries[key]
	sharedReasoningScopes.mu.Unlock()
	if !ok {
		return true
	}
	return recorded == expectedScope
}

// IsGBXManagedSignature reports whether encryptedContent carries a GBX adapter
// signature prefix that native third-party upstreams cannot decrypt.
func IsGBXManagedSignature(encryptedContent string) bool {
	for _, prefix := range []string{
		"gbx-anthropic:",
		"gbx-anthropic-redacted:",
		"gbx-responses-reasoning-v1:",
		"gbx-kiro:",
		"gbx-kiro-redacted:",
	} {
		if strings.HasPrefix(encryptedContent, prefix) {
			return true
		}
	}
	return false
}
