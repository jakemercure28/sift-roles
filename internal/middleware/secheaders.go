package middleware

import "net/http"

// contentSecurityPolicy is the dashboard's CSP. It keeps script/style/font/img to
// same-origin assets, blocks plugins and framing, and forbids <base> hijacking.
//
//   - 'unsafe-inline' on script-src is required because the server-rendered pages
//     carry inline onclick handlers (and a few inline <script> bootstraps). It does
//     NOT defeat the policy's main value here: default-src 'self' still blocks
//     loading or exfiltrating to foreign origins, and a non-http(s) "javascript:"
//     URL in an href is blocked outright by script-src not allowing such schemes.
//   - script-src also allows https://cdn.jsdelivr.net because the page loads
//     supabase-js and chart.js from that CDN (see page.go). Without it the browser
//     blocks supabase-js, auth.js then throws, and the dashboard never hydrates
//     past its loading skeleton.
//   - connect-src adds https: and wss: so supabase-js can reach the hosted auth /
//     Postgres endpoints; on self-host nothing cross-origin is contacted.
//   - style-src allows 'unsafe-inline' for the inlined theme <style> block.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self' https: wss:; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// SecurityHeaders sets defense-in-depth response headers on every response: a
// Content-Security-Policy, nosniff, and clickjacking protection. These are belt-
// and-suspenders alongside the per-field output escaping in the dashboard
// renderers; CSP in particular contains any escaping gap to same-origin.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
