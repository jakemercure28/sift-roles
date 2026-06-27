package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqAs(tenant string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	if tenant != "" {
		r = r.WithContext(ContextWithUserID(r.Context(), tenant))
	}
	return r
}

func okHandler() (http.Handler, *int) {
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})
	return h, &calls
}

func TestPerTenantLimiterDisabled(t *testing.T) {
	// perMinute <= 0 disables limiting; a nil limiter's Limit is a pass-through.
	var l *PerTenantLimiter = NewPerTenantLimiter(0, 5)
	if l != nil {
		t.Fatalf("NewPerTenantLimiter(0,...) = %v, want nil", l)
	}
	h, calls := okHandler()
	wrapped := l.Limit(h)
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, reqAs("tenant-a"))
		if rec.Code != http.StatusOK {
			t.Fatalf("disabled limiter blocked request %d: %d", i, rec.Code)
		}
	}
	if *calls != 50 {
		t.Fatalf("handler calls = %d, want 50", *calls)
	}
}

func TestPerTenantLimiterBurstThen429(t *testing.T) {
	// 60/min with burst 3: the first 3 are allowed, the 4th is throttled.
	l := NewPerTenantLimiter(60, 3)
	h, _ := okHandler()
	wrapped := l.Limit(h)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, reqAs("tenant-a"))
		if rec.Code != http.StatusOK {
			t.Fatalf("burst request %d = %d, want 200", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, reqAs("tenant-a"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit request = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 missing Retry-After header")
	}
}

func TestPerTenantLimiterIsolatesTenants(t *testing.T) {
	l := NewPerTenantLimiter(60, 1)
	h, _ := okHandler()
	wrapped := l.Limit(h)

	// Drain tenant-a's single token.
	first := httptest.NewRecorder()
	wrapped.ServeHTTP(first, reqAs("tenant-a"))
	throttled := httptest.NewRecorder()
	wrapped.ServeHTTP(throttled, reqAs("tenant-a"))
	if throttled.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant-a second request = %d, want 429", throttled.Code)
	}
	// tenant-b has its own bucket and is unaffected.
	other := httptest.NewRecorder()
	wrapped.ServeHTTP(other, reqAs("tenant-b"))
	if other.Code != http.StatusOK {
		t.Fatalf("tenant-b request = %d, want 200 (separate bucket)", other.Code)
	}
}

func TestPerTenantLimiterSkipsLocalAndAnon(t *testing.T) {
	l := NewPerTenantLimiter(60, 1)
	h, _ := okHandler()
	wrapped := l.Limit(h)

	for _, who := range []string{LocalUser, ""} {
		for i := 0; i < 10; i++ {
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, reqAs(who))
			if rec.Code != http.StatusOK {
				t.Fatalf("unthrottled identity %q blocked at %d: %d", who, i, rec.Code)
			}
		}
	}
}
