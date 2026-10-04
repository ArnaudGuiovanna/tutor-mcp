// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// blockingEmailSender holds every delivery until released, standing in for a
// slow SMTP round trip.
type blockingEmailSender struct {
	release chan struct{}
	mu      sync.Mutex
	sent    []string
}

func (s *blockingEmailSender) record(kind, to string) error {
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, kind+":"+to)
	return nil
}

func (s *blockingEmailSender) SendVerification(_ context.Context, to, _ string) error {
	return s.record("verification", to)
}

func (s *blockingEmailSender) SendPasswordReset(_ context.Context, to, _ string) error {
	return s.record("reset", to)
}

func (s *blockingEmailSender) deliveries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// Before the fix the handler waited for SMTP only when the address had an
// account (recovery) or none yet (registration), so the response time told an
// observer which addresses exist despite identical response bodies.
func TestAccountEmailsDoNotDelayTheResponse(t *testing.T) {
	s, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	seedLearner(t, store, "known@example.com", "old-password-123")
	sender := &blockingEmailSender{release: make(chan struct{})}
	s.SetEmailSender(sender)

	within := func(name string, request func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			request()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s waited for email delivery", name)
		}
	}
	within("recovery of a known account", func() {
		csrf := accountCSRF(t, s, "/recover", s.HandleRecoverGet)
		rec := postAccountForm(t, "/recover", csrf, url.Values{"email": {"known@example.com"}}, s.HandleRecoverPost)
		if rec.Code != http.StatusAccepted {
			t.Errorf("recover status=%d", rec.Code)
		}
	})
	within("registration of a new address", func() {
		rec := postRegistrationStartNoDrain(t, s, "new@example.com", "register-csrf")
		if rec.Code != http.StatusAccepted {
			t.Errorf("registration status=%d body=%q", rec.Code, rec.Body.String())
		}
	})
	if got := sender.deliveries(); len(got) != 0 {
		t.Fatalf("deliveries completed before release: %v", got)
	}

	close(sender.release)
	drainBackgroundMail(t, s)
	got := map[string]bool{}
	for _, delivery := range sender.deliveries() {
		got[delivery] = true
	}
	if !got["reset:known@example.com"] || !got["verification:new@example.com"] || len(got) != 2 {
		t.Fatalf("deliveries after release=%v", sender.deliveries())
	}
}

func TestBackgroundMailBacklogIsBounded(t *testing.T) {
	s, _ := newTestServer(t)
	release := make(chan struct{})
	started := make(chan struct{}, backgroundMailConcurrency+1)
	for i := 0; i < backgroundMailConcurrency+5; i++ {
		s.runBackgroundMail(context.Background(), "test", func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		})
	}
	for i := 0; i < backgroundMailConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d background tasks started", i, backgroundMailConcurrency)
		}
	}
	select {
	case <-started:
		t.Fatal("a task beyond the concurrency bound started")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	drainBackgroundMail(t, s)
}
