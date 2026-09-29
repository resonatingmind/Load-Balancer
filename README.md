# L7 HTTP Load Balancer (Go)

A Layer 7 HTTP load balancer written in Go using only the standard library.

## Features (current)

- Reverse proxying via `httputil.ReverseProxy` with `X-Forwarded-*` headers
- **Connection pooling**: one shared, tuned `http.Transport` keeps keep-alive connections to backends warm
- **Pooled copy buffers**: `sync.Pool` recycles the 32KB buffers used to stream request and response bodies
- **Strategies**: least-connections (default) and round-robin, both lock-free (atomics)
- **Active health checks**: one prober goroutine per backend sends results over a channel to a single aggregator; a backend goes down after 3 failed probes in a row and comes back after 2 successful ones (configurable)
- **Circuit breaker** per backend (closed → open → half-open): trips after 5 consecutive failures in real traffic (5xx or connection errors), so a backend that passes `/health` but fails requests is still isolated within a few requests
- **Retries on a different backend**: connection errors and 5xx responses on idempotent requests (GET/HEAD/OPTIONS without a body) are held back and retried elsewhere, up to `max_retries`. The last attempt's real response is passed through. POSTs are never replayed
- **Request IDs and access logs**: every request gets an `X-Request-ID` (kept from the client if valid), forwarded to the backend and echoed back; one structured log line per request with status, latency, backend and attempt count
- Admin endpoint with live backend state (`alive`, `circuit`, `active_connections`)
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
  health/         active health checker (probers -> channel -> aggregator)
  circuitbreaker/ per-backend breaker state machine
  middleware/     request IDs + access logging
  balancer/       strategies (least-conn, round-robin) + routing/retry loop
config.json       example config
```

## Request flow

```
client ──► RequestID ──► AccessLog ──► LoadBalancer.ServeHTTP
                                          │ strategy.Next(candidates)   (skips unhealthy backends and open circuits)
                                          ▼
                                       Backend.Serve                    breaker.Allow() → activeConns++
                                          │ httputil.ReverseProxy
                                          ▼
                                       shared http.Transport ──(pooled keep-alive conns)──► backend server
                                          │
                                          ▼
                                       outcome → breaker (5xx / conn error = failure) → activeConns--
                                          │
                     Served ◄─────────────┼─────────────► Failed / Rejected
                   (response sent)                       (nothing sent yet: remove this backend
                                                          from candidates and pick again)
```

A retry is only possible while nothing has been sent to the client. So when a retry is allowed, a `ModifyResponse` hook turns a 5xx into an error and the `ErrorHandler` records it instead of writing a 502. If no other backend is available, or retries are used up, the last backend's real response goes to the client.

## Zero-downtime check

1500 requests at 10 concurrent, killing one of three backends mid-run:

| `max_retries` | Failed requests |
|---|---|
| 0 | 6 (502) |
| 2 | **0** (6 requests retried on another backend) |

## Simulating failures

The dummy backend can fail on purpose while its `/health` still reports OK, which health checks can't catch but the circuit breaker can:

```bash
go run ./cmd/backend -addr :9002 -name b2 -error-rate 1
```

## Running locally

Requires Go 1.22+. If port 8080 is taken on your machine (Docker Desktop often uses it), change `listen_addr` in `config.json`.

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
- [x] Active health checks (per-backend goroutine, status events over channels)
- [x] Circuit breaker (closed → open → half-open) per backend
- [x] Retry on another backend when a request fails
- [x] Request IDs and structured access logs
- [ ] Prometheus `/metrics`
- [ ] Load benchmarks (`hey` / `wrk` / `k6`): RPS, p50/p99 latency at 1,000 concurrent connections
- [ ] Docker Compose setup
