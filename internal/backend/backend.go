// Package backend represents a single upstream server and its live state.
package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	URL   *url.URL
	proxy *httputil.ReverseProxy

	alive       atomic.Bool
	activeConns atomic.Int64
}

// New creates a backend that forwards requests to rawURL using the shared
// transport and buffer pool. bufPool may be nil.
func New(rawURL string, transport http.RoundTripper, bufPool httputil.BufferPool) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend url %q must be http(s)://host[:port]", rawURL)
	}

	b := &Backend{URL: u}
	b.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
		},
		Transport:    transport,
		BufferPool:   bufPool,
		ErrorHandler: b.handleError,
	}
	b.alive.Store(true)
	return b, nil
}

// ServeHTTP forwards the request and tracks it as an active connection,
// which is what the least-connections strategy reads.
func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.activeConns.Add(1)
	defer b.activeConns.Add(-1)
	b.proxy.ServeHTTP(w, r)
}

func (b *Backend) handleError(w http.ResponseWriter, r *http.Request, err error) {
	// A client hanging up mid-request is not a backend failure.
	if !errors.Is(err, context.Canceled) {
		slog.Warn("proxy error", "backend", b.URL.String(), "path", r.URL.Path, "err", err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

func (b *Backend) IsAlive() bool { return b.alive.Load() }

func (b *Backend) SetAlive(alive bool) { b.alive.Store(alive) }

func (b *Backend) ActiveConnections() int64 { return b.activeConns.Load() }
