package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

var generatedTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestTraceUsesInboundHexTraceID(t *testing.T) {
	var got string
	handler := Trace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = TraceID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TraceHeader, "ABC123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "abc123" {
		t.Fatalf("context trace ID = %q, want abc123", got)
	}
	if header := rec.Header().Get(TraceHeader); header != "abc123" {
		t.Fatalf("response trace header = %q, want abc123", header)
	}
}

func TestTraceGeneratesTraceIDForInvalidInboundHeader(t *testing.T) {
	var got string
	handler := Trace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = TraceID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(TraceHeader, "not hex")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !generatedTraceIDPattern.MatchString(got) {
		t.Fatalf("generated context trace ID = %q, want 32 lowercase hex chars", got)
	}
	if header := rec.Header().Get(TraceHeader); header != got {
		t.Fatalf("response trace header = %q, want %q", header, got)
	}
}

func TestEnsureTraceIDPreservesExistingTraceID(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "FACE")
	next, id := EnsureTraceID(ctx)

	if id != "face" {
		t.Fatalf("trace ID = %q, want face", id)
	}
	if next != ctx {
		t.Fatalf("EnsureTraceID should return original context when trace exists")
	}
}

func TestEnsureTraceIDGeneratesTraceID(t *testing.T) {
	ctx, id := EnsureTraceID(context.Background())

	if !generatedTraceIDPattern.MatchString(id) {
		t.Fatalf("generated trace ID = %q, want 32 lowercase hex chars", id)
	}
	if got := TraceID(ctx); got != id {
		t.Fatalf("context trace ID = %q, want %q", got, id)
	}
}

func TestInjectTraceHeader(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "123abc")
	req := httptest.NewRequest(http.MethodGet, "http://example.test", nil).WithContext(ctx)

	InjectTraceHeader(req)

	if got := req.Header.Get(TraceHeader); got != "123abc" {
		t.Fatalf("trace header = %q, want 123abc", got)
	}
}

func TestInjectTraceHeaderGeneratesTraceID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test", nil)

	InjectTraceHeader(req)

	if got := req.Header.Get(TraceHeader); !generatedTraceIDPattern.MatchString(got) {
		t.Fatalf("trace header = %q, want generated trace ID", got)
	}
}
