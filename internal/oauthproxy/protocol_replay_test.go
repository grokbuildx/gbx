package oauthproxy

import (
	"strings"
	"testing"
)

func TestProtocolStreamErrorsDoNotFinishSuccessfully(t *testing.T) {
	a := newResponseAssembler(&adapterRequest{}, nil)
	err := processChatCompletionsStream(strings.NewReader("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"), a)
	if err == nil || !strings.Contains(err.Error(), "upstream failed") || a.finished {
		t.Fatalf("error=%v finished=%v", err, a.finished)
	}
}
