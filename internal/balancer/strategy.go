package balancer

import (
	"fmt"
	"sync/atomic"

	"l7lb/internal/backend"
)

// Strategy picks the backend for the next request. Implementations must be
// safe for concurrent use and return nil when no backend is available
// (healthy and with a closed or trial-ready circuit).
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
//
// It rotates over the alive backends only. Rotating over all backends and
// skipping dead ones would hand a dead backend's turn to its neighbour,
// giving that neighbour double traffic.
type RoundRobin struct {
	counter atomic.Uint64
}

func (rr *RoundRobin) Name() string { return "round_robin" }

func (rr *RoundRobin) Next(backends []*backend.Backend) *backend.Backend {
	alive := 0
	for _, b := range backends {
		if b.Available() {
			alive++
		}
	}
	if alive == 0 {
		return nil
	}
	return nthMatching(backends, int(rr.counter.Add(1)%uint64(alive)), func(b *backend.Backend) bool {
		return b.Available()
	})
}

// LeastConnections picks the alive backend with the fewest in-flight requests.
// Ties (e.g. every backend idle) are broken by rotating over the tied
// backends, so they share traffic evenly instead of the first one taking it all.
type LeastConnections struct {
	counter atomic.Uint64
}

func (lc *LeastConnections) Name() string { return "least_connections" }

func (lc *LeastConnections) Next(backends []*backend.Backend) *backend.Backend {
	// First pass: find the minimum connection count and how many share it.
	var minConns int64
	ties := 0
	for _, b := range backends {
		if !b.Available() {
			continue
		}
		c := b.ActiveConnections()
		switch {
		case ties == 0 || c < minConns:
			minConns, ties = c, 1
		case c == minConns:
			ties++
		}
	}
	if ties == 0 {
		return nil
	}

	// Second pass: pick one of the tied backends in rotation. Counts can move
	// between passes; if no backend matches any more, fall back to any alive one.
	k := int(lc.counter.Add(1) % uint64(ties))
	if b := nthMatching(backends, k, func(b *backend.Backend) bool {
		return b.Available() && b.ActiveConnections() == minConns
	}); b != nil {
		return b
	}
	return nthMatching(backends, 0, func(b *backend.Backend) bool { return b.Available() })
}

// nthMatching returns the k-th (0-based) backend for which match is true,
// or the last match if there are fewer than k+1, or nil if none match.
func nthMatching(backends []*backend.Backend, k int, match func(*backend.Backend) bool) *backend.Backend {
	var last *backend.Backend
	for _, b := range backends {
		if !match(b) {
			continue
		}
		if k == 0 {
			return b
		}
		k--
		last = b
	}
	return last
}
