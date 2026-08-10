// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type failingLoginFailureBackend struct{}

func (failingLoginFailureBackend) Record(context.Context, string, time.Time) (int, error) {
	return 0, errors.New("backend unavailable")
}

func (failingLoginFailureBackend) CountInWindow(context.Context, string, time.Duration, time.Time) (int, error) {
	return 0, errors.New("backend unavailable")
}

func (failingLoginFailureBackend) Reset(context.Context, string) error {
	return errors.New("backend unavailable")
}

// Issue #36 part 4: per-account login failure lockout. The tracker is
// thread-safe, time-windowed, and resettable on success.

func TestLoginFailureTracker_BlocksAtThreshold(t *testing.T) {
	tk := NewLoginFailureTracker(3, time.Minute)
	if !tk.Allow("a@x") {
		t.Fatal("first attempt must be allowed")
	}
	for i := 0; i < 3; i++ {
		tk.Record("a@x")
	}
	if tk.Allow("a@x") {
		t.Error("expected lockout after threshold reached")
	}
	if !tk.Allow("b@x") {
		t.Error("other emails must NOT be locked out")
	}
}

func TestLoginFailureTracker_ResetUnlocks(t *testing.T) {
	tk := NewLoginFailureTracker(2, time.Minute)
	tk.Record("a@x")
	tk.Record("a@x")
	if tk.Allow("a@x") {
		t.Fatal("expected lockout after 2 fails")
	}
	tk.Reset("a@x")
	if !tk.Allow("a@x") {
		t.Error("Reset should clear the failure history")
	}
}

func TestLoginFailureTracker_StaleFailuresExpire(t *testing.T) {
	tk := NewLoginFailureTracker(2, 10*time.Millisecond)
	tk.Record("a@x")
	tk.Record("a@x")
	if tk.Allow("a@x") {
		t.Fatal("expected lockout immediately")
	}
	time.Sleep(20 * time.Millisecond)
	if !tk.Allow("a@x") {
		t.Error("expected window to decay; tracker still locked")
	}
}

func TestLoginFailureTracker_DisabledWhenThresholdNonPositive(t *testing.T) {
	tk := NewLoginFailureTracker(0, time.Minute)
	for i := 0; i < 100; i++ {
		tk.Record("a@x")
	}
	if !tk.Allow("a@x") {
		t.Error("threshold ≤ 0 must disable the tracker")
	}
}

func TestLoginFailureTracker_NilSafe(t *testing.T) {
	var tk *LoginFailureTracker
	if !tk.Allow("a@x") {
		t.Error("nil tracker must Allow")
	}
	if got := tk.Record("a@x"); got != 0 {
		t.Errorf("nil tracker Record = %d, want 0", got)
	}
	tk.Reset("a@x") // must not panic
}

func TestLoginFailureTracker_ConcurrentAccessSafe(t *testing.T) {
	tk := NewLoginFailureTracker(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				tk.Record("shared@x")
				tk.Allow("shared@x")
			}
		}()
	}
	wg.Wait()
	// 50 goroutines × 20 records = 1000 — exactly at threshold (Allow uses <).
	if tk.Allow("shared@x") {
		t.Errorf("expected lockout at 1000 fails; tracker still allows")
	}
}

func TestLoginFailureTracker_BackendFailureUsesLocalAccountBucket(t *testing.T) {
	tk := NewLoginFailureTracker(2, time.Minute)
	tk.SetBackend(failingLoginFailureBackend{})

	if got := tk.Record("learner@example.test"); got != 1 {
		t.Fatalf("first fallback record = %d, want 1", got)
	}
	if got := tk.Record("learner@example.test"); got != 2 {
		t.Fatalf("second fallback record = %d, want 2", got)
	}
	if tk.Allow("learner@example.test") {
		t.Fatal("backend outage must not bypass per-account lockout")
	}
	tk.Reset("learner@example.test")
	if !tk.Allow("learner@example.test") {
		t.Fatal("reset should clear the local fallback state")
	}
}

func TestLoginFailureTrackerBoundsUniqueAccountState(t *testing.T) {
	tk := NewLoginFailureTracker(5, time.Hour)
	for i := 0; i < maxLoginFailureBuckets+500; i++ {
		tk.Record(fmt.Sprintf("account-%d@example.test", i))
	}
	if got := len(tk.fails); got != maxLoginFailureBuckets {
		t.Fatalf("failure buckets=%d, want cap %d", got, maxLoginFailureBuckets)
	}
	if tk.lru.Len() != maxLoginFailureBuckets {
		t.Fatalf("LRU entries=%d, want cap %d", tk.lru.Len(), maxLoginFailureBuckets)
	}
}
