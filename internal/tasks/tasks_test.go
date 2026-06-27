package tasks

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"testing"

	"github.com/hibiken/asynq"

	"job-search-automation/internal/middleware"
)

var generatedTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestSmokePingPayloadRoundTrip(t *testing.T) {
	task, err := NewSmokePingTask(context.Background(), SmokePingPayload{Message: "ok"})
	if err != nil {
		t.Fatalf("NewSmokePingTask returned error: %v", err)
	}
	if task.Type() != TypeSmokePing {
		t.Fatalf("task type = %q, want %q", task.Type(), TypeSmokePing)
	}

	payload, err := DecodeSmokePingPayload(task)
	if err != nil {
		t.Fatalf("DecodeSmokePingPayload returned error: %v", err)
	}
	if payload.Message != "ok" {
		t.Fatalf("payload message = %q, want ok", payload.Message)
	}
	if !generatedTraceIDPattern.MatchString(payload.TraceID) {
		t.Fatalf("payload trace ID = %q, want generated trace ID", payload.TraceID)
	}
}

func TestSmokePingPayloadUsesContextTraceID(t *testing.T) {
	ctx := middleware.ContextWithTraceID(context.Background(), "ABC123")
	task, err := NewSmokePingTask(ctx, SmokePingPayload{Message: "ok"})
	if err != nil {
		t.Fatalf("NewSmokePingTask returned error: %v", err)
	}
	payload, err := DecodeSmokePingPayload(task)
	if err != nil {
		t.Fatalf("DecodeSmokePingPayload returned error: %v", err)
	}
	if payload.TraceID != "abc123" {
		t.Fatalf("payload trace ID = %q, want abc123", payload.TraceID)
	}
}

func TestRegisterHandlersRegistersSmokePing(t *testing.T) {
	mux := asynq.NewServeMux()
	RegisterHandlers(mux, slog.New(slog.DiscardHandler))

	task, err := NewSmokePingTask(context.Background(), SmokePingPayload{Message: "registered"})
	if err != nil {
		t.Fatalf("NewSmokePingTask returned error: %v", err)
	}
	handler, pattern := mux.Handler(task)
	if pattern != TypeSmokePing {
		t.Fatalf("registered pattern = %q, want %q", pattern, TypeSmokePing)
	}
	if err := handler.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
}

func TestRegisterHandlersLeavesUnknownTasksUnhandled(t *testing.T) {
	mux := asynq.NewServeMux()
	RegisterHandlers(mux, slog.New(slog.DiscardHandler))

	err := mux.ProcessTask(context.Background(), asynq.NewTask("go:maintenance:unknown", nil))
	if !errors.Is(err, asynq.ErrHandlerNotFound) {
		t.Fatalf("unknown task error = %v, want ErrHandlerNotFound", err)
	}
}
