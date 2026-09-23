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
	"l7lb/internal/config"
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
		b, err := backend.New(bc.URL, transport, bufPool)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}

	strategy, err := balancer.NewStrategy(cfg.Strategy)
	if err != nil {
		return err
	}
	lb := balancer.New(backends, strategy)

	servers := []*http.Server{{
		Addr:              cfg.ListenAddr,
		Handler:           lb,
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

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		go func(srv *http.Server) {
			slog.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv)
	}
	slog.Info("load balancer started", "strategy", strategy.Name(), "backends", len(backends))

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
	slog.Info("stopped")
	return runErr
}
