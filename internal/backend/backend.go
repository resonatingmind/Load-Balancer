// Package backend represents a single upstream server and its live state.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"

	"l7lb/internal/circuitbreaker"
)

// Outcome says what happened when a backend was asked to serve a request.
type Outcome int

const (
	// Served: a response (successful or not) was written to the client.
	Served Outcome = iota
	// Rejected: the circuit breaker refused the request. Nothing was written.
	Rejected
	// Failed: the backend failed and, because the caller allowed a retry, the
	// failure was held back instead of being written. Nothing was written.
	Failed
)

type Backend struct {
	URL     *url.URL
	proxy   *httputil.ReverseProxy
	breaker *circuitbreaker.Breaker // nil when circuit breaking is disabled

	alive       atomic.Bool
	activeConns atomic.Int64
}

// New creates a backend that forwards requests to rawURL using the shared
// transport and buffer pool. bufPool and breaker may be nil.
func New(rawURL string, transport http.RoundTripper, bufPool httputil.BufferPool, breaker *circuitbreaker.Breaker) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend url %q must be http(s)://host[:port]", rawURL)
	}

	b := &Backend{URL: u, breaker: breaker}
	b.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
		},
		Transport:      transport,
		BufferPool:     bufPool,
		ModifyResponse: b.modifyResponse,
		ErrorHandler:   b.handleError,
	}
	b.alive.Store(true)
	return b, nil
}

// attempt carries per-request retry state through the shared ReverseProxy
// (via the request context) to its ModifyResponse and ErrorHandler hooks.
type attempt struct {
	canRetry bool
	failed   bool // set when a failure was held back for a retry
}

type attemptKey struct{}

func attemptFrom(r *http.Request) *attempt {
	a, _ := r.Context().Value(attemptKey{}).(*attempt)
	return a
}

// Serve forwards the request. With canRetry set, a connection error or 5xx
// response is not written to the client; Serve returns Failed so the caller
// can try another backend. The request is tracked as an active connection,
// which is what the least-connections strategy reads.
func (b *Backend) Serve(w http.ResponseWriter, r *http.Request, canRetry bool) Outcome {
	var done func(circuitbreaker.Result)
	if b.breaker != nil {
		var ok bool
		if done, ok = b.breaker.Allow(); !ok {
			return Rejected
		}
	}

	b.activeConns.Add(1)
	defer b.activeConns.Add(-1)

	at := &attempt{canRetry: canRetry}
	sw := &statusWriter{ResponseWriter: w}
	b.proxy.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), attemptKey{}, at)))

	if at.failed {
		if done != nil {
			done(circuitbreaker.Failure)
		}
		if !sw.wrote {
			return Failed
		}
		// An informational 1xx already reached the client, so it is too late
		// to retry elsewhere. Finish the response as a normal proxy error.
		sw.WriteHeader(http.StatusBadGateway)
		return Served
	}

	if done != nil {
		done(classify(r, sw.status))
	}
	return Served
}

func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b.Serve(w, r, false) == Rejected {
		http.Error(w, "backend circuit open", http.StatusServiceUnavailable)
	}
}

// modifyResponse turns a 5xx into a proxy error when a retry is allowed.
// ReverseProxy then closes the response body and calls handleError before
// anything is copied to the client.
func (b *Backend) modifyResponse(resp *http.Response) error {
	if at := attemptFrom(resp.Request); at != nil && at.canRetry && resp.StatusCode >= 500 {
		return fmt.Errorf("backend returned status %d", resp.StatusCode)
	}
	return nil
}

func (b *Backend) handleError(w http.ResponseWriter, r *http.Request, err error) {
	// A client hanging up mid-request is not a backend failure, and there is
	// no one left to retry for.
	clientGone := r.Context().Err() != nil

	if at := attemptFrom(r); at != nil && at.canRetry && !clientGone {
		at.failed = true
		slog.Warn("backend request failed, retrying on another backend",
			"backend", b.URL.String(), "path", r.URL.Path, "request_id", r.Header.Get("X-Request-ID"), "err", err)
		return
	}

	if !clientGone {
		slog.Warn("proxy error",
			"backend", b.URL.String(), "path", r.URL.Path, "request_id", r.Header.Get("X-Request-ID"), "err", err)
	}
	w.WriteHeader(http.StatusBadGateway)
}

// classify turns a finished request into a circuit breaker result. Proxy
// errors are written as 502 by handleError, so they count as 5xx too.
func classify(r *http.Request, status int) circuitbreaker.Result {
	if r.Context().Err() != nil {
		return circuitbreaker.Ignore // the client went away; not the backend's fault
	}
	if status >= 500 {
		return circuitbreaker.Failure
	}
	return circuitbreaker.Success
}

func (b *Backend) IsAlive() bool { return b.alive.Load() }

func (b *Backend) SetAlive(alive bool) { b.alive.Store(alive) }

func (b *Backend) ActiveConnections() int64 { return b.activeConns.Load() }

// Available reports whether the backend should receive new requests: it
// passes health checks and its circuit is not open.
func (b *Backend) Available() bool {
	return b.IsAlive() && (b.breaker == nil || b.breaker.Ready())
}

// CircuitState is the breaker state for status output.
func (b *Backend) CircuitState() string {
	if b.breaker == nil {
		return "disabled"
	}
	return b.breaker.State().String()
}

// statusWriter records what was written to the client.
type statusWriter struct {
	http.ResponseWriter
	status int  // final (non-1xx) status code
	wrote  bool // anything at all, including a 1xx, was written
}

func (w *statusWriter) WriteHeader(code int) {
	w.wrote = true
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	w.wrote = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// ReverseProxy needs for flushing streamed responses and WebSocket upgrades.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
