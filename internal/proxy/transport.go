// Package proxy holds the shared building blocks for forwarding traffic:
// the pooled upstream transport and the reusable copy buffers.
package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"l7lb/internal/config"
)

// NewTransport builds the single http.Transport shared by all backends.
// Sharing one transport means keep-alive connections to each backend are
// pooled and reused instead of doing a new TCP handshake per request.
func NewTransport(cfg config.TransportConfig) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   cfg.DialTimeout.Duration,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 nil, // never route LB -> backend traffic through an env proxy
		DialContext:           dialer.DialContext,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout.Duration,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout.Duration,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// bufferPool recycles the byte slices ReverseProxy uses to stream bodies,
// so copying a response doesn't allocate a fresh 32KB buffer every time.
type bufferPool struct {
	pool sync.Pool
}

func NewBufferPool(size int) httputil.BufferPool {
	return &bufferPool{
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, size)
				return &b
			},
		},
	}
}

func (p *bufferPool) Get() []byte {
	return *(p.pool.Get().(*[]byte))
}

func (p *bufferPool) Put(b []byte) {
	p.pool.Put(&b)
}
