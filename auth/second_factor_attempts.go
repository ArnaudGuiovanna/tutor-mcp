package auth

import (
	"context"
	"fmt"
	"time"

	storeport "tutor-mcp/store"
)

type secondFactorAttemptWindow struct {
	started time.Time
	count   int
}

// ReserveSecondFactorAttempt spends a verification slot before any factor is
// checked. Successful verifications consume a slot too: resetting the window
// on success could erase reservations made by concurrent requests.
// Windows are fixed, not sliding. Limits are 1..100 checks over at most 24h;
// production permits five checks in ten minutes, including successful checks.
func (t *LoginFailureTracker) ReserveSecondFactorAttempt(parent context.Context, key string, now time.Time) (int, time.Duration, error) {
	if parent == nil {
		parent = context.Background()
	}
	if t == nil || key == "" || t.threshold < 1 || t.threshold > 100 || t.window <= 0 || t.window > 24*time.Hour {
		return 0, 0, fmt.Errorf("invalid second-factor attempt budget")
	}
	if backend := t.getBackend(); backend != nil {
		strict, ok := backend.(storeport.SecondFactorAttemptBackend)
		if !ok {
			return 0, 0, fmt.Errorf("shared backend cannot reserve second-factor attempts")
		}
		ctx, cancel := loginFailureBackendContext(parent)
		defer cancel()
		// An unavailable shared authority must never become an empty local
		// budget on another node. Password accounting retains its fallback.
		return strict.ReserveSecondFactorAttempt(ctx, key, t.threshold, t.window, now)
	}
	if err := parent.Err(); err != nil {
		return 0, 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.secondFactorAttempts == nil {
		t.secondFactorAttempts = make(map[string]secondFactorAttemptWindow)
	}
	current, exists := t.secondFactorAttempts[key]
	if exists && !now.Before(current.started.Add(t.window)) {
		delete(t.secondFactorAttempts, key)
		exists = false
	}
	if !exists {
		if len(t.secondFactorAttempts) >= maxLoginFailureBuckets {
			for candidate, window := range t.secondFactorAttempts {
				if !now.Before(window.started.Add(t.window)) {
					delete(t.secondFactorAttempts, candidate)
				}
			}
		}
		if len(t.secondFactorAttempts) >= maxLoginFailureBuckets {
			// Evicting an active window would reopen its verification budget.
			return 0, 0, fmt.Errorf("second-factor attempt capacity reached")
		}
		current = secondFactorAttemptWindow{started: now}
	}
	if current.count >= t.threshold {
		return t.threshold + 1, current.started.Add(t.window).Sub(now), nil
	}
	current.count++
	t.secondFactorAttempts[key] = current
	return current.count, current.started.Add(t.window).Sub(now), nil
}
