package protocol

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// maxModelsResponseBytes bounds /models JSON responses read from third-party gateways.
	maxModelsResponseBytes = 16 << 20
	// maxProbeDiscardBytes bounds response bodies drained during 1-token probes.
	maxProbeDiscardBytes = 64 << 10
)

// NewProbeHTTPClient returns an HTTP client for credentialed API/model probes
// that refuses cross-origin redirects so API keys are never leaked.
func NewProbeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: checkSameOriginRedirect,
	}
}

func newProbeHTTPClient(timeout time.Duration) *http.Client {
	return NewProbeHTTPClient(timeout)
}

// checkSameOriginRedirect refuses cross-origin redirects so API keys carried in
// Authorization or x-api-key headers are never forwarded to another host.
func checkSameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 || via[0].URL == nil || req.URL == nil {
		return nil
	}
	orig := via[0].URL
	if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
		return fmt.Errorf("refusing cross-origin redirect to %s://%s", req.URL.Scheme, req.URL.Host)
	}
	return nil
}
