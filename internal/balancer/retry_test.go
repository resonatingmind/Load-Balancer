package balancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"l7lb/internal/backend"
	"l7lb/internal/circuitbreaker"
	"l7lb/internal/config"
)

// failingServer answers every request with 500 and counts how many it got.
func failingServer(t *testing.T, hits *atomic.Int64) *backend.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom from backend", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return mustBackend(t, srv.URL)
}

// deadBackend points at an address nothing listens on (connection refused).
func deadBackend(t *testing.T) *backend.Backend {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return mustBackend(t, url)
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body == "" {
		req.Body = http.NoBody
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRetryHidesBackendErrors(t *testing.T) {
	cases := map[string]func(t *testing.T) *backend.Backend{
		"5xx response":       func(t *testing.T) *backend.Backend { return failingServer(t, new(atomic.Int64)) },
		"connection refused": deadBackend,
	}
	for name, makeBad := range cases {
		t.Run(name, func(t *testing.T) {
			lb := New([]*backend.Backend{makeBad(t), namedServer(t, "good")}, &RoundRobin{}, Options{MaxRetries: 2})
			front := httptest.NewServer(lb)
			defer front.Close()

			for i := 0; i < 10; i++ {
				if status, body := do(t, http.MethodGet, front.URL, ""); status != http.StatusOK || body != "good" {
					t.Fatalf("request %d: got %d %q, want 200 \"good\"", i, status, body)
				}
			}
		})
	}
}

func TestNonIdempotentRequestsAreNotRetried(t *testing.T) {
	var badHits atomic.Int64
	lb := New([]*backend.Backend{failingServer(t, &badHits), namedServer(t, "good")}, &RoundRobin{}, Options{MaxRetries: 2})
	front := httptest.NewServer(lb)
	defer front.Close()

	// Round-robin alternates, so of 4 POSTs, 2 land on the failing backend.
	// They must not be replayed: a POST may have had side effects.
	failed := 0
	for i := 0; i < 4; i++ {
		if status, _ := do(t, http.MethodPost, front.URL, `{"order":1}`); status != http.StatusOK {
			failed++
		}
	}
	if failed != 2 || badHits.Load() != 2 {
		t.Fatalf("failed=%d badHits=%d, want 2 and 2 (no retries for POST)", failed, badHits.Load())
	}
}

func TestLastAttemptPassesRealErrorThrough(t *testing.T) {
	var hits atomic.Int64
	bs := []*backend.Backend{failingServer(t, &hits), failingServer(t, &hits), failingServer(t, &hits)}
	lb := New(bs, &RoundRobin{}, Options{MaxRetries: 1})
	front := httptest.NewServer(lb)
	defer front.Close()

	status, body := do(t, http.MethodGet, front.URL, "")
	// 1 try + 1 retry, then the second backend's own 500 goes to the client.
	if status != http.StatusInternalServerError || !strings.Contains(body, "boom from backend") {
		t.Fatalf("got %d %q, want the backend's 500", status, body)
	}
	if hits.Load() != 2 {
		t.Fatalf("backends hit %d times, want 2 (max_retries=1)", hits.Load())
	}
}

func TestRetriedFailuresStillTripTheBreaker(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()

	cb := circuitbreaker.New("bad", config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 3,
		OpenTimeout:      config.Duration{Duration: time.Minute},
		HalfOpenRequests: 1,
	})
	bad, err := backend.New(failing.URL, http.DefaultTransport, nil, cb)
	if err != nil {
		t.Fatal(err)
	}
	lb := New([]*backend.Backend{bad, namedServer(t, "good")}, &RoundRobin{}, Options{MaxRetries: 2})
	front := httptest.NewServer(lb)
	defer front.Close()

	for i := 0; i < 20; i++ {
		if status, _ := do(t, http.MethodGet, front.URL, ""); status != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, status)
		}
	}
	// Clients never saw the failures, but the breaker did.
	if got := bad.CircuitState(); got != "open" {
		t.Fatalf("circuit = %s, want open", got)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		method, body string
		want         bool
	}{
		{http.MethodGet, "", true},
		{http.MethodHead, "", true},
		{http.MethodOptions, "", true},
		{http.MethodGet, "unexpected body", false},
		{http.MethodPost, "", false},
		{http.MethodPut, "x", false},
		{http.MethodDelete, "", false},
	}
	for _, c := range cases {
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		r := httptest.NewRequest(c.method, "/", body)
		if got := isRetryable(r); got != c.want {
			t.Errorf("%s with body %q: isRetryable = %v, want %v", c.method, c.body, got, c.want)
		}
	}
}
