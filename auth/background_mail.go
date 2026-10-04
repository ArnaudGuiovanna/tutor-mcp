// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"time"
)

// Account emails whose existence depends on the submitted address (password
// recovery, registration) are delivered after the response. A synchronous
// STARTTLS round trip takes hundreds of milliseconds and only happened for
// some addresses, so response time revealed which accounts exist even though
// every response body was identical.
const (
	backgroundMailConcurrency = 16
	backgroundMailTimeout     = 30 * time.Second
)

// runBackgroundMail runs task without delaying the response. It keeps the
// request's values (tenant scope, logging) but not its cancellation, and is
// bounded both in time and in concurrency. When every slot is busy the task is
// dropped and logged rather than run inline, which would bring the timing
// signal back; the per-IP rate limits keep that backlog out of reach of a
// single client.
func (s *OAuthServer) runBackgroundMail(parent context.Context, name string, task func(context.Context) error) {
	select {
	case s.mailSlots <- struct{}{}:
	default:
		s.logger.Warn("account email skipped: delivery backlog is full", "task", name)
		return
	}
	s.mailJobs.Add(1)
	go func() {
		defer s.mailJobs.Done()
		defer func() { <-s.mailSlots }()
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("account email task panicked", "task", name, "panic_type", fmt.Sprintf("%T", recovered))
			}
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), backgroundMailTimeout)
		defer cancel()
		if err := task(ctx); err != nil {
			s.logger.Error("account email delivery failed", "task", name, "error_type", authLogErrorType(err))
		}
	}()
}

// DrainBackgroundMail waits for queued account emails to finish, or for ctx to
// end. Call it during graceful shutdown after the HTTP server stops accepting
// requests.
func (s *OAuthServer) DrainBackgroundMail(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.mailJobs.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
