package middleware

import (
	"net/http"
	"strconv"
	"time"

	"job-search-automation/internal/metrics"
)

// statusRecorder captures the response status code so the metrics middleware can
// label by it. It defaults to 200, matching net/http's implicit WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Metrics times each request and records jsa_http_request_duration_seconds. The
// route label is the matched ServeMux pattern (set on the request during routing),
// which keeps cardinality bounded; requests with no matched pattern report
// "unmatched". It reads r.Pattern after the inner handler runs, so routing has
// populated it by then. Wrap this DIRECTLY around the mux (inside any middleware
// that swaps the request via r.WithContext, e.g. Auth): the pattern is recorded on
// the request the mux actually receives, so reading it on a pre-clone request would
// always be empty.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		metrics.ObserveHTTP(route, r.Method, strconv.Itoa(rec.status), time.Since(start))
	})
}
