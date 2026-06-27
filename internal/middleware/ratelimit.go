package middleware

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// idleBucketTTL is how long a tenant's bucket survives with no requests before the
// sweeper drops it, bounding memory on a long-running process.
const idleBucketTTL = 10 * time.Minute

// PerTenantLimiter throttles requests per tenant using one token bucket per tenant
// id (the Supabase sub from UserID). It guards the expensive authenticated endpoints
// so a single tenant can't hammer the shared Gemini host key or CPU: it bounds
// request *rate*, complementing the daily Gemini quota that bounds call *volume*.
// Self-host (LocalUser) and unauthenticated requests are never throttled.
type PerTenantLimiter struct {
	rps        rate.Limit
	burst      int
	retryAfter int // seconds advertised in Retry-After: time to regain one token

	mu      sync.Mutex
	buckets map[string]*tenantBucket
}

type tenantBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewPerTenantLimiter builds a limiter allowing perMinute requests/minute per tenant
// with the given burst. perMinute <= 0 disables limiting: it returns nil, and a nil
// *PerTenantLimiter's Limit is a pass-through, so callers need no special-casing.
func NewPerTenantLimiter(perMinute, burst int) *PerTenantLimiter {
	if perMinute <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = 1
	}
	l := &PerTenantLimiter{
		rps:        rate.Limit(float64(perMinute) / 60.0),
		burst:      burst,
		retryAfter: (60 + perMinute - 1) / perMinute, // ceil(60/perMinute), >= 1
		buckets:    map[string]*tenantBucket{},
	}
	go l.sweep()
	return l
}

// allow records the tenant as active and reports whether it has a token to spend.
func (l *PerTenantLimiter) allow(tenant string) bool {
	l.mu.Lock()
	b := l.buckets[tenant]
	if b == nil {
		b = &tenantBucket{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[tenant] = b
	}
	b.seen = time.Now()
	l.mu.Unlock()
	return b.lim.Allow()
}

// sweep periodically drops buckets idle past idleBucketTTL so a churn of tenants
// can't grow the map without bound. It runs for the process lifetime.
func (l *PerTenantLimiter) sweep() {
	t := time.NewTicker(idleBucketTTL)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-idleBucketTTL)
		l.mu.Lock()
		for tenant, b := range l.buckets {
			if b.seen.Before(cutoff) {
				delete(l.buckets, tenant)
			}
		}
		l.mu.Unlock()
	}
}

// Limit wraps next so each request is throttled by its tenant. A nil limiter, an
// unresolved tenant, or self-host (LocalUser) passes through unthrottled. Over-limit
// requests get 429 with the canonical {ok:false,...} envelope and a Retry-After
// header so clients back off instead of busy-retrying.
func (l *PerTenantLimiter) Limit(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := UserID(r.Context())
		if tenant == "" || tenant == LocalUser || l.allow(tenant) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", strconv.Itoa(l.retryAfter))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Code  string `json:"code"`
		}{OK: false, Error: "rate limited", Code: "rate_limited"})
	})
}
