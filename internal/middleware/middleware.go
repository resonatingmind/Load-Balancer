// Package middleware wraps the load balancer with request IDs and access logs.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

const RequestIDHeader = "X-Request-ID"

// RequestInfo is filled in by the load balancer as it routes a request, so
// the access log can say which backend answered and after how many attempts.
type RequestInfo struct {
	Backend  string
	Attempts int
}

type infoKey struct{}

// InfoFrom returns the request's RequestInfo, or nil if access logging is off.
func InfoFrom(ctx context.Context) *RequestInfo {
	info, _ := ctx.Value(infoKey{}).(*RequestInfo)
	return info
}

// RequestID makes sure every request carries an X-Request-ID. A valid ID
// from the client is kept; otherwise a new one is generated. The ID is
// forwarded to the backend and echoed in the response, so one request can
// be traced across client, LB and backend logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
			r.Header.Set(RequestIDHeader, id)
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

// validRequestID accepts short IDs made of safe characters, so a client
// can't inject newlines or huge values into our logs and backend headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// AccessLog writes one structured log line per request.
func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &RequestInfo{}
		rec := &recorder{ResponseWriter: w}

		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", r.Header.Get(RequestIDHeader)),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("backend", info.Backend),
			slog.Int("attempts", info.Attempts),
			slog.String("remote", r.RemoteAddr),
		)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *recorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
