package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	readinessSuccessTTL   = time.Second
	readinessFailureTTL   = 250 * time.Millisecond
	readinessProbeTimeout = 2 * time.Second
)

type readinessResult struct {
	ready  bool
	reason string
}

type readinessChecker struct {
	pinger  readinessPinger
	now     func() time.Time
	timeout time.Duration
	mu      sync.Mutex
	result  readinessResult
	expires time.Time
	flight  *readinessFlight
}

type readinessFlight struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func readinessHandler(pinger readinessPinger) http.HandlerFunc {
	checker := &readinessChecker{pinger: pinger, now: time.Now, timeout: readinessProbeTimeout}
	return checker.ServeHTTP
}

func (c *readinessChecker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	result := c.check(r.Context())
	if !result.ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready", "error": result.reason})
		return
	}
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func (c *readinessChecker) check(ctx context.Context) readinessResult {
	for {
		c.mu.Lock()
		if c.now().Before(c.expires) {
			result := c.result
			c.mu.Unlock()
			return result
		}
		flight := c.flight
		if flight == nil {
			probeCtx, cancel := context.WithTimeout(context.Background(), c.timeout)
			flight = &readinessFlight{ctx: probeCtx, cancel: cancel, done: make(chan struct{})}
			c.flight = flight
			go c.refresh(flight)
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return readinessResult{reason: "readiness wait canceled"}
		case <-flight.done:
			continue
		case <-flight.ctx.Done():
			if errors.Is(flight.ctx.Err(), context.DeadlineExceeded) {
				return readinessResult{reason: "readiness check timed out"}
			}
			// The probe cancels its timer only after publishing the result.
			<-flight.done
		}
	}
}

func (c *readinessChecker) refresh(flight *readinessFlight) {
	result := readinessResult{reason: "readiness check failed"}
	defer func() {
		// Probes run outside the HTTP recovery middleware. Never let a faulty
		// backend panic terminate the process or expose the panic payload.
		if recover() != nil {
			result = readinessResult{reason: "readiness check failed"}
		}
		if flight.ctx.Err() != nil {
			result = readinessResult{reason: "readiness check timed out"}
		}
		c.mu.Lock()
		c.result = result
		ttl := readinessFailureTTL
		if result.ready {
			ttl = readinessSuccessTTL
		}
		c.expires = c.now().Add(ttl)
		close(flight.done)
		c.flight = nil
		c.mu.Unlock()
		flight.cancel()
	}()
	// Keep the flight occupied until the backend actually returns, even if it
	// ignores cancellation. HTTP waiters still stop at the shared deadline.
	ctx := flight.ctx
	if err := c.pinger.Ping(ctx); err != nil {
		result.reason = "database unreachable"
		return
	}
	if ctx.Err() != nil {
		return
	}
	if verifier, ok := c.pinger.(schemaCompatibilityVerifier); ok {
		if err := verifier.VerifySchemaCurrent(ctx); err != nil {
			result.reason = "schema incompatible"
			return
		}
	}
	result = readinessResult{ready: true}
}
