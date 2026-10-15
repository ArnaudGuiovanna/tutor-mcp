package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controlledReadinessPinger struct {
	pings    atomic.Int32
	verifies atomic.Int32
	ping     func(context.Context) error
	verify   func(context.Context) error
}

func (p *controlledReadinessPinger) Ping(ctx context.Context) error {
	p.pings.Add(1)
	if p.ping != nil {
		return p.ping(ctx)
	}
	return nil
}
func (p *controlledReadinessPinger) VerifySchemaCurrent(ctx context.Context) error {
	p.verifies.Add(1)
	if p.verify != nil {
		return p.verify(ctx)
	}
	return nil
}

func TestReadinessSharedFlightAndBoundedCache(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var failed atomic.Bool
	p := &controlledReadinessPinger{ping: func(context.Context) error {
		if !failed.Load() {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			return nil
		}
		return errors.New("private backend detail")
	}}
	now := time.Now()
	c := &readinessChecker{pinger: p, now: func() time.Time { return now }, timeout: time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", c.ServeHTTP)
	mux.HandleFunc("GET /health", c.ServeHTTP)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/ready"
			if i%2 == 1 {
				path = "/health"
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 {
				t.Errorf("status=%d", w.Code)
			}
		}(i)
	}
	<-started
	if p.pings.Load() != 1 {
		t.Fatalf("concurrent probes=%d", p.pings.Load())
	}
	unblock()
	wg.Wait()
	if p.pings.Load() != 1 || p.verifies.Load() != 1 {
		t.Fatalf("probe counts=%d/%d", p.pings.Load(), p.verifies.Load())
	}
	failed.Store(true)
	if !c.check(context.Background()).ready {
		t.Fatal("unexpired success not cached")
	}
	now = now.Add(readinessSuccessTTL)
	if c.check(context.Background()).ready {
		t.Fatal("expired success hid database failure")
	}
	for i := 0; i < 100; i++ {
		if c.check(context.Background()).ready {
			t.Fatal("failure cache")
		}
	}
	if p.pings.Load() != 2 {
		t.Fatalf("failure amplified probes=%d", p.pings.Load())
	}
	failed.Store(false)
	now = now.Add(readinessFailureTTL)
	if !c.check(context.Background()).ready {
		t.Fatal("cache failed to recover")
	}
	p.verify = func(context.Context) error { return errors.New("checksum drift") }
	now = now.Add(readinessSuccessTTL)
	if got := c.check(context.Background()); got.ready || got.reason != "schema incompatible" {
		t.Fatalf("schema failure=%+v", got)
	}
}

func TestReadinessNonCooperativeProbeRemainsSingleAndWaitersAreBounded(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	p := &controlledReadinessPinger{ping: func(context.Context) error { <-release; return nil }}
	c := &readinessChecker{pinger: p, now: time.Now, timeout: 30 * time.Millisecond}
	start := time.Now()
	if c.check(context.Background()).ready {
		t.Fatal("stuck backend reported ready")
	}
	if time.Since(start) > time.Second {
		t.Fatal("first caller did not respect probe deadline")
	}
	start = time.Now()
	for i := 0; i < 100; i++ {
		if c.check(context.Background()).ready {
			t.Fatal("stuck backend reported ready")
		}
	}
	if time.Since(start) > time.Second || p.pings.Load() != 1 {
		t.Fatalf("expired flight amplification: elapsed=%s probes=%d", time.Since(start), p.pings.Load())
	}
	c.mu.Lock()
	flight := c.flight
	c.mu.Unlock()
	unblock()
	<-flight.done
	if c.check(context.Background()).ready || p.verifies.Load() != 0 {
		t.Fatal("late success or schema call after deadline")
	}
}

func TestReadinessCallerCancellationDoesNotCancelSharedProbe(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	p := &controlledReadinessPinger{ping: func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}}
	c := &readinessChecker{pinger: p, now: time.Now, timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan readinessResult, 1)
	go func() { first <- c.check(ctx) }()
	<-started
	cancel()
	if (<-first).ready {
		t.Fatal("canceled caller ready")
	}
	unblock()
	if !c.check(context.Background()).ready || p.pings.Load() != 1 {
		t.Fatal("caller cancellation killed shared probe")
	}
}

func TestReadinessRecoversProbePanic(t *testing.T) {
	for _, where := range []string{"ping", "schema"} {
		t.Run(where, func(t *testing.T) {
			p := &controlledReadinessPinger{}
			panicFn := func(context.Context) error { panic("private credential") }
			if where == "ping" {
				p.ping = panicFn
			} else {
				p.verify = panicFn
			}
			h := readinessHandler(p)
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				h(w, httptest.NewRequest("GET", "/ready", nil))
				if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("panic response=%d %s", w.Code, w.Body.String())
				}
			}
			if p.pings.Load() != 1 {
				t.Fatal("panic failure was not cached")
			}
		})
	}
}
