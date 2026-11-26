package protocol

import (
	"net/url"
	"strings"
)

func normalizeEndpoint(endpoint, fallback string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fallback
	}
	return endpoint
}

// Rewrite only the escaped path. Query parameters, fragments and encoded path
// components must survive endpoint normalization unchanged.
func rewriteEndpointPath(endpoint string, rewrite func(string) string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return rewrite(endpoint)
	}
	escaped := rewrite(strings.TrimRight(u.EscapedPath(), "/"))
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return endpoint
	}
	u.Path, u.RawPath = path, escaped
	return u.String()
}

func normalizeOpenAIPath(baseURL, suffix string) string {
	return rewriteEndpointPath(normalizeEndpoint(baseURL, "https://api.openai.com/v1"), func(path string) string {
		return trimOpenAIEndpointSuffix(path) + suffix
	})
}

func trimOpenAIEndpointSuffix(path string) string {
	for _, existing := range []string{"/chat/completions", "/responses", "/models"} {
		if strings.HasSuffix(path, existing) {
			return strings.TrimSuffix(path, existing)
		}
	}
	return path
}

// NormalizeOpenAIBaseURL strips a trailing /models, /chat/completions, or
// /responses path suffix from a configured endpoint while keeping an empty
// input empty so callers never accidentally substitute api.openai.com.
func NormalizeOpenAIBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	return strings.TrimRight(rewriteEndpointPath(trimmed, trimOpenAIEndpointSuffix), "/")
}

func NormalizeOpenAIModelsURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/models")
}

func NormalizeOpenAIChatCompletionsURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/chat/completions")
}

func NormalizeOpenAIResponsesURL(baseURL string) string {
	return normalizeOpenAIPath(baseURL, "/responses")
}
