package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"job-search-automation/internal/middleware"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// decode parses the single JSON log line written to buf.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v (%q)", err, buf.String())
	}
	return rec
}

func TestNewEmitsJSONWithServiceAndLevelFields(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{Level: slog.LevelInfo, Service: "job-search-go", Env: "test"})
	logger.Info("hello", "k", "v")

	rec := decode(t, &buf)
	if rec["service"] != "job-search-go" {
		t.Errorf("service = %v, want job-search-go", rec["service"])
	}
	if rec["env"] != "test" {
		t.Errorf("env = %v, want test", rec["env"])
	}
	if rec["level"] != "info" {
		t.Errorf("level = %v, want info", rec["level"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if _, ok := rec["time"]; !ok {
		t.Error("expected a time field")
	}
}

func TestLevelIsLowercase(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "debug",
		slog.LevelInfo:  "info",
		slog.LevelWarn:  "warn",
		slog.LevelError: "error",
	}
	for lvl, want := range cases {
		var buf bytes.Buffer
		logger := New(&buf, Options{Level: slog.LevelDebug, Service: "svc"})
		logger.Log(context.Background(), lvl, "msg")
		if got := decode(t, &buf)["level"]; got != want {
			t.Errorf("level for %v = %v, want %q", lvl, got, want)
		}
	}
}

func TestLevelGatesOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{Level: slog.LevelWarn, Service: "svc"})
	logger.Info("dropped")
	if buf.Len() != 0 {
		t.Errorf("Info should be gated at Warn level, got %q", buf.String())
	}
	logger.Warn("kept")
	if buf.Len() == 0 {
		t.Error("Warn should be emitted at Warn level")
	}
}

func TestContextStampsTraceAndUser(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{Service: "svc"})

	ctx := middleware.ContextWithTraceID(context.Background(), "deadbeefdeadbeefdeadbeefdeadbeef")
	ctx = middleware.ContextWithUserID(ctx, "user-123")
	logger.InfoContext(ctx, "with context")

	rec := decode(t, &buf)
	if rec["trace_id"] != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("trace_id = %v, want the context trace", rec["trace_id"])
	}
	if rec["user_id"] != "user-123" {
		t.Errorf("user_id = %v, want user-123", rec["user_id"])
	}
}

func TestPlainCallHasNoTraceFields(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, Options{Service: "svc"})
	logger.Info("no context")

	rec := decode(t, &buf)
	if _, ok := rec["trace_id"]; ok {
		t.Error("plain Info call should not carry trace_id")
	}
	if _, ok := rec["user_id"]; ok {
		t.Error("plain Info call should not carry user_id")
	}
}
