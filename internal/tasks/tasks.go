// Package tasks defines isolated Go queue tasks for the Asynq worker.
package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"job-search-automation/internal/middleware"
)

const (
	// QueueGoMaintenance is reserved for Go/Asynq smoke and maintenance tasks.
	QueueGoMaintenance = "go:maintenance"

	// TypeSmokePing is a no-op smoke task used to verify worker plumbing.
	TypeSmokePing = "go:maintenance:smoke_ping"
)

// SmokePingPayload is the payload for TypeSmokePing.
type SmokePingPayload struct {
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

// NewSmokePingTask returns an encoded Asynq smoke-test task.
func NewSmokePingTask(ctx context.Context, payload SmokePingPayload) (*asynq.Task, error) {
	_, traceID := middleware.EnsureTraceID(ctx)
	payload.TraceID = traceID
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode smoke ping payload: %w", err)
	}
	return asynq.NewTask(TypeSmokePing, body), nil
}

// DecodeSmokePingPayload decodes a TypeSmokePing task payload.
func DecodeSmokePingPayload(task *asynq.Task) (SmokePingPayload, error) {
	var payload SmokePingPayload
	if task == nil {
		return payload, fmt.Errorf("decode smoke ping payload: nil task")
	}
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return payload, fmt.Errorf("decode smoke ping payload: %w", err)
	}
	return payload, nil
}

// EnqueueSmokePing enqueues a smoke-test task on the Go maintenance queue.
func EnqueueSmokePing(ctx context.Context, client *asynq.Client, payload SmokePingPayload) (*asynq.TaskInfo, error) {
	task, err := NewSmokePingTask(ctx, payload)
	if err != nil {
		return nil, err
	}
	info, err := client.EnqueueContext(ctx, task, asynq.Queue(QueueGoMaintenance))
	if err != nil {
		return nil, fmt.Errorf("enqueue smoke ping task: %w", err)
	}
	return info, nil
}

// RegisterHandlers registers all Go/Asynq handlers with a single mux.
func RegisterHandlers(mux *asynq.ServeMux, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	mux.HandleFunc(TypeSmokePing, func(ctx context.Context, task *asynq.Task) error {
		payload, err := DecodeSmokePingPayload(task)
		if err != nil {
			return err
		}
		// The trace ID rides in the payload across the queue; restore it into the
		// handler context so the logging handler stamps trace_id automatically.
		ctx = middleware.ContextWithTraceID(ctx, payload.TraceID)
		logger.InfoContext(ctx, "handled go smoke task", "task_type", task.Type(), "message", payload.Message)
		return nil
	})
}
