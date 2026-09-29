package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratedAndForwarded(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(RequestIDHeader)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(seen) != 32 {
		t.Fatalf("generated id %q, want 32 hex chars", seen)
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Fatalf("response id %q != forwarded id %q", got, seen)
	}
}

func TestRequestIDKeepsValidClientID(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(RequestIDHeader)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "client-abc_123")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "client-abc_123" {
		t.Fatalf("id = %q, want the client's id", seen)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "bad id with spaces")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen == "bad id with spaces" {
		t.Fatal("an invalid client id was passed through")
	}
}

func TestAccessLogRecordsRoutingInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	h := AccessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := InfoFrom(r.Context())
		info.Backend, info.Attempts = "localhost:9002", 2
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("hello"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	line := buf.String()
	for _, want := range []string{"method=GET", "path=/x", "status=418", "bytes=5", "backend=localhost:9002", "attempts=2"} {
		if !strings.Contains(line, want) {
			t.Errorf("access log %q is missing %q", line, want)
		}
	}
}
