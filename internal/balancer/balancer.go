// Package balancer routes incoming requests to backends using a Strategy.
package balancer

import (
	"encoding/json"
	"net/http"

	"l7lb/internal/backend"
)

type LoadBalancer struct {
	backends []*backend.Backend
	strategy Strategy
}

func New(backends []*backend.Backend, strategy Strategy) *LoadBalancer {
	return &LoadBalancer{backends: backends, strategy: strategy}
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := lb.strategy.Next(lb.backends)
	if b == nil {
		http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
		return
	}
	b.ServeHTTP(w, r)
}

type backendStatus struct {
	URL               string `json:"url"`
	Alive             bool   `json:"alive"`
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
				ActiveConnections: b.ActiveConnections(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s)
	})
}
