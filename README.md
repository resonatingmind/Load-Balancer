# L7 HTTP Load Balancer (Go)

A Layer 7 HTTP load balancer written in Go using only the standard library.

## Features (current)

- Reverse proxying via `httputil.ReverseProxy` with `X-Forwarded-*` headers
- **Connection pooling**: one shared, tuned `http.Transport` keeps keep-alive connections to backends warm
- **Pooled copy buffers**: `sync.Pool` recycles the 32KB buffers used to stream request and response bodies
- **Strategies**: least-connections (default) and round-robin, both lock-free (atomics)
- Admin endpoint with live backend state (`alive`, `active_connections`)
- Graceful shutdown: drains in-flight requests on Ctrl+C / SIGTERM

## Layout

```
cmd/
  loadbalancer/   entry point: loads config, wires everything, runs servers
  backend/        dummy upstream server for local testing and benchmarks
internal/
  config/         JSON config loading, defaults, validation
  proxy/          shared pooled transport + buffer pool
  backend/        one upstream: reverse proxy, alive flag, active-conn counter
  balancer/       strategies (least-conn, round-robin) + the HTTP handler
config.json       example config
```

## Request flow

```
client ──► LoadBalancer.ServeHTTP
              │ strategy.Next(backends)   (skips backends marked not alive)
              ▼
           Backend.ServeHTTP              (activeConns++ ... activeConns--)
              │ httputil.ReverseProxy
              ▼
           shared http.Transport ──(pooled keep-alive conns)──► backend server
```

## Running locally

Requires Go 1.22+.

Start three dummy backends, each in its own terminal:

```bash
go run ./cmd/backend -addr :9001 -name b1
```
```bash
go run ./cmd/backend -addr :9002 -name b2
```
```bash
go run ./cmd/backend -addr :9003 -name b3
```

Start the load balancer:

```bash
go run ./cmd/loadbalancer -config config.json
```

Send traffic and inspect state:

```bash
curl http://localhost:8080/hello
```
```bash
curl http://localhost:9090
```

## Tests

```bash
go test -race ./...
```

## Roadmap

- [x] Reverse proxy, config, connection pooling, buffer pool
- [x] Least-connections and round-robin strategies
- [ ] Active health checks (per-backend goroutine, status events over channels)
- [ ] Circuit breaker (closed → open → half-open) per backend
- [ ] Retry on another backend when a request fails
- [ ] Prometheus `/metrics`
- [ ] Load benchmarks (`hey` / `wrk` / `k6`): RPS, p50/p99 latency at 1,000 concurrent connections
- [ ] Docker Compose setup
