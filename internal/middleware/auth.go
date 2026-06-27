package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// LocalUser is the tenant id used when no real identity is resolved: the SQLite
// self-host backend is single-tenant and every request runs as this user. It
// mirrors db.LocalUser (kept independent here to avoid a middleware->db import).
const LocalUser = "local"

type userContextKey struct{}

// UserID returns the resolved tenant id stored in ctx, or "" if none was set.
func UserID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(userContextKey{}).(string)
	return id
}

// ContextWithUserID stores uid in ctx as the request's resolved tenant.
func ContextWithUserID(ctx context.Context, uid string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, userContextKey{}, uid)
}

// Verifier validates a bearer token and returns the caller's tenant id (the
// Supabase `sub`). internal/auth.SupabaseVerifier satisfies it; keeping the
// interface here lets middleware stay free of the auth package's dependencies.
type Verifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// Auth resolves the request's tenant and stores it in the context.
//
// When v is nil the service runs unauthenticated (SQLite self-host): every
// request is stamped as LocalUser and passed through. When v is non-nil (hosted
// Postgres) it verifies the Authorization: Bearer token; a valid token's subject
// becomes the request tenant. Requests to a non-public path without a resolved
// identity are rejected with 401. Public paths (the page shell, static assets,
// health, auth config) always pass so the browser can load and run the login
// flow, which then re-requests protected routes with a token.
func Auth(next http.Handler, v Verifier, isPublic func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v == nil {
			next.ServeHTTP(w, r.WithContext(ContextWithUserID(r.Context(), LocalUser)))
			return
		}

		if token := bearerToken(r); token != "" {
			if uid, err := v.Verify(r.Context(), token); err == nil && uid != "" {
				r = r.WithContext(ContextWithUserID(r.Context(), uid))
			}
		}

		if UserID(r.Context()) == "" && (isPublic == nil || !isPublic(r)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			// Matches the canonical error envelope used across the API
			// (dashboard.errorEnvelope); a struct keeps ok first.
			_ = json.NewEncoder(w).Encode(struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}{OK: false, Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// or "" if absent or malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
