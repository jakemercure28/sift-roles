package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Serve runs a minimal HTTP server exposing GET /metrics on addr until ctx is
// cancelled, then shuts down cleanly. It is deliberately a separate listener from
// the dashboard so /metrics is never published to the host or reachable through
// the Cloudflare tunnel: Prometheus scrapes it over the loopback-published port.
// Mirrors internal/trigger.Server.Listen.
func Serve(ctx context.Context, addr string, log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler())
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("metrics server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
