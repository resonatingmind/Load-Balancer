package balancer

import (
	"fmt"
	"sync/atomic"

	"l7lb/internal/backend"
)

// Strategy picks the backend for the next request. Implementations must be
// safe for concurrent use and return nil when no backend is alive.
type Strategy interface {
	Next(backends []*backend.Backend) *backend.Backend
	Name() string
}

func NewStrategy(name string) (Strategy, error) {
	switch name {
	case "least_connections", "":
		return &LeastConnections{}, nil
	case "round_robin":
		return &RoundRobin{}, nil
	default:
		return nil, fmt.Errorf("unknown strategy %q (want least_connections or round_robin)", name)
	}
}

// RoundRobin cycles through alive backends in order.
type RoundRobin struct {
	counter atomic.Uint64
}

func (rr *RoundRobin) Name() string { return "round_robin" }

func (rr *RoundRobin) Next(backends []*backend.Backend) *backend.Backend {
	n := uint64(len(backends))
	if n == 0 {
		return nil
	}
	start := rr.counter.Add(1)
	for i := uint64(0); i < n; i++ {
		if b := backends[(start+i)%n]; b.IsAlive() {
			return b
		}
	}
	return nil
}

// LeastConnections picks the alive backend with the fewest in-flight requests.
// The scan starts at a rotating offset so ties (e.g. all idle) are spread
// across backends instead of always landing on the first one.
type LeastConnections struct {
	offset atomic.Uint64
}

func (lc *LeastConnections) Name() string { return "least_connections" }

func (lc *LeastConnections) Next(backends []*backend.Backend) *backend.Backend {
	n := len(backends)
	if n == 0 {
		return nil
	}
	start := int(lc.offset.Add(1) % uint64(n))

	var best *backend.Backend
	var bestConns int64
	for i := 0; i < n; i++ {
		b := backends[(start+i)%n]
		if !b.IsAlive() {
			continue
		}
		if c := b.ActiveConnections(); best == nil || c < bestConns {
			best, bestConns = b, c
		}
	}
	return best
}
