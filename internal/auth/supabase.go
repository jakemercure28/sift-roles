// Package auth verifies Supabase-issued JWTs for the hosted multi-tenant
// backend. The browser runs the OAuth flow via supabase-js and sends the
// resulting access token as a Bearer credential; this package validates that
// token's signature against the project's published JWKS (asymmetric signing
// keys) and returns the Supabase user id (the JWT `sub` claim) that the
// dashboard middleware scopes the request's Repository to.
package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// SupabaseVerifier validates Supabase access tokens against the project JWKS.
// The keyfunc fetches and caches the JWKS and refreshes it on rotation, so a
// single verifier is safe for the process lifetime and for concurrent use.
type SupabaseVerifier struct {
	kf       keyfunc.Keyfunc
	issuer   string
	audience string
}

// gotrueAudience is the audience Supabase Auth (GoTrue) stamps on tokens issued
// to a signed-in user.
const gotrueAudience = "authenticated"

// NewSupabaseVerifier builds a verifier for the project at supabaseURL (e.g.
// https://<ref>.supabase.co). It points the JWKS client at the project's
// well-known JWKS endpoint and pins the expected issuer/audience. ctx bounds the
// initial JWKS fetch and backs the background refresh.
func NewSupabaseVerifier(ctx context.Context, supabaseURL string) (*SupabaseVerifier, error) {
	base := strings.TrimRight(strings.TrimSpace(supabaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("supabase url is empty")
	}
	issuer := base + "/auth/v1"
	jwksURL := issuer + "/.well-known/jwks.json"

	kf, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("build supabase jwks keyfunc: %w", err)
	}
	return &SupabaseVerifier{kf: kf, issuer: issuer, audience: gotrueAudience}, nil
}

// Verify validates token's signature, issuer, audience, and expiry, and returns
// the Supabase user id (the `sub` claim). It errors on any validation failure or
// a missing/empty subject.
func (v *SupabaseVerifier) Verify(_ context.Context, token string) (string, error) {
	parsed, err := jwt.Parse(
		token,
		v.kf.Keyfunc,
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", fmt.Errorf("verify token: %w", err)
	}
	if !parsed.Valid {
		return "", fmt.Errorf("token invalid")
	}
	sub, err := parsed.Claims.GetSubject()
	if err != nil {
		return "", fmt.Errorf("read subject: %w", err)
	}
	if sub == "" {
		return "", fmt.Errorf("token has no subject")
	}
	return sub, nil
}
