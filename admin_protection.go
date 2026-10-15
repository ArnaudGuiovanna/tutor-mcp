package main

import (
	"net/http"

	"tutor-mcp/auth"
)

// Apply the same resource guards to catalog and both review APIs. Identity
// authentication runs after the IP shield and before principal-based guards.
func administrativeHandler(ip, principal *auth.RateLimiter, concurrency *auth.PrincipalConcurrencyLimiter, authenticate func(http.Handler) http.Handler, next http.Handler) http.Handler {
	return auth.RateLimitMiddleware(ip, authenticate(auth.LearnerRateLimitMiddleware(principal, concurrency.Middleware(next))))
}
