package engine

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxIntegrationBackoff = 64 * time.Minute
const maxIntegrationRetryAfter = 24 * time.Hour

func (s *Scheduler) saasTimeNow() time.Time {
	if s.saasNow != nil {
		return s.saasNow().UTC()
	}
	return time.Now().UTC()
}

func integrationRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		attempt = 7
	}
	return time.Minute * time.Duration(1<<(attempt-1))
}

func (s *Scheduler) saasRetryAt(now time.Time, attempt int, retryAfter string) time.Time {
	sample := rand.Float64()
	if s.saasJitter != nil {
		sample = s.saasJitter()
	}
	if sample < 0 || sample > 1 || math.IsNaN(sample) {
		sample = 0.5
	}
	delay := time.Duration(float64(integrationRetryDelay(attempt)) * (0.75 + sample*0.5))
	if delay > maxIntegrationBackoff {
		delay = maxIntegrationBackoff
	}
	at := now.Add(delay)
	if after, ok := integrationRetryAfter(now, retryAfter); ok && after.After(at) {
		at = after
	}
	return at
}

func integrationRetryAfter(now time.Time, value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	digits := true
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxIntegrationRetryAfter/time.Second) {
			return now.Add(maxIntegrationRetryAfter), true
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	if at.Before(now) {
		at = now
	}
	if at.After(now.Add(maxIntegrationRetryAfter)) {
		at = now.Add(maxIntegrationRetryAfter)
	}
	return at, true
}
