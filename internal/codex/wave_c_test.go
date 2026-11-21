package codex

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestResponseHistoryPutOverwriteTracksBytes(t *testing.T) {
	var h responseHistory
	payload1 := []map[string]any{{"text": strings.Repeat("a", 4096)}}
	payload2 := []map[string]any{{"text": "short"}}

	h.put("resp-1", "route-a", "owner-1", payload1)
	firstBytes := h.bytes
	if firstBytes < 4096 {
		t.Fatalf("expected bytes >= 4096, got %d", firstBytes)
	}

	// Overwriting the same key must subtract the old size rather than leaking it.
	for i := 0; i < 100; i++ {
		h.put("resp-1", "route-a", "owner-1", payload2)
	}
	if h.bytes >= firstBytes {
		t.Fatalf("expected overwritten entry bytes < %d, got %d", firstBytes, h.bytes)
	}
	if len(h.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(h.entries))
	}

	// Empty id must be ignored.
	before := h.bytes
	h.put("", "route-a", "owner-1", payload1)
	if h.bytes != before || len(h.entries) != 1 {
		t.Fatalf("empty id should not modify history: bytes=%d entries=%d", h.bytes, len(h.entries))
	}
}

func TestIsBearerTokenCaseInsensitive(t *testing.T) {
	const tok = "gbx_secret_token"
	if !isBearerToken("Bearer "+tok, tok) {
		t.Fatal("expected exact Bearer match")
	}
	if !isBearerToken("bearer "+tok, tok) {
		t.Fatal("expected lowercase bearer match")
	}
	if !isBearerToken("  BeArEr   "+tok+"  ", tok) {
		t.Fatal("expected trimmed case-insensitive match")
	}
	if isBearerToken("Bearer wrong", tok) {
		t.Fatal("wrong token should not match")
	}
	if isBearerToken("Basic "+tok, tok) {
		t.Fatal("non-Bearer scheme should not match")
	}
}

func TestTrimUTF8CutBoundaries(t *testing.T) {
	// 3-byte CJK rune split at 1 and 2 bytes, 4-byte emoji split at 1..3 bytes.
	sample := "abc" + "中" + "😀"
	for cut := 0; cut <= len(sample); cut++ {
		head := trimUTF8CutEnd(sample[:cut])
		tail := trimUTF8CutStart(sample[cut:])
		if !utf8.ValidString(head) {
			t.Fatalf("cut=%d: head %q is not valid UTF-8", cut, head)
		}
		if !utf8.ValidString(tail) {
			t.Fatalf("cut=%d: tail %q is not valid UTF-8", cut, tail)
		}
	}
	// Invalid UTF-8 in the interior must not cause quadratic stripping of the whole string.
	interiorInvalid := "prefix\xff" + strings.Repeat("x", 3000) + "中"[:2]
	trimmed := trimUTF8CutEnd(interiorInvalid)
	if len(trimmed) != len(interiorInvalid)-2 {
		t.Fatalf("expected only trailing 2 split bytes trimmed, got len %d vs %d", len(trimmed), len(interiorInvalid)-2)
	}
}
