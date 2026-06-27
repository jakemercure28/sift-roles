package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubVerifier returns a fixed user id for a known token and an error otherwise.
type stubVerifier struct {
	wantToken string
	userID    string
}

func (s stubVerifier) Verify(_ context.Context, token string) (string, error) {
	if token == s.wantToken {
		return s.userID, nil
	}
	return "", errors.New("bad token")
}

func TestAuthNilVerifierInjectsLocalUser(t *testing.T) {
	var got string
	handler := Auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = UserID(r.Context())
	}), nil, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard-list", nil))

	if got != LocalUser {
		t.Fatalf("user id = %q, want %q", got, LocalUser)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthValidTokenInjectsSubject(t *testing.T) {
	var got string
	v := stubVerifier{wantToken: "good", userID: "user-123"}
	handler := Auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = UserID(r.Context())
	}), v, func(*http.Request) bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard-list", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got != "user-123" {
		t.Fatalf("user id = %q, want user-123", got)
	}
}

func TestAuthMissingTokenOnProtectedPathIs401(t *testing.T) {
	called := false
	v := stubVerifier{wantToken: "good", userID: "user-123"}
	handler := Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), v, func(*http.Request) bool { return false })

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard-list", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next handler should not run on a rejected request")
	}
}

func TestAuthInvalidTokenOnProtectedPathIs401(t *testing.T) {
	v := stubVerifier{wantToken: "good", userID: "user-123"}
	handler := Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), v,
		func(*http.Request) bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/api/dashboard-list", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthPublicPathPassesWithoutToken(t *testing.T) {
	called := false
	v := stubVerifier{wantToken: "good", userID: "user-123"}
	handler := Auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), v, func(*http.Request) bool { return true })

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || !called {
		t.Fatalf("public path should pass: status=%d called=%v", rec.Code, called)
	}
}
