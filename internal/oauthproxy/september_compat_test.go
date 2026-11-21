package oauthproxy

import (
	"strings"
	"testing"
)

func TestChatStructureContainsNoPayload(t *testing.T) {
	body := []byte(`{"messages":[{"content":"secret-text"},{"content":[{"type":"image_url","image_url":{"url":"secret-url"}},{"type":"secret-type"}]}]}`)
	got := chatContentStructure(body)
	if strings.Contains(got, "secret") || !strings.Contains(got, `"part_image_url":1`) || !strings.Contains(got, `"part_unknown":1`) {
		t.Fatalf("unsafe/incorrect structure: %s", got)
	}
}
