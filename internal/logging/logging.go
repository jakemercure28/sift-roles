// Package logging is the single source of truth for building the application's
// structured loggers. Every service builds its *slog.Logger here so format,
// level, and base fields stay consistent across the dashboard, scheduler, and
// queue worker.
//
// Output is one-line JSON on stdout (captured by Docker's json-file driver),
// using slog's default time/level/msg keys, which Datadog and Splunk auto-map.
// A base "service" attribute tags every line for filtering, and a context-aware
// handler stamps trace_id / user_id onto any record logged through a *Context
// call so a single trace ID stitches a request together across log lines.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"job-search-automation/internal/middleware"
)

// Options configures New.
type Options struct {
	// Level is the minimum level emitted. Use ParseLevel to derive it from an
	// env string. The zero value is slog.LevelInfo.
	Level slog.Level
	// Service tags every line as "service": <name> for log-platform filtering.
	Service string
	// Env, when non-empty, tags every line as "env": <name> (e.g. prod, dev).
	Env string
}

// New builds a JSON logger writing to w. When w is nil it is the caller's job to
// pass a real writer (e.g. os.Stdout); a nil writer panics on first use, matching
// slog's own contract.
func New(w io.Writer, opts Options) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       opts.Level,
		ReplaceAttr: lowercaseLevel,
	})
	logger := slog.New(&contextHandler{inner: base})
	if opts.Service != "" {
		logger = logger.With("service", opts.Service)
	}
	if opts.Env != "" {
		logger = logger.With("env", opts.Env)
	}
	return logger
}

// Discard returns a logger that drops everything. Use it in tests instead of
// rebuilding an io.Discard handler at each call site.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// lowercaseLevel rewrites slog's default upper-case level token (INFO, WARN,
// ERROR, DEBUG) to lower-case so every service in the stack emits the same
// level vocabulary. Go slog, Fastify/pino, and the native scrapers all then log
// "level":"info"|"warn"|"error", which keeps Loki's "level" label to a single
// value per severity instead of splitting INFO/info/30 into separate series.
func lowercaseLevel(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(strings.ToLower(lvl.String()))
		}
	}
	return a
}

// ParseLevel maps an env string (debug|info|warn|error, case-insensitive) to a
// slog.Level, defaulting to Info on empty or unknown input.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler wraps a handler and, when a record is logged through a *Context
// call, enriches it with trace_id / user_id pulled from the context. Plain log
// calls (the bulk of the codebase) carry no context, so they rely on request- or
// job-scoped child loggers built with .With(...) at the entry point.
type contextHandler struct {
	inner slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if ctx != nil {
		if id := middleware.TraceID(ctx); id != "" {
			rec.AddAttrs(slog.String("trace_id", id))
		}
		if uid := middleware.UserID(ctx); uid != "" {
			rec.AddAttrs(slog.String("user_id", uid))
		}
	}
	return h.inner.Handle(ctx, rec)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}
