package balancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"l7lb/internal/backend"
	"l7lb/internal/circuitbreaker"
	"l7lb/internal/config"
)

func mustBackend(t *testing.T, rawURL string) *backend.Backend {
	t.Helper()
	b, err := backend.New(rawURL, http.DefaultTransport, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func namedServer(t *testing.T, name string) *backend.Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return mustBackend(t, srv.URL)
}

func TestRoundRobinDistributesEvenly(t *testing.T) {
	bs := []*backend.Backend{
		mustBackend(t, "http://a:1"),
		mustBackend(t, "http://b:1"),
		mustBackend(t, "http://c:1"),
	}
	rr := &RoundRobin{}
	counts := map[*backend.Backend]int{}
	for i := 0; i < 30; i++ {
		counts[rr.Next(bs)]++
	}
	for _, b := range bs {
		if counts[b] != 10 {
			t.Errorf("%s got %d requests, want 10", b.URL, counts[b])
		}
	}
}

func TestStrategiesStayFairWhenABackendIsDead(t *testing.T) {
	for _, s := range []Strategy{&RoundRobin{}, &LeastConnections{}} {
		t.Run(s.Name(), func(t *testing.T) {
			bs := []*backend.Backend{
				mustBackend(t, "http://a:1"),
				mustBackend(t, "http://b:1"),
				mustBackend(t, "http://c:1"),
			}
			bs[1].SetAlive(false)

			counts := map[*backend.Backend]int{}
			for i := 0; i < 30; i++ {
				counts[s.Next(bs)]++
			}
			if counts[bs[0]] != 15 || counts[bs[2]] != 15 {
				t.Fatalf("uneven split with one dead backend: a=%d c=%d, want 15/15", counts[bs[0]], counts[bs[2]])
			}
		})
	}
}

func TestStrategiesSkipDeadBackends(t *testing.T) {
	for _, s := range []Strategy{&RoundRobin{}, &LeastConnections{}} {
		t.Run(s.Name(), func(t *testing.T) {
			alive := mustBackend(t, "http://alive:1")
			dead := mustBackend(t, "http://dead:1")
			dead.SetAlive(false)
			bs := []*backend.Backend{dead, alive}

			for i := 0; i < 10; i++ {
				if got := s.Next(bs); got != alive {
					t.Fatalf("picked %v, want the alive backend", got)
				}
			}

			alive.SetAlive(false)
			if got := s.Next(bs); got != nil {
				t.Fatalf("picked %v with all backends dead, want nil", got.URL)
			}
		})
	}
}

func TestLeastConnectionsPrefersIdleBackend(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(slowSrv.Close)
	t.Cleanup(func() { close(release) }) // runs before slowSrv.Close (LIFO)

	busy := mustBackend(t, slowSrv.URL)
	idle := mustBackend(t, "http://idle:1")

	// Hold one request open on the busy backend.
	go busy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-started

	lc := &LeastConnections{}
	bs := []*backend.Backend{busy, idle}
	for i := 0; i < 10; i++ {
		if got := lc.Next(bs); got != idle {
			t.Fatalf("picked %s, want the idle backend", got.URL)
		}
	}
}

func TestLoadBalancerProxiesAcrossBackends(t *testing.T) {
	lb := New([]*backend.Backend{namedServer(t, "one"), namedServer(t, "two")}, &RoundRobin{}, Options{})
	front := httptest.NewServer(lb)
	defer front.Close()

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		resp, err := http.Get(front.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200", resp.StatusCode)
		}
		seen[string(body)]++
	}
	if seen["one"] != 2 || seen["two"] != 2 {
		t.Fatalf("uneven distribution: %v", seen)
	}
}

func TestCircuitBreakerIsolatesFailingBackend(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()

	cbCfg := config.CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 3,
		OpenTimeout:      config.Duration{Duration: time.Minute}, // stays open for the whole test
		HalfOpenRequests: 1,
	}
	bad, err := backend.New(failing.URL, http.DefaultTransport, nil, circuitbreaker.New("bad", cbCfg))
	if err != nil {
		t.Fatal(err)
	}
	good := namedServer(t, "good")

	front := httptest.NewServer(New([]*backend.Backend{bad, good}, &RoundRobin{}, Options{}))
	defer front.Close()

	errors := 0
	for i := 0; i < 20; i++ {
		resp, err := http.Get(front.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errors++
		}
	}
	// The bad backend fails exactly FailureThreshold times, then its circuit
	// opens and every remaining request goes to the good backend.
	if errors != 3 {
		t.Fatalf("got %d failed requests, want 3 (the failure threshold)", errors)
	}
	if got := bad.CircuitState(); got != "open" {
		t.Fatalf("bad backend circuit = %s, want open", got)
	}
}

func TestLoadBalancerReturns503WhenNoBackendAlive(t *testing.T) {
	b := namedServer(t, "one")
	b.SetAlive(false)
	lb := New([]*backend.Backend{b}, &LeastConnections{}, Options{})

	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}
