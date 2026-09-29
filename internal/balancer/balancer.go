// Package balancer routes incoming requests to backends using a Strategy.
package balancer

import (
	"encoding/json"
	"net/http"

	"l7lb/internal/backend"
	"l7lb/internal/middleware"
)

type Options struct {
	// MaxRetries is how many times a failed idempotent request is retried,
	// each time on a backend it has not tried yet.
	MaxRetries int
}

type LoadBalancer struct {
	backends   []*backend.Backend
	strategy   Strategy
	maxRetries int
}

func New(backends []*backend.Backend, strategy Strategy, opts Options) *LoadBalancer {
	return &LoadBalancer{backends: backends, strategy: strategy, maxRetries: opts.MaxRetries}
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info := middleware.InfoFrom(r.Context())
	retryable := isRetryable(r)
	candidates := lb.backends
	failures := 0

	// Each pass removes the backend it used from the candidates, so the loop
	// ends after at most len(backends) passes. A pass is either:
	//   - Served: done.
	//   - Rejected: its breaker refused (e.g. another request took the only
	//     half-open trial slot). Nothing was written; pick again.
	//   - Failed: it errored and we held the error back to retry elsewhere.
	for {
		b := lb.strategy.Next(candidates)
		if b == nil {
			break
		}
		// Only hold a failure back if there is somewhere else to send the
		// retry; on the last attempt the backend's real response goes out.
		canRetry := retryable && failures < lb.maxRetries && anyOtherAvailable(candidates, b)

		outcome := b.Serve(w, r, canRetry)
		if outcome != backend.Rejected && info != nil {
			info.Backend = b.URL.Host
			info.Attempts++
		}
		switch outcome {
		case backend.Served:
			return
		case backend.Failed:
			failures++
		}
		candidates = without(candidates, b)
	}

	if failures > 0 {
		http.Error(w, "all backends failed", http.StatusBadGateway)
		return
	}
	http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
}

// isRetryable reports whether a request is safe to send twice: an idempotent
// method with no body (a consumed body can't be replayed).
func isRetryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	return r.ContentLength == 0 && (r.Body == nil || r.Body == http.NoBody)
}

func anyOtherAvailable(backends []*backend.Backend, exclude *backend.Backend) bool {
	for _, b := range backends {
		if b != exclude && b.Available() {
			return true
		}
	}
	return false
}

// without returns a copy of backends minus b. It only runs on a retry or a
// breaker rejection, never on the normal path, so the allocation is fine.
func without(backends []*backend.Backend, b *backend.Backend) []*backend.Backend {
	out := make([]*backend.Backend, 0, len(backends)-1)
	for _, c := range backends {
		if c != b {
			out = append(out, c)
		}
	}
	return out
}

type backendStatus struct {
	URL               string `json:"url"`
	Alive             bool   `json:"alive"`
	Circuit           string `json:"circuit"`
	ActiveConnections int64  `json:"active_connections"`
}

type status struct {
	Strategy string          `json:"strategy"`
	Backends []backendStatus `json:"backends"`
}

// StatusHandler serves a JSON snapshot of backend state for the admin port.
func (lb *LoadBalancer) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := status{Strategy: lb.strategy.Name(), Backends: make([]backendStatus, 0, len(lb.backends))}
		for _, b := range lb.backends {
			s.Backends = append(s.Backends, backendStatus{
				URL:               b.URL.String(),
				Alive:             b.IsAlive(),
				Circuit:           b.CircuitState(),
				ActiveConnections: b.ActiveConnections(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s)
	})
}
