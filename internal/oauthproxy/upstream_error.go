package oauthproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const maxClientErrorMessageBytes = 4096

// errRequestBodyTooLarge marks an inbound HTTP request body exceeding its
// configured limit so handlers return HTTP 413 instead of 400.
var errRequestBodyTooLarge = errors.New("request body too large")

type requestBodyTooLargeError struct {
	limitLabel string
}

func (e *requestBodyTooLargeError) Error() string {
	return fmt.Sprintf("request body exceeds %s limit", e.limitLabel)
}

func (e *requestBodyTooLargeError) Unwrap() error {
	return errRequestBodyTooLarge
}

// writeInboundBodyError writes HTTP 413 for oversized inbound requests and
// HTTP 400 for other body read errors.
func writeInboundBodyError(writer http.ResponseWriter, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		writeProxyErrorDetail(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request_too_large", "", err.Error())
		return
	}
	writeProxyError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
}

// oauthAuthError marks an upstream OAuth credential/refresh rejection that
// should surface to the caller as HTTP 401 authentication_error rather than
// collapsing into HTTP 502.
type oauthAuthError struct {
	status  int
	message string
	err     error
}

func (e *oauthAuthError) Error() string {
	if e.message != "" {
		return e.message
	}
	if e.err != nil {
		return e.err.Error()
	}
	return "OAuth authentication failed"
}

func (e *oauthAuthError) Unwrap() error {
	return e.err
}

func wrapOAuthAuthorizeError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	var existing *oauthAuthError
	if errors.As(err, &existing) {
		return &oauthAuthError{
			status:  existing.status,
			message: fmt.Sprintf("%s: %v", prefix, err),
			err:     err,
		}
	}
	if isPermanentOAuthAuthFailure(err) {
		return &oauthAuthError{
			status:  http.StatusUnauthorized,
			message: fmt.Sprintf("%s: %v", prefix, err),
			err:     err,
		}
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func isPermanentOAuthAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid_grant",
		"invalid_token",
		"unauthorized_client",
		"access_denied",
		"no refresh token",
		"missing refresh token",
		"refresh token is empty",
		"credential is disabled",
		"disabled credential",
		"token revoked",
		"account deactivated",
		"status 401",
		"http 401",
		"status 403",
		"http 403",
		"status 400",
		"http 400",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// streamUpstreamError carries a structured error discovered inside an HTTP 200
// response body or stream (such as a JSON error envelope, SSE error event, or
// Kiro EventStream exception) so pre-stream gates can return accurate HTTP
// statuses and machine-readable error codes.
type streamUpstreamError struct {
	dialect    string
	status     int
	errType    string
	code       string
	param      string
	message    string
	body       string
	retryAfter string
	headers    http.Header
}

func (e *streamUpstreamError) Error() string {
	if e.message != "" {
		return e.message
	}
	if e.body != "" {
		return truncateErrorMessage(e.body, maxClientErrorMessageBytes)
	}
	return "upstream stream error"
}

func (e *streamUpstreamError) upstreamStatus() int {
	return e.status
}

func (e *streamUpstreamError) upstreamRetryAfter() string {
	return e.retryAfter
}

// normalizedProxyError is the canonical OpenAI-shaped error returned by GBX.
type normalizedProxyError struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
}

// normalizeUpstreamError extracts machine-readable error metadata from an
// upstream error response body across OpenAI, Anthropic, Kiro, Qoder, Cursor,
// and WorkBuddy dialects while keeping diagnostic text bounded.
func normalizeUpstreamError(dialect string, status int, body, fallbackMsg string) normalizedProxyError {
	if status <= 0 {
		status = http.StatusBadGateway
	}
	trimmedBody := strings.TrimSpace(body)
	rawCode, rawType, rawParam, rawMsg := parseUpstreamErrorFields(trimmedBody)

	code := canonicalErrorCode(dialect, status, rawCode, rawType, firstNonEmptyString(rawMsg, trimmedBody, fallbackMsg))
	errType := proxyErrorType(status)
	if code == "context_length_exceeded" && status == http.StatusBadRequest {
		errType = "invalid_request_error"
	} else if code == "insufficient_quota" && status == http.StatusTooManyRequests {
		errType = "insufficient_quota"
	} else if code == "rate_limit_exceeded" && (status == http.StatusTooManyRequests || status == 529) {
		errType = "rate_limit_error"
	} else if rawType != "" && isKnownProxyErrorType(rawType) && status < 500 {
		errType = rawType
	}

	message := strings.TrimSpace(fallbackMsg)
	if message == "" {
		message = firstNonEmptyString(rawMsg, trimmedBody, http.StatusText(status))
	}
	message = truncateErrorMessage(message, maxClientErrorMessageBytes)
	return normalizedProxyError{
		Status:  status,
		Type:    errType,
		Code:    code,
		Param:   rawParam,
		Message: message,
	}
}

func isKnownProxyErrorType(errType string) bool {
	switch errType {
	case "invalid_request_error", "authentication_error", "permission_error",
		"not_found_error", "request_too_large", "rate_limit_error",
		"insufficient_quota", "request_timeout_error", "overloaded_error",
		"api_error", "server_error":
		return true
	default:
		return false
	}
}

func parseUpstreamErrorFields(body string) (code, errType, param, message string) {
	if body == "" || !strings.HasPrefix(body, "{") {
		return "", "", "", ""
	}
	raw := []byte(body)
	if !gjson.ValidBytes(raw) {
		return "", "", "", ""
	}
	if errObj := gjson.GetBytes(raw, "error"); errObj.IsObject() {
		code = cleanJSONErrorString(errObj.Get("code"))
		errType = cleanJSONErrorString(errObj.Get("type"))
		param = cleanJSONErrorString(errObj.Get("param"))
		message = cleanJSONErrorString(errObj.Get("message"))
		if message == "" {
			message = cleanJSONErrorString(errObj.Get("msg"))
		}
		return code, errType, param, message
	} else if errObj.Type == gjson.String {
		message = strings.TrimSpace(errObj.String())
	}
	// Flat envelope (Responses SSE error, AWS/Kiro exception JSON, WorkBuddy/OneAPI).
	code = firstNonEmptyString(
		cleanJSONErrorString(gjson.GetBytes(raw, "code")),
		cleanJSONErrorString(gjson.GetBytes(raw, "__type")),
		cleanJSONErrorString(gjson.GetBytes(raw, "reason")),
	)
	errType = cleanJSONErrorString(gjson.GetBytes(raw, "type"))
	param = cleanJSONErrorString(gjson.GetBytes(raw, "param"))
	message = firstNonEmptyString(
		message,
		cleanJSONErrorString(gjson.GetBytes(raw, "message")),
		cleanJSONErrorString(gjson.GetBytes(raw, "msg")),
		cleanJSONErrorString(gjson.GetBytes(raw, "detail")),
		cleanJSONErrorString(gjson.GetBytes(raw, "error_description")),
	)
	return code, errType, param, message
}

func cleanJSONErrorString(res gjson.Result) string {
	if !res.Exists() || res.Type == gjson.Null {
		return ""
	}
	switch res.Type {
	case gjson.String:
		v := strings.TrimSpace(res.String())
		if strings.EqualFold(v, "null") {
			return ""
		}
		return v
	case gjson.Number:
		return strings.TrimSpace(res.Raw)
	default:
		return ""
	}
}

func canonicalErrorCode(dialect string, status int, rawCode, rawType, text string) string {
	lowerCode := strings.ToLower(strings.TrimSpace(rawCode))
	lowerType := strings.ToLower(strings.TrimSpace(rawType))
	lowerText := strings.ToLower(text)

	// 1. Context length overflow — Codex CLI depends on this exact code for auto-compact.
	switch lowerCode {
	case "context_length_exceeded", "string_above_max_length", "contentlengthexceededexception",
		"input_too_long", "context_window_exceeded", "prompt_too_long":
		return "context_length_exceeded"
	}
	for _, needle := range []string{
		"context_length_exceeded",
		"maximum context length",
		"prompt is too long",
		"input is too long",
		"exceeds the context window",
		"exceed context limit",
		"exceeds context length",
		"context window exceeded",
		"too many input tokens",
		"too many tokens",
		"contentlengthexceededexception",
	} {
		if strings.Contains(lowerText, needle) {
			return "context_length_exceeded"
		}
	}

	// 2. Permanent quota / billing exhaustion (must precede generic 429 rate_limit_exceeded).
	if isPermanentQuotaSignal(lowerCode, lowerType, lowerText) {
		return "insufficient_quota"
	}

	// 3. Rate limit / overload.
	switch lowerCode {
	case "rate_limit_exceeded", "rate_limit_error", "overloaded_error",
		"throttlingexception", "resource_limit", "resource_exhausted", "too_many_requests":
		return "rate_limit_exceeded"
	}
	if lowerType == "rate_limit_error" || lowerType == "overloaded_error" ||
		status == http.StatusTooManyRequests || status == 529 {
		return "rate_limit_exceeded"
	}

	// 4. Authentication.
	switch lowerCode {
	case "invalid_api_key", "authentication_error", "unauthenticated", "expired_token", "token_expired":
		if lowerCode == "invalid_api_key" || lowerCode == "expired_token" || lowerCode == "token_expired" {
			return lowerCode
		}
		return "invalid_api_key"
	}
	if status == http.StatusUnauthorized || lowerType == "authentication_error" {
		if rawCode != "" && !strings.EqualFold(rawCode, "error") {
			return rawCode
		}
		return "invalid_api_key"
	}

	// 5. Model not found.
	if lowerCode == "model_not_found" || (status == http.StatusNotFound && strings.Contains(lowerText, "model")) {
		return "model_not_found"
	}

	// 6. Request entity too large.
	if status == http.StatusRequestEntityTooLarge {
		if rawCode != "" {
			return rawCode
		}
		return "request_too_large"
	}

	// 7. Preserve explicit upstream machine-readable code if present.
	if rawCode != "" && !strings.EqualFold(rawCode, "error") {
		return rawCode
	}

	// 8. Server errors.
	if status >= http.StatusInternalServerError {
		return "server_error"
	}
	_ = dialect
	return ""
}

func isPermanentQuotaSignal(lowerCode, lowerType, lowerText string) bool {
	switch lowerCode {
	case "insufficient_quota", "billing_hard_limit_reached", "credit_balance_too_low",
		"monthly_request_count", "quota_exceeded", "insufficient_balance":
		return true
	}
	if lowerType == "insufficient_quota" {
		return true
	}
	for _, needle := range []string{
		"insufficient_quota",
		"billing_hard_limit_reached",
		"credit balance is too low",
		"credit_balance_too_low",
		"monthly_request_count",
		"exceeded your current quota",
		"insufficient balance",
	} {
		if strings.Contains(lowerText, needle) {
			return true
		}
	}
	return false
}

// isNonRetryableQuotaBody reports whether an HTTP 429 error body indicates
// permanent quota/billing exhaustion that fast retries cannot recover from.
func isNonRetryableQuotaBody(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	code, errType, _, msg := parseUpstreamErrorFields(strings.TrimSpace(body))
	return isPermanentQuotaSignal(strings.ToLower(code), strings.ToLower(errType), strings.ToLower(firstNonEmptyString(msg, body)))
}

// detectJSONErrorEnvelope inspects an HTTP 200 JSON payload for upstream error
// envelopes (OpenRouter, OneAPI, WorkBuddy, Responses status="failed", etc.).
// Returns nil for normal non-error payloads.
func detectJSONErrorEnvelope(body []byte, dialect string) *streamUpstreamError {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !gjson.ValidBytes(trimmed) {
		return nil
	}
	// OpenAI Responses non-streaming failed response object.
	if gjson.GetBytes(trimmed, "object").String() == "response" {
		if strings.EqualFold(gjson.GetBytes(trimmed, "status").String(), "failed") {
			errObj := gjson.GetBytes(trimmed, "error")
			errBody := string(trimmed)
			if errObj.IsObject() {
				errBody = errObj.Raw
			}
			return buildStreamUpstreamError(dialect, errBody, "")
		}
		return nil
	}
	// Explicit {"error": ...} or {"type":"error", ...}.
	if errVal := gjson.GetBytes(trimmed, "error"); errVal.Exists() && errVal.Type != gjson.Null {
		if errVal.IsObject() || (errVal.Type == gjson.String && strings.TrimSpace(errVal.String()) != "") {
			return buildStreamUpstreamError(dialect, string(trimmed), "")
		}
	}
	if strings.EqualFold(gjson.GetBytes(trimmed, "type").String(), "error") {
		return buildStreamUpstreamError(dialect, string(trimmed), "")
	}
	// Gateway business-error envelope without choices/output (e.g. WorkBuddy/OneAPI {"code":11101,"msg":"..."}).
	if !gjson.GetBytes(trimmed, "choices").Exists() && !gjson.GetBytes(trimmed, "output").Exists() && !gjson.GetBytes(trimmed, "content").Exists() {
		codeVal := gjson.GetBytes(trimmed, "code")
		msgVal := gjson.GetBytes(trimmed, "msg")
		if !msgVal.Exists() {
			msgVal = gjson.GetBytes(trimmed, "message")
		}
		if codeVal.Exists() && codeVal.Type != gjson.Null && msgVal.Exists() && strings.TrimSpace(msgVal.String()) != "" {
			nonZero := (codeVal.Type == gjson.Number && codeVal.Int() != 0 && codeVal.Int() != 200) ||
				(codeVal.Type == gjson.String && codeVal.String() != "" && codeVal.String() != "0" && !strings.EqualFold(codeVal.String(), "ok") && !strings.EqualFold(codeVal.String(), "success"))
			if nonZero {
				return buildStreamUpstreamError(dialect, string(trimmed), "")
			}
		}
	}
	return nil
}

func buildStreamUpstreamError(dialect, body, fallbackMsg string) *streamUpstreamError {
	rawCode, rawType, rawParam, rawMsg := parseUpstreamErrorFields(strings.TrimSpace(body))
	status := inferErrorStatusFromFields(rawCode, rawType, firstNonEmptyString(rawMsg, body, fallbackMsg))
	norm := normalizeUpstreamError(dialect, status, body, firstNonEmptyString(fallbackMsg, rawMsg, body))
	if norm.Param == "" {
		norm.Param = rawParam
	}
	return &streamUpstreamError{
		dialect: dialect,
		status:  norm.Status,
		errType: norm.Type,
		code:    norm.Code,
		param:   norm.Param,
		message: norm.Message,
		body:    strings.TrimSpace(body),
	}
}

func inferErrorStatusFromFields(rawCode, rawType, text string) int {
	lowerCode := strings.ToLower(strings.TrimSpace(rawCode))
	lowerType := strings.ToLower(strings.TrimSpace(rawType))
	lowerText := strings.ToLower(text)

	if isPermanentQuotaSignal(lowerCode, lowerType, lowerText) {
		return http.StatusTooManyRequests
	}
	switch lowerCode {
	case "rate_limit_exceeded", "rate_limit_error", "throttlingexception", "resource_limit", "resource_exhausted", "too_many_requests":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	case "invalid_api_key", "authentication_error", "unauthenticated", "expired_token", "token_expired":
		return http.StatusUnauthorized
	case "permission_denied", "permission_error":
		return http.StatusForbidden
	case "model_not_found", "not_found", "not_found_error":
		return http.StatusNotFound
	case "context_length_exceeded", "string_above_max_length", "contentlengthexceededexception",
		"input_too_long", "invalid_request_error", "invalid_argument", "validationexception":
		return http.StatusBadRequest
	}
	switch lowerType {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "invalid_request_error":
		return http.StatusBadRequest
	}
	for _, needle := range []string{"context_length_exceeded", "maximum context length", "prompt is too long", "input is too long", "contentlengthexceededexception"} {
		if strings.Contains(lowerText, needle) {
			return http.StatusBadRequest
		}
	}
	if strings.Contains(lowerText, "rate limit") || strings.Contains(lowerText, "too many requests") {
		return http.StatusTooManyRequests
	}
	if strings.Contains(lowerText, "overloaded") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

// writeProxyErrorDetail writes an OpenAI-shaped JSON error body preserving
// optional machine-readable code and param fields.
func writeProxyErrorDetail(writer http.ResponseWriter, status int, errorType, code, param, message string) {
	writer.Header().Del("Cache-Control")
	writer.Header().Del("Connection")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	var codeValue any
	if code != "" {
		codeValue = code
	}
	var paramValue any
	if param != "" {
		paramValue = param
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"message": truncateErrorMessage(message, maxClientErrorMessageBytes),
			"type":    errorType,
			"code":    codeValue,
			"param":   paramValue,
		},
	})
}

// writeStreamFailure handles an error returned from a streaming or non-streaming
// decoder. Before SSE output begins it writes an HTTP error with normalized
// status/code; once SSE output has begun it emits response.failed with the
// normalized code. Client cancellation (context.Canceled) is treated quietly.
func writeStreamFailure(writer http.ResponseWriter, assembler *responseAssembler, streaming bool, dialect string, err error) {
	if err == nil {
		return
	}
	var streamErr *streamUpstreamError
	if errors.As(err, &streamErr) {
		if !streaming || !assembler.started {
			copyProxyResponseHeaders(writer.Header(), streamErr.headers)
			if streamErr.retryAfter != "" {
				writer.Header().Set("Retry-After", streamErr.retryAfter)
			}
			status := streamErr.status
			if status <= 0 {
				status = http.StatusBadGateway
			}
			errType := firstNonEmptyString(streamErr.errType, proxyErrorType(status))
			writeProxyErrorDetail(writer, status, errType, streamErr.code, streamErr.param, streamErr.Error())
			return
		}
		code := firstNonEmptyString(streamErr.code, streamErr.errType, "upstream_stream_error")
		assembler.fail(code, streamErr.Error())
		return
	}
	if !streaming || !assembler.started {
		norm := normalizeUpstreamError(dialect, http.StatusBadGateway, "", err.Error())
		writeProxyErrorDetail(writer, norm.Status, norm.Type, norm.Code, norm.Param, norm.Message)
		return
	}
	assembler.fail("upstream_stream_error", truncateErrorMessage(err.Error(), maxClientErrorMessageBytes))
}

// extractProxyResponseHeaders copies rate-limit, retry, and request-id headers
// from an upstream HTTP response so they survive error draining and proxying.
func extractProxyResponseHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	out := make(http.Header)
	copyProxyResponseHeaders(out, src)
	if len(out) == 0 {
		return nil
	}
	return out
}

// copyProxyResponseHeaders relays upstream retry, request-id, and rate-limit
// metadata headers onto dst without overwriting existing non-empty values.
func copyProxyResponseHeaders(dst, src http.Header) {
	if dst == nil || len(src) == 0 {
		return
	}
	for key, values := range src {
		if len(values) == 0 {
			continue
		}
		if !isRelayedProxyHeader(key) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				dst.Set(key, value)
				break
			}
		}
	}
}

func isRelayedProxyHeader(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "retry-after", "retry-after-ms", "x-request-id", "request-id":
		return true
	}
	return strings.HasPrefix(lower, "x-ratelimit-") ||
		strings.HasPrefix(lower, "anthropic-ratelimit-") ||
		strings.HasPrefix(lower, "x-codex-")
}

func truncateErrorMessage(message string, maxBytes int) string {
	if maxBytes <= 0 || len(message) <= maxBytes {
		return message
	}
	const suffix = "... [truncated]"
	if maxBytes <= len(suffix) {
		return message[:maxBytes]
	}
	cut := maxBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + suffix
}
