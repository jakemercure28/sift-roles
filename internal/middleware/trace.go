// Package middleware provides small HTTP middleware shared by Go services.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const TraceHeader = "X-Trace-ID"

type traceContextKey struct{}

// TraceID returns the trace ID stored in ctx, if present.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceContextKey{}).(string)
	return id
}

// ContextWithTraceID stores id in ctx when id is valid hexadecimal. Invalid IDs
// are ignored so callers cannot accidentally poison trace metadata.
func ContextWithTraceID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !isHexTraceID(id) {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, strings.ToLower(id))
}

// EnsureTraceID returns a context carrying a trace ID and the trace ID value.
func EnsureTraceID(ctx context.Context) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if id := TraceID(ctx); isHexTraceID(id) {
		return ctx, strings.ToLower(id)
	}
	id := newTraceID()
	return context.WithValue(ctx, traceContextKey{}, id), id
}

// Trace extracts or generates a trace ID, stores it in the request context, and
// makes it visible on the response and request headers.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(TraceHeader)
		if !isHexTraceID(id) {
			id = newTraceID()
		} else {
			id = strings.ToLower(id)
		}
		w.Header().Set(TraceHeader, id)
		r.Header.Set(TraceHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceContextKey{}, id)))
	})
}

// InjectTraceHeader sets X-Trace-ID on an outbound request from its context, or
// generates one if the context does not already carry a trace ID.
func InjectTraceHeader(req *http.Request) {
	if req == nil {
		return
	}
	_, id := EnsureTraceID(req.Context())
	req.Header.Set(TraceHeader, id)
}

func isHexTraceID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
