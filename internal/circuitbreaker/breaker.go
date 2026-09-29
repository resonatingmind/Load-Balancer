// Package circuitbreaker implements a per-backend circuit breaker.
//
//	Closed ──(FailureThreshold consecutive failures)──► Open
//	Open ──(OpenTimeout elapsed, next request)──► HalfOpen
//	HalfOpen ──(HalfOpenRequests trials all succeed)──► Closed
//	HalfOpen ──(any trial fails)──► Open
//
// Health checks only notice a dead backend every few seconds. The breaker
// watches real traffic, so a failing backend is cut off within a handful of
// requests.
package circuitbreaker

import (
	"log/slog"
	"sync"
	"time"

	"l7lb/internal/config"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Result is the outcome of one request, reported back to the breaker.
type Result int

const (
	Success Result = iota
	Failure
	Ignore // e.g. the client hung up: says nothing about backend health
)

type Breaker struct {
	name             string
	failureThreshold int
	openTimeout      time.Duration
	halfOpenRequests int
	now              func() time.Time // swapped out in tests

	mu                sync.Mutex
	state             State
	generation        uint64 // bumped on every state change
	failures          int    // consecutive failures while closed
	openedAt          time.Time
	halfOpenInFlight  int
	halfOpenSuccesses int
}

// New creates a closed breaker. name is only used in logs.
func New(name string, cfg config.CircuitBreakerConfig) *Breaker {
	return &Breaker{
		name:             name,
		failureThreshold: cfg.FailureThreshold,
		openTimeout:      cfg.OpenTimeout.Duration,
		halfOpenRequests: cfg.HalfOpenRequests,
		now:              time.Now,
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Ready reports whether Allow would currently admit a request, without
// reserving anything. Strategies use it to skip backends while choosing.
func (b *Breaker) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		return b.cooldownElapsed()
	default:
		return b.halfOpenInFlight < b.halfOpenRequests
	}
}

// Allow asks to send one request. If ok, the caller must call done exactly
// once with the request's outcome. In the half-open state only
// HalfOpenRequests trial requests are admitted at a time.
func (b *Breaker) Allow() (done func(Result), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open {
		if !b.cooldownElapsed() {
			return nil, false
		}
		b.setState(HalfOpen)
	}
	if b.state == HalfOpen {
		if b.halfOpenInFlight >= b.halfOpenRequests {
			return nil, false
		}
		b.halfOpenInFlight++
	}

	gen := b.generation
	return func(r Result) { b.record(gen, r) }, true
}

func (b *Breaker) record(gen uint64, r Result) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The state changed while this request was in flight, so its outcome
	// belongs to a previous state (e.g. a slow request admitted while closed
	// finishing after the circuit opened). Counting it would corrupt the
	// current state's counters.
	if gen != b.generation {
		return
	}

	switch b.state {
	case Closed:
		switch r {
		case Success:
			b.failures = 0
		case Failure:
			b.failures++
			if b.failures >= b.failureThreshold {
				b.setState(Open)
			}
		}
	case HalfOpen:
		b.halfOpenInFlight--
		switch r {
		case Success:
			b.halfOpenSuccesses++
			if b.halfOpenSuccesses >= b.halfOpenRequests {
				b.setState(Closed)
			}
		case Failure:
			b.setState(Open)
		}
	}
}

func (b *Breaker) cooldownElapsed() bool {
	return !b.now().Before(b.openedAt.Add(b.openTimeout))
}

// setState must be called with b.mu held.
func (b *Breaker) setState(to State) {
	from := b.state
	b.state = to
	b.generation++
	b.failures = 0
	b.halfOpenInFlight = 0
	b.halfOpenSuccesses = 0
	if to == Open {
		b.openedAt = b.now()
	}

	if to == Open {
		slog.Warn("circuit breaker opened", "backend", b.name, "from", from.String(), "retry_after", b.openTimeout.String())
	} else {
		slog.Info("circuit breaker state change", "backend", b.name, "from", from.String(), "to", to.String())
	}
}
