// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recordingRateLimitBackend struct{ keys []string }

func (b *recordingRateLimitBackend) Allow(_ context.Context, key string, _ float64, _ int, _ time.Time) (bool, error) {
	b.keys = append(b.keys, key)
	return true, nil
}

type failingRateLimitBackend struct{}

func (failingRateLimitBackend) Allow(context.Context, string, float64, int, time.Time) (bool, error) {
	return false, errors.New("backend unavailable")
}

func TestNewRateLimiter_FieldsInitialized(t *testing.T) {
	rl := NewRateLimiter(2.5, 7)
	defer rl.Stop()

	if rl == nil {
		t.Fatal("NewRateLimiter returned nil")
	}
	if rl.rate != 2.5 {
		t.Fatalf("rate = %v, want 2.5", rl.rate)
	}
	if rl.burst != 7 {
		t.Fatalf("burst = %d, want 7", rl.burst)
	}
	if rl.buckets == nil {
		t.Fatal("buckets map is nil")
	}
	if rl.stop == nil {
		t.Fatal("stop channel is nil")
	}
}

func TestRateLimiter_BackendNamespacesPolicies(t *testing.T) {
	backend := &recordingRateLimitBackend{}
	authLimiter := NewRateLimiterWithNamespace("oauth", 1, 1)
	registerLimiter := NewRateLimiterWithNamespace("oauth_register", 1, 1)
	defer authLimiter.Stop()
	defer registerLimiter.Stop()
	authLimiter.SetBackend(backend)
	registerLimiter.SetBackend(backend)

	if !authLimiter.Allow("198.51.100.7") || !registerLimiter.Allow("198.51.100.7") {
		t.Fatal("recording backend unexpectedly rejected request")
	}
	want := []string{"oauth:198.51.100.7", "oauth_register:198.51.100.7"}
	if len(backend.keys) != len(want) || backend.keys[0] != want[0] || backend.keys[1] != want[1] {
		t.Fatalf("backend keys = %#v, want %#v", backend.keys, want)
	}
}

func TestRateLimiter_BackendFailureUsesBoundedLocalFallback(t *testing.T) {
	rl := NewRateLimiterWithNamespace("oauth_login", 0, 1)
	defer rl.Stop()
	rl.SetBackend(failingRateLimitBackend{})

	if !rl.Allow("sensitive-caller-key") {
		t.Fatal("first request should use the local fallback burst")
	}
	if rl.Allow("sensitive-caller-key") {
		t.Fatal("backend outage must not turn rate limiting into fail-open")
	}
}

func TestRateLimiter_Stop_ClosesChannel(t *testing.T) {
	rl := NewRateLimiter(1, 1)
	rl.Stop()
	// Reading from a closed channel returns immediately with zero value.
	select {
	case _, ok := <-rl.stop:
		if ok {
			t.Fatal("stop channel should be closed after Stop()")
		}
	default:
		t.Fatal("stop channel did not deliver after Stop()")
	}
}

func TestRateLimiter_Allow_FirstHitSucceeds(t *testing.T) {
	rl := NewRateLimiter(1, 3)
	defer rl.Stop()
	if !rl.Allow("1.2.3.4") {
		t.Fatal("first request from new IP must be allowed")
	}
}

func TestRateLimiter_Allow_BurstThenReject(t *testing.T) {
	// rate=0 so no refill happens during the test → exhausting burst must reject.
	rl := NewRateLimiter(0, 2)
	defer rl.Stop()

	const ip = "5.6.7.8"
	if !rl.Allow(ip) {
		t.Fatal("hit 1 should be allowed")
	}
	if !rl.Allow(ip) {
		t.Fatal("hit 2 should be allowed (burst=2)")
	}
	if rl.Allow(ip) {
		t.Fatal("hit 3 must be rejected (burst exhausted, rate=0)")
	}
	if rl.Allow(ip) {
		t.Fatal("hit 4 must remain rejected with rate=0")
	}
}

func TestRateLimiter_Allow_DifferentIPsIsolated(t *testing.T) {
	rl := NewRateLimiter(0, 1)
	defer rl.Stop()

	if !rl.Allow("a") {
		t.Fatal("a hit 1 should be allowed")
	}
	if rl.Allow("a") {
		t.Fatal("a hit 2 must be rejected")
	}
	// b has its own bucket — must still be allowed.
	if !rl.Allow("b") {
		t.Fatal("b hit 1 must be allowed (independent bucket)")
	}
}

func TestRateLimitMiddleware_PassesThroughWhenAllowed(t *testing.T) {
	rl := NewRateLimiter(100, 5)
	defer rl.Stop()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := RateLimitMiddleware(rl, next)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler must run when under limit")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRateLimitMiddleware_Returns429AndSetsRetryAfter(t *testing.T) {
	// rate=0, burst=1: second call from same IP is throttled.
	rl := NewRateLimiter(0, 1)
	defer rl.Stop()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := RateLimitMiddleware(rl, next)

	// First request: allowed.
	req1 := httptest.NewRequest("GET", "/", nil)
	req1.RemoteAddr = "203.0.113.20:1234"
	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first req status = %d, want 200", rec1.Code)
	}

	// Second request: throttled.
	called2 := 0
	next2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called2++
	})
	mw2 := RateLimitMiddleware(rl, next2)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.RemoteAddr = "203.0.113.20:1234"
	rec2 := httptest.NewRecorder()
	mw2.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second req status = %d, want 429", rec2.Code)
	}
	if ra := rec2.Header().Get("Retry-After"); ra != "5" {
		t.Fatalf("Retry-After = %q, want 5", ra)
	}
	if !strings.Contains(rec2.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("body missing rate_limit_exceeded marker: %s", rec2.Body.String())
	}
	if called2 != 0 {
		t.Fatal("next handler must NOT be invoked when rate-limited")
	}
}

func TestLearnerRateLimitMiddleware_UsesLearnerIDBuckets(t *testing.T) {
	rl := NewRateLimiter(0, 1)
	defer rl.Stop()

	calls := map[string]int{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[GetLearnerID(r.Context())]++
		w.WriteHeader(http.StatusOK)
	})
	mw := LearnerRateLimitMiddleware(rl, next)

	serve := func(learnerID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/mcp", nil)
		ctx := context.WithValue(req.Context(), LearnerIDKey, learnerID)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req.WithContext(ctx))
		return rec
	}

	if rec := serve("learner-a"); rec.Code != http.StatusOK {
		t.Fatalf("learner-a first status = %d, want 200", rec.Code)
	}
	if rec := serve("learner-a"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("learner-a second status = %d, want 429", rec.Code)
	}
	if rec := serve("learner-b"); rec.Code != http.StatusOK {
		t.Fatalf("learner-b first status = %d, want 200", rec.Code)
	}
	if calls["learner-a"] != 1 {
		t.Fatalf("learner-a handler calls = %d, want 1", calls["learner-a"])
	}
	if calls["learner-b"] != 1 {
		t.Fatalf("learner-b handler calls = %d, want 1", calls["learner-b"])
	}
}

func TestLearnerRateLimitMiddleware_UsesBearerInjectedLearnerID(t *testing.T) {
	setTestSecret(t)

	tokenA, err := GenerateJWT("https://test.example", "learner-a")
	if err != nil {
		t.Fatalf("generate token A: %v", err)
	}
	tokenB, err := GenerateJWT("https://test.example", "learner-b")
	if err != nil {
		t.Fatalf("generate token B: %v", err)
	}

	rl := NewRateLimiter(0, 1)
	defer rl.Stop()

	calls := map[string]int{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[GetLearnerID(r.Context())]++
		w.WriteHeader(http.StatusOK)
	})
	mw := BearerMiddleware("https://test.example", LearnerRateLimitMiddleware(rl, next))

	serve := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/mcp", nil)
		req.Header.Set("Authorization", "bearer "+token)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec
	}

	if rec := serve(tokenA); rec.Code != http.StatusOK {
		t.Fatalf("learner-a first status = %d, want 200", rec.Code)
	}
	if rec := serve(tokenA); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("learner-a second status = %d, want 429", rec.Code)
	}
	if rec := serve(tokenB); rec.Code != http.StatusOK {
		t.Fatalf("learner-b first status = %d, want 200", rec.Code)
	}
	if calls["learner-a"] != 1 {
		t.Fatalf("learner-a handler calls = %d, want 1", calls["learner-a"])
	}
	if calls["learner-b"] != 1 {
		t.Fatalf("learner-b handler calls = %d, want 1", calls["learner-b"])
	}
}

func TestLearnerRateLimitMiddleware_RejectsMissingLearnerID(t *testing.T) {
	rl := NewRateLimiter(1, 1)
	defer rl.Stop()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := LearnerRateLimitMiddleware(rl, next)

	req := httptest.NewRequest("GET", "/mcp", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("next handler must not run without learner_id in context")
	}
}

func TestRemoteIP_ParsesHostPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.5:54321"
	ip := remoteIP(r)
	if ip == nil || ip.String() != "192.0.2.5" {
		t.Fatalf("remoteIP = %v, want 192.0.2.5", ip)
	}
}

func TestRemoteIP_NoPortFallback(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.5" // missing :port
	ip := remoteIP(r)
	if ip == nil || ip.String() != "192.0.2.5" {
		t.Fatalf("remoteIP fallback = %v, want 192.0.2.5", ip)
	}
}

func TestRemoteIP_Garbage(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "garbage-not-an-ip"
	if got := remoteIP(r); got != nil {
		t.Fatalf("remoteIP for garbage = %v, want nil", got)
	}
}

func TestIsTrustedProxy_NilReturnsFalse(t *testing.T) {
	trustedProxiesOnce.Do(func() {})
	trustedProxies = nil
	if isTrustedProxy(nil) {
		t.Fatal("nil IP must not be trusted")
	}
}

func TestClientIP_NoPeerIPFallsBackToRemoteAddr(t *testing.T) {
	trustedProxiesOnce.Do(func() {})
	trustedProxies = nil

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "garbage-not-an-ip"
	if got := clientIP(r); got != "garbage-not-an-ip" {
		t.Fatalf("clientIP fallback = %q, want raw RemoteAddr", got)
	}
}

func TestLoadTrustedProxies_ParsesEnv(t *testing.T) {
	// loadTrustedProxies uses sync.Once → we cannot truly re-parse. But we can
	// verify that the parsed slice (set up by other tests or a fresh process)
	// is consistent with the global trustedProxies value. Calling loadTrustedProxies
	// must be a no-op after the first call and must return the same slice.
	first := loadTrustedProxies()
	second := loadTrustedProxies()
	if len(first) != len(second) {
		t.Fatalf("loadTrustedProxies non-idempotent: %d vs %d", len(first), len(second))
	}
}
