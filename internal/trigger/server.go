// Package trigger exposes a tiny HTTP surface so the Node dashboard can kick off
// a scrape on demand. The Go engine is the only writer of the scrape heartbeat,
// so an in-app "Scrape now" button must come through here (a Node-side scrape
// would not clear the dashboard's staleness banner). It is reachable only inside
// the compose network by service name; no host port is published.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"job-search-automation/internal/middleware"
)

// Runner is the slice of the scheduler the trigger needs: a non-blocking start
// that reports whether it began a cycle (false means one was already in flight).
type Runner interface {
	TryStart(ctx context.Context, timeout time.Duration, userID string) bool
}

// Server serves the on-demand scrape trigger.
type Server struct {
	runner Runner
	log    *slog.Logger
	// scrapeTimeout bounds the background scrape, independent of the short-lived
	// HTTP request that kicked it off.
	scrapeTimeout time.Duration
}

// New builds a trigger Server.
func New(runner Runner, scrapeTimeout time.Duration, log *slog.Logger) *Server {
	return &Server{runner: runner, log: log, scrapeTimeout: scrapeTimeout}
}

// Handler returns the HTTP routes: POST /scrape and GET /health.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/scrape", s.handleScrape)
	return middleware.Trace(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleScrape starts a scrape cycle and returns immediately: 202 if this call
// started it, 409 if a cycle was already running.
func (s *Server) handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	var body struct {
		UserID string `json:"userId"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	}
	if s.runner.TryStart(r.Context(), s.scrapeTimeout, body.UserID) {
		if body.UserID != "" {
			s.log.Info("scrape triggered via dashboard", "user_id", body.UserID)
		} else {
			s.log.Info("scrape triggered via dashboard")
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true})
		return
	}
	writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "busy": true})
}

// Listen serves Handler on addr until ctx is cancelled, then shuts down cleanly.
func (s *Server) Listen(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	s.log.Info("scrape trigger server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
