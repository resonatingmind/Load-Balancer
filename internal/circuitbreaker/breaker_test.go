package circuitbreaker

import (
	"testing"
	"time"

	"l7lb/internal/config"
)

// fakeClock lets tests move time forward instantly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(halfOpenRequests int) (*Breaker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	b := New("test", config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 3,
		OpenTimeout:      config.Duration{Duration: 10 * time.Second},
		HalfOpenRequests: halfOpenRequests,
	})
	b.now = clock.now
	return b, clock
}

// send runs one request through the breaker and reports r as its outcome.
func send(t *testing.T, b *Breaker, r Result) {
	t.Helper()
	done, ok := b.Allow()
	if !ok {
		t.Fatalf("request rejected in state %s", b.State())
	}
	done(r)
}

func wantState(t *testing.T, b *Breaker, want State) {
	t.Helper()
	if got := b.State(); got != want {
		t.Fatalf("state = %s, want %s", got, want)
	}
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreaker(1)

	send(t, b, Failure)
	send(t, b, Failure)
	send(t, b, Success) // resets the streak
	send(t, b, Failure)
	send(t, b, Failure)
	wantState(t, b, Closed)

	send(t, b, Failure)
	wantState(t, b, Open)
	if b.Ready() {
		t.Fatal("Ready() = true while open")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("Allow() admitted a request while open")
	}
}

func TestHalfOpenAfterCooldownThenCloses(t *testing.T) {
	b, clock := newTestBreaker(1)
	for i := 0; i < 3; i++ {
		send(t, b, Failure)
	}

	clock.advance(9 * time.Second)
	if b.Ready() {
		t.Fatal("Ready() before open_timeout elapsed")
	}

	clock.advance(1 * time.Second)
	if !b.Ready() {
		t.Fatal("not Ready() after open_timeout elapsed")
	}

	done, ok := b.Allow()
	if !ok {
		t.Fatal("trial request rejected after cooldown")
	}
	wantState(t, b, HalfOpen)

	// Only one trial may be in flight at a time.
	if b.Ready() {
		t.Fatal("Ready() while the only trial slot is taken")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("admitted a second concurrent trial")
	}

	done(Success)
	wantState(t, b, Closed)
}

func TestHalfOpenFailureReopensWithFreshCooldown(t *testing.T) {
	b, clock := newTestBreaker(1)
	for i := 0; i < 3; i++ {
		send(t, b, Failure)
	}
	clock.advance(10 * time.Second)

	send(t, b, Failure) // the trial fails
	wantState(t, b, Open)

	clock.advance(5 * time.Second)
	if b.Ready() {
		t.Fatal("cooldown was not restarted after the failed trial")
	}
	clock.advance(5 * time.Second)
	if !b.Ready() {
		t.Fatal("not Ready() after the second cooldown")
	}
}

func TestHalfOpenNeedsAllTrialsToSucceed(t *testing.T) {
	b, clock := newTestBreaker(2)
	for i := 0; i < 3; i++ {
		send(t, b, Failure)
	}
	clock.advance(10 * time.Second)

	send(t, b, Success)
	wantState(t, b, HalfOpen)
	send(t, b, Success)
	wantState(t, b, Closed)
}

func TestIgnoredResultFreesTrialSlot(t *testing.T) {
	b, clock := newTestBreaker(1)
	for i := 0; i < 3; i++ {
		send(t, b, Failure)
	}
	clock.advance(10 * time.Second)

	send(t, b, Ignore) // client hung up during the trial
	wantState(t, b, HalfOpen)
	if !b.Ready() {
		t.Fatal("trial slot not released after an ignored result")
	}
}

func TestStaleResultsAreIgnored(t *testing.T) {
	b, _ := newTestBreaker(1)

	// A slow request is admitted while closed...
	slowDone, ok := b.Allow()
	if !ok {
		t.Fatal("rejected while closed")
	}

	// ...the circuit opens meanwhile...
	for i := 0; i < 3; i++ {
		send(t, b, Failure)
	}
	wantState(t, b, Open)

	// ...and the slow request's late success must not close it again.
	slowDone(Success)
	wantState(t, b, Open)
}
