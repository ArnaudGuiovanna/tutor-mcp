package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type factorBudgetAccounts struct {
	storeport.InstitutionAccountStore
	checks  atomic.Int32
	deleted atomic.Int32
	succeed bool
	entered chan struct{}
	release chan struct{}
}

func (a *factorBudgetAccounts) VerifySecondFactor(context.Context, models.TenantScope, string, time.Time) error {
	a.checks.Add(1)
	if a.entered != nil {
		a.entered <- struct{}{}
		<-a.release
	}
	if a.succeed {
		return nil
	}
	return errors.New("invalid factor")
}
func (a *factorBudgetAccounts) GetConsoleSession(context.Context, models.ConsoleSessionCredential) (*models.ConsoleSession, error) {
	now := time.Now().UTC()
	return &models.ConsoleSession{TenantID: "tenant", UserID: "user", MembershipID: "member", MembershipVersion: 1, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, nil
}
func (a *factorBudgetAccounts) GetConsoleMembership(context.Context, models.TenantScope) (*models.ConsoleMembership, error) {
	return &models.ConsoleMembership{Roles: []string{models.RoleOwner, models.RoleLearner}, LearnerID: "learner", Version: 1, MFARequired: true, MFAEnrolled: true}, nil
}
func (a *factorBudgetAccounts) UpdateConsoleSession(context.Context, models.ConsoleSessionCredential, time.Time, int64, *time.Time) error {
	return nil
}
func (a *factorBudgetAccounts) DeleteConsoleSession(context.Context, models.ConsoleSessionCredential) error {
	a.deleted.Add(1)
	return nil
}
func (a *factorBudgetAccounts) HasConfirmedTOTP(context.Context, models.TenantScope) (bool, error) {
	return true, nil
}

type unavailableFactorBudget struct{ failingLoginFailureBackend }

func (unavailableFactorBudget) ReserveSecondFactorAttempt(context.Context, string, int, time.Duration, time.Time) (int, time.Duration, error) {
	return 0, 0, errors.New("shared authority unavailable")
}

func TestSecondFactorBudgetAcrossTwoServersAndSQLBackends(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var raw *sql.DB
			var store *db.Store
			if dialect == "postgres" {
				dsn := os.Getenv("TUTOR_TEST_PG_DSN")
				if dsn == "" {
					t.Skip("set TUTOR_TEST_PG_DSN for shared PostgreSQL MFA budget")
				}
				raw, store = postgresAuthTestStoreInSchema(t, dsn, "p1_auth_mfa_budget")
			} else {
				_, store = newTestServer(t)
				raw = store.RawDB()
			}
			for _, success := range []bool{false, true} {
				t.Run(fmt.Sprintf("success=%v", success), func(t *testing.T) {
					accounts := &factorBudgetAccounts{succeed: success, entered: make(chan struct{}, 100), release: make(chan struct{})}
					var releaseOnce sync.Once
					releaseChecks := func() { releaseOnce.Do(func() { close(accounts.release) }) }
					defer releaseChecks()
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					first := NewOAuthServer(store, "https://test.example", logger)
					var other *db.Store
					if dialect == "postgres" {
						other = db.NewStoreWithDialect(raw, db.DialectPostgres)
					} else {
						other = db.NewStore(raw)
					}
					second := NewOAuthServer(other, "https://test.example", logger)
					first.accounts, second.accounts = accounts, accounts
					first.SetLoginFailureBackend(db.NewLoginFailureBackend(store))
					second.SetLoginFailureBackend(db.NewLoginFailureBackend(other))
					scope := models.TenantScope{TenantID: "tenant", UserID: fmt.Sprintf("user-%v", success), MembershipID: "member"}
					now := time.Now().UTC()
					start := make(chan struct{})
					done := make(chan error, 100)
					for i := 0; i < 100; i++ {
						go func(i int) {
							<-start
							s := first
							if i%2 == 1 {
								s = second
							}
							_, _, err := s.checkSecondFactor(context.Background(), scope, "123456", now)
							done <- err
						}(i)
					}
					close(start)
					for i := 0; i < 5; i++ {
						select {
						case <-accounts.entered:
						case <-time.After(5 * time.Second):
							t.Fatal("five reserved verifications did not enter")
						}
					}
					// Denials must complete while all five admitted checks are still in flight.
					for i := 0; i < 95; i++ {
						select {
						case err := <-done:
							if err != nil {
								t.Error(err)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("concurrent requests bypassed the shared budget")
						}
					}
					if accounts.checks.Load() != 5 {
						t.Fatalf("checks=%d want5", accounts.checks.Load())
					}
					releaseChecks()
					for i := 0; i < 5; i++ {
						if err := <-done; err != nil {
							t.Error(err)
						}
					}
					if _, retry, err := first.checkSecondFactor(context.Background(), scope, "123456", now.Add(time.Second)); err != nil || retry <= 0 {
						t.Fatalf("success erased reservations: retry=%v err=%v", retry, err)
					}
					if accounts.checks.Load() != 5 {
						t.Fatal("successful checks reset the verification budget")
					}
					if ok, _, err := second.checkSecondFactor(context.Background(), scope, "123456", now.Add(10*time.Minute)); err != nil || ok != success {
						t.Fatalf("window did not reopen: ok=%v err=%v", ok, err)
					}
					if accounts.checks.Load() != 6 {
						t.Fatalf("new window checks=%d", accounts.checks.Load())
					}
				})
			}
		})
	}
}

func TestSecondFactorBudgetConfiguredWindowAndBounds(t *testing.T) {
	_, store := newTestServer(t)
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			tracker := NewLoginFailureTracker(2, 37*time.Second)
			if shared {
				tracker.SetBackend(db.NewLoginFailureBackend(store))
			}
			ctx := context.Background()
			now := time.Now().UTC()
			key := fmt.Sprintf("mfa:configured-%v", shared)
			for i := 1; i <= 4; i++ {
				count, retry, err := tracker.ReserveSecondFactorAttempt(ctx, key, now)
				if err != nil || count != min(i, 3) || retry != 37*time.Second {
					t.Fatalf("count=%d retry=%v err=%v", count, retry, err)
				}
			}
			if count, retry, err := tracker.ReserveSecondFactorAttempt(ctx, key, now.Add(36*time.Second)); err != nil || count != 3 || retry != time.Second {
				t.Fatalf("before expiry count=%d retry=%v err=%v", count, retry, err)
			}
			if count, _, err := tracker.ReserveSecondFactorAttempt(ctx, key, now.Add(37*time.Second)); err != nil || count != 1 {
				t.Fatalf("configured expiry count=%d err=%v", count, err)
			}
			tracker = NewLoginFailureTracker(100, time.Minute)
			if shared {
				tracker.SetBackend(db.NewLoginFailureBackend(store))
			}
			for i := 1; i <= 101; i++ {
				count, _, err := tracker.ReserveSecondFactorAttempt(ctx, key+"-max", now)
				if err != nil || count != i {
					t.Fatalf("schema boundary attempt%d count=%d err=%v", i, count, err)
				}
			}
			if shared {
				count, err := db.NewLoginFailureBackend(store).CountInWindow(ctx, key+"-max", time.Minute, now)
				if err != nil || count != 100 {
					t.Fatalf("persisted schema-bound counter=%d err=%v", count, err)
				}
			}
			for _, bad := range []struct {
				limit  int
				window time.Duration
				key    string
			}{{101, time.Minute, key}, {1, 0, key}, {1, 25 * time.Hour, key}, {1, time.Minute, ""}} {
				tk := NewLoginFailureTracker(bad.limit, bad.window)
				if shared {
					tk.SetBackend(db.NewLoginFailureBackend(store))
				}
				if _, _, err := tk.ReserveSecondFactorAttempt(ctx, bad.key, now); err == nil {
					t.Fatal("invalid strict budget accepted")
				}
			}
			// Password resets never affect the separate strict MFA budget.
			tracker.ResetContext(ctx, "somebody@example.test")
			if count, _, err := tracker.ReserveSecondFactorAttempt(ctx, key+"-max", now); err != nil || count != 101 {
				t.Fatalf("password reset changed MFA: count=%d err=%v", count, err)
			}
		})
	}
}

func TestSecondFactorBudgetFailureIsClosedWithoutDestroyingSessions(t *testing.T) {
	s, _ := newTestServer(t)
	accounts := &factorBudgetAccounts{}
	s.accounts = accounts
	s.SetLoginFailureBackend(unavailableFactorBudget{})
	scope := models.TenantScope{TenantID: "tenant", UserID: "user", MembershipID: "member"}
	if _, _, err := s.checkSecondFactor(context.Background(), scope, "123456", time.Now()); err == nil {
		t.Fatal("backend outage fell back locally")
	}
	for _, path := range []string{"/console/mfa", "/learn/mfa"} {
		csrf := path + "-csrf"
		form := url.Values{"csrf_token": {csrf}, "code": {"123456"}}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: accountCSRFCookieName, Value: csrf})
		name := consoleSessionCookieName
		if strings.HasPrefix(path, "/learn") {
			name = learnerSessionCookieName
		}
		req.AddCookie(&http.Cookie{Name: name, Value: "valid-session"})
		rec := httptest.NewRecorder()
		if name == consoleSessionCookieName {
			s.HandleConsoleMFAPost(rec, req)
		} else {
			s.HandleLearnerMFA(rec, req)
		}
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" {
			t.Fatalf("%s status%d body%s", path, rec.Code, rec.Body.String())
		}
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == name && cookie.MaxAge < 0 {
				t.Fatal("outage deleted browser session")
			}
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader("totp_code=123456"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, ok := s.verifyAuthorizeSecondFactor(rec, req, authPageData{}, scope); ok || rec.Code != 503 {
		t.Fatalf("authorize outage ok=%v status%d", ok, rec.Code)
	}
	if accounts.checks.Load() != 0 || accounts.deleted.Load() != 0 {
		t.Fatalf("outage checked=%d deleted=%d", accounts.checks.Load(), accounts.deleted.Load())
	}
}

func TestSecondFactorLocalCapacityNeverEvictsActiveBudgets(t *testing.T) {
	tracker := NewLoginFailureTracker(1, time.Minute)
	now := time.Now().UTC()
	ctx := context.Background()
	for i := 0; i < maxLoginFailureBuckets; i++ {
		if _, _, err := tracker.ReserveSecondFactorAttempt(ctx, fmt.Sprint(i), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := tracker.ReserveSecondFactorAttempt(ctx, "overflow", now); err == nil {
		t.Fatal("active window evicted")
	}
	if count, _, err := tracker.ReserveSecondFactorAttempt(ctx, "0", now); err != nil || count != 2 {
		t.Fatalf("active budget reopened count%d err%v", count, err)
	}
	if count, _, err := tracker.ReserveSecondFactorAttempt(ctx, "overflow", now.Add(time.Minute)); err != nil || count != 1 {
		t.Fatalf("expired capacity not reclaimed count%d err%v", count, err)
	}
}
