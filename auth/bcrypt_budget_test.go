// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestBcryptBudgetRejectsNPlusOneAndReleases(t *testing.T) {
	budget, err := newBcryptBudget(2)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	budget.compare = func(_, _ []byte) error {
		entered <- struct{}{}
		<-release
		return nil
	}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- budget.Compare(context.Background(), nil, nil) }()
	}
	<-entered
	<-entered
	if err := budget.Compare(context.Background(), nil, nil); !errors.Is(err, ErrBcryptBusy) {
		t.Fatalf("N+1 admission error=%v, want ErrBcryptBusy", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("admitted operation %d: %v", i, err)
		}
	}
	if err := budget.Compare(context.Background(), nil, nil); err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
}

func TestBcryptBudgetReleasesOnErrorAndHonorsCanceledContext(t *testing.T) {
	budget, err := newBcryptBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("bcrypt failed")
	var calls atomic.Int32
	budget.generate = func([]byte, int) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, sentinel
		}
		return []byte("hash"), nil
	}
	if _, err := budget.Generate(context.Background(), nil, bcrypt.MinCost); !errors.Is(err, sentinel) {
		t.Fatalf("first error=%v, want sentinel", err)
	}
	if hash, err := budget.Generate(context.Background(), nil, bcrypt.MinCost); err != nil || string(hash) != "hash" {
		t.Fatalf("slot not released after error: hash=%q err=%v", hash, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.Generate(canceled, nil, bcrypt.MinCost); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission error=%v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("bcrypt function called %d times, want 2", got)
	}
}

func postBudgetedLogin(t *testing.T, server *OAuthServer, email, password, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"csrf_token": {csrf}, "mode": {"login"}, "client_id": {"cid"},
		"redirect_uri": {"https://good.example/cb"}, "response_type": {"code"},
		"resource": {testOAuthResource}, "code_challenge": {"challenge"},
		"code_challenge_method": {"S256"}, "email": {email},
		"password": {password}, "approve_client": {"yes"},
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
	rec := httptest.NewRecorder()
	server.HandleAuthorizePost(rec, req)
	return rec
}

func TestLoginAbsentWrongAndInactiveAreUniformAndRecordedOnce(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		password string
	}{
		{name: "absent", email: "absent@example.com", password: "some-password"},
		{name: "wrong", email: "active@example.com", password: "wrong-password"},
		{name: "inactive", email: "inactive@example.com", password: "known-password"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, store := newTestServer(t)
			seedClient(t, store, "cid", "https://good.example/cb")
			switch tc.name {
			case "wrong":
				seedLearner(t, store, tc.email, "correct-password")
			case "inactive":
				hash, err := bcrypt.GenerateFromPassword([]byte(tc.password), bcrypt.MinCost)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.CreateUnverifiedLearner(context.Background(), tc.email, string(hash), "", ""); err != nil {
					t.Fatal(err)
				}
			}

			budget, err := newBcryptBudget(1)
			if err != nil {
				t.Fatal(err)
			}
			originalCompare := budget.compare
			var comparisons atomic.Int32
			budget.compare = func(hash, password []byte) error {
				comparisons.Add(1)
				return originalCompare(hash, password)
			}
			server.bcrypt = budget

			rec := postBudgetedLogin(t, server, tc.email, tc.password, "uniform-csrf")
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("Retry-After") != "" || rec.Header().Get("Location") != "" {
				t.Fatalf("response status=%d retry=%q location=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get("Location"))
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Invalid email or password.") || strings.Contains(body, tc.email) {
				t.Fatalf("non-uniform credential response body=%q", body)
			}
			if got := comparisons.Load(); got != 2 {
				t.Fatalf("bcrypt comparisons=%d, want real plus timing-padding comparison", got)
			}
			server.loginFailures.mu.Lock()
			bucket := server.loginFailures.fails[NormalizeEmail(tc.email)]
			failureCount := 0
			if bucket != nil {
				failureCount = len(bucket.stamps)
			}
			server.loginFailures.mu.Unlock()
			if failureCount != 1 {
				t.Fatalf("recorded failures=%d, want exactly 1", failureCount)
			}
		})
	}
}

func TestLoginAbsentAndLegacyExistingTimingDistributionsStayClose(t *testing.T) {
	server, store := newTestServer(t)
	seedClient(t, store, "cid", "https://good.example/cb")
	// seedLearner deliberately creates a legacy minimum-cost hash. The login
	// path must pad it to current cost just as it pads the unknown-account path.
	seedLearner(t, store, "legacy@example.com", "correct-password")
	server.loginFailures = NewLoginFailureTracker(1000, time.Hour)

	const samplesPerPath = 6
	durations := map[string][]time.Duration{"absent": {}, "existing": {}}
	for pair := 0; pair < samplesPerPath; pair++ {
		order := []string{"absent", "existing"}
		if pair%2 == 1 {
			order[0], order[1] = order[1], order[0]
		}
		for _, path := range order {
			email := "absent@example.com"
			if path == "existing" {
				email = "legacy@example.com"
			}
			started := time.Now()
			rec := postBudgetedLogin(
				t, server, email, "wrong-password",
				fmt.Sprintf("timing-%s-%d", path, pair),
			)
			durations[path] = append(durations[path], time.Since(started))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s timing sample status=%d", path, rec.Code)
			}
		}
	}

	absentMedian := medianDuration(durations["absent"])
	existingMedian := medianDuration(durations["existing"])
	ratio := float64(absentMedian) / float64(existingMedian)
	// This deliberately tolerant statistical gate catches the historical
	// cost-12-vs-cost-4 oracle (roughly an order of magnitude) without making
	// normal shared-runner jitter flaky.
	if ratio < 0.60 || ratio > 1.67 {
		t.Fatalf(
			"timing medians diverged: absent=%s existing=%s ratio=%.2f samples=%v/%v",
			absentMedian, existingMedian, ratio, durations["absent"], durations["existing"],
		)
	}
}

func medianDuration(values []time.Duration) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func TestLoginBcryptSaturationIsUniformAndTransient(t *testing.T) {
	budget, err := newBcryptBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	budget.compare = func(_, _ []byte) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- budget.Compare(context.Background(), nil, nil) }()
	<-entered

	for _, tc := range []struct {
		name  string
		email string
		seed  bool
	}{
		{name: "absent", email: "absent@example.com"},
		{name: "existing", email: "active@example.com", seed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store := newTestServer(t)
			seedClient(t, store, "cid", "https://good.example/cb")
			if tc.seed {
				seedLearner(t, store, tc.email, "correct-password")
			}
			server.bcrypt = budget
			rec := postBudgetedLogin(t, server, tc.email, "wrong-password", "busy-csrf")
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != bcryptBusyRetryAfter {
				t.Fatalf("status=%d Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
			}
			if body := rec.Body.String(); !strings.Contains(body, "temporarily busy") || strings.Contains(body, tc.email) {
				t.Fatalf("busy response is not uniform: %q", body)
			}
			server.loginFailures.mu.Lock()
			_, recorded := server.loginFailures.fails[NormalizeEmail(tc.email)]
			server.loginFailures.mu.Unlock()
			if recorded {
				t.Fatal("capacity rejection must not be recorded as a credential failure")
			}
		})
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("occupying comparison: %v", err)
	}
}

func rejectingBcryptBudget(t *testing.T) *bcryptBudget {
	t.Helper()
	budget, err := newBcryptBudget(1)
	if err != nil {
		t.Fatal(err)
	}
	budget.generate = func([]byte, int) ([]byte, error) { return nil, ErrBcryptBusy }
	budget.compare = func([]byte, []byte) error { return ErrBcryptBusy }
	return budget
}

func postRegistrationStart(t *testing.T, server *OAuthServer, email, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{
		"csrf_token": {csrf}, "mode": {"register"}, "client_id": {"cid"},
		"redirect_uri": {"https://good.example/cb"}, "response_type": {"code"},
		"resource": {testOAuthResource}, "code_challenge": {"challenge"},
		"code_challenge_method": {"S256"}, "email": {email},
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: csrf})
	rec := httptest.NewRecorder()
	server.HandleAuthorizePost(rec, req)
	return rec
}

func assertBcryptBusyResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != bcryptBusyRetryAfter {
		t.Fatalf("status=%d Retry-After=%q body=%q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}

func TestEveryHandlerBcryptEntryPointUsesBudget(t *testing.T) {
	t.Run("pending registration hash", func(t *testing.T) {
		server, store := newTestServer(t)
		seedClient(t, store, "cid", "https://good.example/cb")
		server.bcrypt = rejectingBcryptBudget(t)
		assertBcryptBusyResponse(t, postRegistrationStart(t, server, "new@example.com", "register-busy"))
	})

	t.Run("confidential client comparison", func(t *testing.T) {
		server, store := newTestServer(t)
		seedConfidentialClient(t, store, "confidential", "https://good.example/cb", "client-secret")
		server.bcrypt = rejectingBcryptBudget(t)
		form := url.Values{
			"grant_type": {"authorization_code"}, "client_id": {"confidential"},
			"client_secret": {"client-secret"}, "code": {"unused-code"},
			"resource": {testOAuthResource},
		}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		server.HandleToken(rec, req)
		assertBcryptBusyResponse(t, rec)
	})

	t.Run("dynamic client secret hash", func(t *testing.T) {
		server, _ := newTestServer(t)
		server.bcrypt = rejectingBcryptBudget(t)
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
			`{"client_name":"Confidential","redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.HandleRegister(rec, req)
		assertBcryptBusyResponse(t, rec)
	})

	t.Run("email activation password hash", func(t *testing.T) {
		server, store := newTestServer(t)
		seedClient(t, store, "cid", "https://good.example/cb")
		started := postRegistrationStart(t, server, "verify@example.com", "verify-start")
		if started.Code != http.StatusAccepted {
			t.Fatalf("registration status=%d body=%q", started.Code, started.Body.String())
		}
		verificationURL, err := url.Parse(server.emailSender.(*testEmailSender).verificationLinks[0])
		if err != nil {
			t.Fatal(err)
		}
		csrf := accountCSRF(t, server, verificationURL.RequestURI(), server.HandleVerifyEmailGet)
		server.bcrypt = rejectingBcryptBudget(t)
		rec := postAccountForm(t, "/verify-email", csrf, url.Values{
			"token": {verificationURL.Query().Get("token")}, "password": {"owner-password-123"},
			"password_confirm": {"owner-password-123"}, "approve_client": {"yes"},
		}, server.HandleVerifyEmailPost)
		assertBcryptBusyResponse(t, rec)
	})

	t.Run("password reset hash", func(t *testing.T) {
		server, store := newTestServer(t)
		seedLearner(t, store, "reset-busy@example.com", "old-password-123")
		recoverCSRF := accountCSRF(t, server, "/recover", server.HandleRecoverGet)
		postAccountForm(t, "/recover", recoverCSRF, url.Values{"email": {"reset-busy@example.com"}}, server.HandleRecoverPost)
		resetURL, err := url.Parse(server.emailSender.(*testEmailSender).resetLinks[0])
		if err != nil {
			t.Fatal(err)
		}
		resetCSRF := accountCSRF(t, server, resetURL.RequestURI(), server.HandleResetPasswordGet)
		server.bcrypt = rejectingBcryptBudget(t)
		rec := postAccountForm(t, "/reset-password", resetCSRF, url.Values{
			"token": {resetURL.Query().Get("token")}, "password": {"new-password-123"},
			"password_confirm": {"new-password-123"},
		}, server.HandleResetPasswordPost)
		assertBcryptBusyResponse(t, rec)
	})
}
