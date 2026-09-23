package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"l7lb/internal/backend"
	"l7lb/internal/config"
)

func testConfig() config.HealthCheckConfig {
	return config.HealthCheckConfig{
		Enabled:            true,
		Path:               "/health",
		Interval:           config.Duration{Duration: 10 * time.Millisecond},
		Timeout:            config.Duration{Duration: 100 * time.Millisecond},
		HealthyThreshold:   2,
		UnhealthyThreshold: 3,
	}
}

func mustBackend(t *testing.T, rawURL string) *backend.Backend {
	t.Helper()
	b, err := backend.New(rawURL, http.DefaultTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestRecordUsesThresholds(t *testing.T) {
	c := New(nil, testConfig())
	b := mustBackend(t, "http://b:1")
	tr := &tracker{}
	fail := errors.New("boom")

	// Two failures are below the unhealthy threshold of 3.
	c.record(tr, b, fail)
	c.record(tr, b, fail)
	if !b.IsAlive() {
		t.Fatal("marked down before reaching unhealthy_threshold")
	}

	// A success resets the failure streak.
	c.record(tr, b, nil)
	c.record(tr, b, fail)
	c.record(tr, b, fail)
	if !b.IsAlive() {
		t.Fatal("failure streak was not reset by a success")
	}

	c.record(tr, b, fail)
	if b.IsAlive() {
		t.Fatal("not marked down after 3 consecutive failures")
	}

	// One success is below the healthy threshold of 2.
	c.record(tr, b, nil)
	if b.IsAlive() {
		t.Fatal("marked up before reaching healthy_threshold")
	}
	c.record(tr, b, nil)
	if !b.IsAlive() {
		t.Fatal("not marked up after 2 consecutive successes")
	}
}

func TestCheckerMarksBackendDownAndRecovers(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New([]*backend.Backend{b}, testConfig()).Run(ctx)
		close(done)
	}()

	healthy.Store(false)
	waitFor(t, "backend marked down", func() bool { return !b.IsAlive() })

	healthy.Store(true)
	waitFor(t, "backend marked up", b.IsAlive)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestCheckerMarksUnreachableBackendDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening on this address any more

	b := mustBackend(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go New([]*backend.Backend{b}, testConfig()).Run(ctx)

	waitFor(t, "unreachable backend marked down", func() bool { return !b.IsAlive() })
}
