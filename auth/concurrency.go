// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"fmt"
	"net/http"
	"sync"
)

// PrincipalConcurrencyLimiter bounds in-flight authenticated work per process
// and per principal. It rejects immediately instead of parking an unbounded
// number of goroutines waiting for a semaphore.
type PrincipalConcurrencyLimiter struct {
	mu           sync.Mutex
	globalMax    int
	principalMax int
	inFlight     int
	byPrincipal  map[string]int
}

func NewPrincipalConcurrencyLimiter(globalMax, principalMax int) (*PrincipalConcurrencyLimiter, error) {
	if globalMax <= 0 || principalMax <= 0 || principalMax > globalMax {
		return nil, fmt.Errorf("invalid concurrency limits: global=%d principal=%d", globalMax, principalMax)
	}
	return &PrincipalConcurrencyLimiter{
		globalMax: globalMax, principalMax: principalMax,
		byPrincipal: make(map[string]int),
	}, nil
}

func (l *PrincipalConcurrencyLimiter) acquire(principal string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.globalMax || l.byPrincipal[principal] >= l.principalMax {
		return false
	}
	l.inFlight++
	l.byPrincipal[principal]++
	return true
}

func (l *PrincipalConcurrencyLimiter) release(principal string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	if count := l.byPrincipal[principal]; count <= 1 {
		delete(l.byPrincipal, principal)
	} else {
		l.byPrincipal[principal] = count - 1
	}
}

func (l *PrincipalConcurrencyLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := GetLearnerID(r.Context())
		if principal == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !l.acquire(principal) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"concurrency_limit_exceeded"}`, http.StatusTooManyRequests)
			return
		}
		defer l.release(principal)
		next.ServeHTTP(w, r)
	})
}
