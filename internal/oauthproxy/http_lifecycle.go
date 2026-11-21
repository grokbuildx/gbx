package oauthproxy

import (
	"net"
	"net/http"
	"time"
)

func newUpstreamTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, ResponseHeaderTimeout: headerTimeout,
	}
}

type idleCloser interface {
	CloseIdleConnections()
}

func newCredentialHTTPClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     newUpstreamTransport(headerTimeout),
		CheckRedirect: checkCredentialRedirect,
	}
}

func newTimedCredentialHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: checkCredentialRedirect,
	}
}

func closeIdleClients(clients ...*http.Client) func() {
	return func() {
		for _, client := range clients {
			if client != nil {
				client.CloseIdleConnections()
			}
		}
	}
}

func authorizerHTTPClient(authorizer any) *http.Client {
	switch a := authorizer.(type) {
	case *codexOAuthAuthorizer:
		return a.client
	case *xaiOAuthAuthorizer:
		return a.client
	case *kimiOAuthAuthorizer:
		return a.client
	case *autoClawOAuthAuthorizer:
		return a.client
	case *cursorOAuthAuthorizer:
		return a.client
	case *workbuddyCredentialStore:
		return a.client
	case *qoderCredentialPool:
		return a.client
	case *kiroCredentialPool:
		return a.client
	default:
		return nil
	}
}

func closeIdleResource(resource any) func() {
	return func() {
		if closer, ok := resource.(idleCloser); ok && closer != nil {
			closer.CloseIdleConnections()
			return
		}
		if client := authorizerHTTPClient(resource); client != nil {
			client.CloseIdleConnections()
		}
	}
}
