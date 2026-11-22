package oauthproxy

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// upstreamFastRetryBackoff is the wait before each fast retry of a 429/5xx
// upstream outcome, shared by every GBX data plane. At most three attempts:
// the initial one plus one per entry. A longer Retry-After ends the fast loop
// immediately so the client can honor the upstream wait instead of GBX
// sending requests ahead of it. No long sleeps or account rotation are added.
// Var (not const) so tests can shorten it; no test in this package runs in
// parallel.
var upstreamFastRetryBackoff = []time.Duration{500 * time.Millisecond, time.Second}

// upstreamStatusError is implemented by each runtime's typed upstream error so
// retryUpstream can branch on the upstream status without knowing the runtime.
// Kiro does not implement it: its own callUpstream loop is its retry layer.
type upstreamStatusError interface {
	upstreamStatus() int
}

type upstreamRetryAfterError interface {
	upstreamRetryAfter() string
}

type upstreamBodyError interface {
	upstreamErrorBody() string
}

type upstreamHeadersError interface {
	upstreamHeaders() http.Header
}

// isFastRetryStatus reports whether an upstream HTTP status gets the fast
// retry treatment. 401/403 are excluded on purpose: each runtime refreshes
// OAuth on its own inside the attempt closure, and a refreshed 401 is terminal.
// Non-transient 5xx statuses such as 501 Not Implemented are also excluded.
func isFastRetryStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529:
		return true
	default:
		return false
	}
}

// retryUpstream runs attempt until it yields a non-retryable outcome or the
// fast retry budget is spent. A retryable outcome is a response with a
// transient 408/429/5xx status or an error carrying one. The LAST outcome is
// returned unchanged — response undrained, error as-is — so the caller's relay
// (status, body, Retry-After) stays intact.
func retryUpstream(ctx context.Context, component string, attempt func() (*http.Response, error)) (*http.Response, error) {
	backoff := upstreamFastRetryBackoff
	for index := 0; ; index++ {
		response, err := attempt()
		if !retryableUpstreamOutcome(response, err) || index >= len(backoff) {
			return response, err
		}
		baseDelay := backoff[index]
		if hint, ok := outcomeRetryAfterDelay(response, err); ok && hint > baseDelay {
			LogWarnEvent("upstream_retry", "component", component, "request_id", requestLogID(ctx),
				"status", outcomeStatus(response, err), "action", "relay_retry_after")
			return response, err
		}
		delay := jitterBackoffDelay(baseDelay)
		LogWarnEvent("upstream_retry", "component", component, "request_id", requestLogID(ctx),
			"status", outcomeStatus(response, err), "retry", index+1, "retry_limit", len(backoff),
			"wait", delay, "action", "fast_retry")
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			// The client gave up mid-backoff: hand back the untouched outcome —
			// the original 429/5xx, not the cancellation.
			return response, err
		}
		drainAndClose(response)
	}
}

func jitterBackoffDelay(base time.Duration) time.Duration {
	// Keep very short test backoffs deterministic while jittering real waits
	// (±20%) to prevent synchronized retry storms across concurrent subagents.
	if base < 50*time.Millisecond {
		return base
	}
	factor := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(base) * factor)
}

func outcomeRetryAfter(response *http.Response, err error) string {
	if response != nil {
		return response.Header.Get("Retry-After")
	}
	var hint upstreamRetryAfterError
	if errors.As(err, &hint) {
		return hint.upstreamRetryAfter()
	}
	return ""
}

func outcomeRetryAfterMS(response *http.Response, err error) string {
	if response != nil {
		return response.Header.Get("Retry-After-Ms")
	}
	var hdrErr upstreamHeadersError
	if errors.As(err, &hdrErr) && hdrErr.upstreamHeaders() != nil {
		return hdrErr.upstreamHeaders().Get("Retry-After-Ms")
	}
	return ""
}

func outcomeRetryAfterDelay(response *http.Response, err error) (time.Duration, bool) {
	if ms := strings.TrimSpace(outcomeRetryAfterMS(response, err)); ms != "" {
		if parsed, parseErr := strconv.ParseFloat(ms, 64); parseErr == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) && parsed >= 0 {
			const maxMS = float64(time.Duration(1<<63-1) / time.Millisecond)
			if parsed >= maxMS {
				return time.Duration(1<<63 - 1), true
			}
			return time.Duration(parsed * float64(time.Millisecond)), true
		}
	}
	return retryAfterDelay(outcomeRetryAfter(response, err))
}

func retryAfterDelay(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		const maxDelay = time.Duration(1<<63 - 1)
		if seconds > uint64(maxDelay/time.Second) {
			return maxDelay, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 {
		const maxSeconds = float64(time.Duration(1<<63-1) / time.Second)
		if seconds >= maxSeconds {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return time.Until(when), true
}

func retryableUpstreamOutcome(response *http.Response, err error) bool {
	status := outcomeStatus(response, err)
	if !isFastRetryStatus(status) {
		return false
	}
	if status == http.StatusTooManyRequests && err != nil {
		var bodyErr upstreamBodyError
		if errors.As(err, &bodyErr) && isNonRetryableQuotaBody(bodyErr.upstreamErrorBody()) {
			return false
		}
	}
	return true
}

// outcomeStatus reads the upstream status from either outcome shape: raw
// responses (passthrough returns non-2xx as responses) or the typed
// errors the converter runtimes drain into.
func outcomeStatus(response *http.Response, err error) int {
	if response != nil {
		return response.StatusCode
	}
	var statusErr upstreamStatusError
	if err != nil && errors.As(err, &statusErr) {
		return statusErr.upstreamStatus()
	}
	return 0
}

// sleepContext waits for delay or until ctx is cancelled, whichever comes
// first. Retries and other waits must go through it so a client abort stops
// the wait instead of running out the clock.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
