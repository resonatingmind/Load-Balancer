package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"l7lb/internal/backend"
	"l7lb/internal/balancer"
	"l7lb/internal/circuitbreaker"
	"l7lb/internal/config"
	"l7lb/internal/health"
	"l7lb/internal/middleware"
	"l7lb/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := run(*configPath); err != nil {
		slog.Error("load balancer exited", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	transport := proxy.NewTransport(cfg.Transport)
	defer transport.CloseIdleConnections()
	bufPool := proxy.NewBufferPool(32 * 1024)

	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		var breaker *circuitbreaker.Breaker
		if cfg.CircuitBreaker.Enabled {
			breaker = circuitbreaker.New(bc.URL, cfg.CircuitBreaker)
		}
		b, err := backend.New(bc.URL, transport, bufPool, breaker)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}

	strategy, err := balancer.NewStrategy(cfg.Strategy)
	if err != nil {
		return err
	}
	lb := balancer.New(backends, strategy, balancer.Options{MaxRetries: cfg.Retry.MaxRetries})

	// Request flow: RequestID -> AccessLog (optional) -> LoadBalancer.
	var handler http.Handler = lb
	if cfg.AccessLog {
		handler = middleware.AccessLog(slog.Default(), handler)
	}
	handler = middleware.RequestID(handler)

	servers := []*http.Server{{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       cfg.Server.ReadTimeout.Duration,
		WriteTimeout:      cfg.Server.WriteTimeout.Duration,
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,
	}}
	if cfg.AdminAddr != "" {
		servers = append(servers, &http.Server{
			Addr:              cfg.AdminAddr,
			Handler:           lb.StatusHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	healthDone := make(chan struct{})
	healthCtx, stopHealth := context.WithCancel(ctx)
	defer stopHealth()
	if cfg.HealthCheck.Enabled {
		checker := health.New(backends, cfg.HealthCheck)
		go func() {
			checker.Run(healthCtx)
			close(healthDone)
		}()
		slog.Info("health checks enabled", "path", cfg.HealthCheck.Path, "interval", cfg.HealthCheck.Interval.String())
	} else {
		close(healthDone)
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func(srv *http.Server) {
			slog.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv)
	}
	slog.Info("load balancer started", "strategy", strategy.Name(), "backends", len(backends), "max_retries", cfg.Retry.MaxRetries)

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	case runErr = <-errCh:
	}

	// Stop accepting new connections and let in-flight requests finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown", "addr", srv.Addr, "err", err)
		}
	}
	stopHealth()
	<-healthDone
	slog.Info("stopped")
	return runErr
}
