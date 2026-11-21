package oauthproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type disableHistoryCacheKey struct{}

func withDisableHistoryCache(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, disableHistoryCacheKey{}, true)
}

func historyCacheDisabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(disableHistoryCacheKey{}).(bool)
	return disabled
}

// startLoopbackServer binds 127.0.0.1:0, serves handler with standard timeouts
// and context-driven shutdown, and returns the initialized Runtime.
func startLoopbackServer(parent context.Context, label string, handler http.Handler, apiKey string, usage *UsageTracker) (*Runtime, error) {
	if parent == nil {
		parent = context.Background()
	}
	listener, err := net.Listen("tcp", runtimeLoopbackHost+":0")
	if err != nil {
		return nil, fmt.Errorf("listen for %s: %w", label, err)
	}
	runCtx, cancel := context.WithCancel(parent)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return runCtx },
	}
	rt := &Runtime{
		endpoint:   "http://" + listener.Addr().String() + "/v1",
		apiKey:     apiKey,
		handler:    handler,
		httpServer: server,
		cancel:     cancel,
		done:       make(chan struct{}),
		usage:      usage,
	}
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		if err != nil {
			LogErrorf("%s server stopped endpoint=%q error=%v", label, SafeLogEndpoint(rt.endpoint), err)
		}
		close(rt.done)
	}()
	go func() {
		select {
		case <-runCtx.Done():
			ctx, stop := context.WithTimeout(context.Background(), runtimeStopTimeout)
			_ = server.Shutdown(ctx)
			stop()
		case <-rt.done:
		}
	}()
	return rt, nil
}

// nextSSEFrameBoundary locates the earliest SSE blank-line separator (\n\n or
// \r\n\r\n) in buf and returns its starting offset and separator byte length.
func nextSSEFrameBoundary(buf []byte) (idx, sepLen int) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case lf >= 0 && (crlf < 0 || lf < crlf):
		return lf, 2
	default:
		return crlf, 4
	}
}
