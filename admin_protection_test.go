package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"tutor-mcp/auth"
)

func TestAdministrativeGuardsRunOnceAndBeforeExpensiveWork(t *testing.T) {
	ip, principal := auth.NewRateLimiter(0, 1), auth.NewRateLimiter(0, 1)
	defer ip.Stop()
	defer principal.Stop()
	concurrency, err := auth.NewPrincipalConcurrencyLimiter(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	authCalls, handlerCalls := 0, 0
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCalls++
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.LearnerIDKey, "admin")))
		})
	}
	h := administrativeHandler(ip, principal, concurrency, authenticate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlerCalls++; w.WriteHeader(204) }))
	call := func(addr string) int {
		r := httptest.NewRequest("GET", "/admin/catalog/", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := call("192.0.2.1:1"); got != 204 {
		t.Fatalf("first request=%d (guard consumed twice)", got)
	}
	if got := call("192.0.2.1:1"); got != 429 || authCalls != 1 {
		t.Fatalf("IP guard=%d authCalls=%d", got, authCalls)
	}
	if got := call("192.0.2.2:1"); got != 429 || handlerCalls != 1 {
		t.Fatalf("principal guard=%d handlers=%d", got, handlerCalls)
	}
}
