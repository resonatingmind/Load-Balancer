// Package health actively probes backends and marks them up or down.
//
// Design: one prober goroutine per backend sends each probe result over a
// shared channel to a single aggregator goroutine. The aggregator owns all
// threshold counters, so they need no locks; the only shared state it
// touches is each backend's atomic alive flag, which the strategies read.
package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"l7lb/internal/backend"
	"l7lb/internal/config"
)

type result struct {
	backend *backend.Backend
	err     error // nil means the probe succeeded
}

// tracker holds the consecutive-result counters for one backend.
type tracker struct {
	successes int
	failures  int
}

type Checker struct {
	backends []*backend.Backend
	cfg      config.HealthCheckConfig
	client   *http.Client
}

func New(backends []*backend.Backend, cfg config.HealthCheckConfig) *Checker {
	return &Checker{
		backends: backends,
		cfg:      cfg,
		client: &http.Client{
			// A dedicated transport so probes never queue behind proxied
			// traffic when the main transport hits MaxConnsPerHost.
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     2 * cfg.Interval.Duration,
			},
			// A redirect from /health is treated as the answer, not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Run probes all backends until ctx is canceled. It blocks, so start it in
// its own goroutine.
func (c *Checker) Run(ctx context.Context) {
	results := make(chan result)

	var wg sync.WaitGroup
	for _, b := range c.backends {
		wg.Add(1)
		go func(b *backend.Backend) {
			defer wg.Done()
			c.probeLoop(ctx, b, results)
		}(b)
	}

	trackers := make(map[*backend.Backend]*tracker, len(c.backends))
	for _, b := range c.backends {
		trackers[b] = &tracker{}
	}

	for {
		select {
		case r := <-results:
			c.record(trackers[r.backend], r.backend, r.err)
		case <-ctx.Done():
			wg.Wait()
			return
		}
	}
}

// probeLoop probes b once immediately, then on every tick.
func (c *Checker) probeLoop(ctx context.Context, b *backend.Backend, out chan<- result) {
	ticker := time.NewTicker(c.cfg.Interval.Duration)
	defer ticker.Stop()

	for {
		r := result{backend: b, err: c.probe(ctx, b)}
		select {
		case out <- r:
		case <-ctx.Done():
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func (c *Checker) probe(ctx context.Context, b *backend.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout.Duration)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath(c.cfg.Path).String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain (a bounded amount of) the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// record applies one probe result. Requiring several results in a row before
// flipping state stops a single slow probe from knocking a backend out.
func (c *Checker) record(t *tracker, b *backend.Backend, err error) {
	if err == nil {
		t.successes++
		t.failures = 0
		if !b.IsAlive() && t.successes >= c.cfg.HealthyThreshold {
			b.SetAlive(true)
			slog.Info("backend is healthy again", "backend", b.URL.String())
		}
		return
	}

	t.failures++
	t.successes = 0
	if b.IsAlive() && t.failures >= c.cfg.UnhealthyThreshold {
		b.SetAlive(false)
		slog.Warn("backend marked unhealthy", "backend", b.URL.String(), "consecutive_failures", t.failures, "err", err)
	}
}
