package engine

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func saasConsumerTestStore(t *testing.T) (*sql.DB, *db.Store) {
	t.Helper()
	raw, s, _ := rawTestSetup(t, "")
	keyring, err := db.NewIntegrationSecretKeyring("key:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "key")
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	if err := db.ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	return raw, s
}

func seedSaaSConsumerTenant(t *testing.T, raw *sql.DB, s *db.Store, id string, jobs int) models.TenantScope {
	t.Helper()
	now := time.Now().UTC().Add(-time.Second)
	if _, err := raw.Exec(`INSERT INTO tenants(id,slug,name,status,region,policy_json,created_at,updated_at) VALUES (?,?,?,'active','default','{}',?,?)`, id, id, id, now, now); err != nil {
		t.Fatal(err)
	}
	actor := models.Principal{TenantID: id, UserID: "user_" + id, MembershipID: "member_" + id, Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1}
	integration, _, err := s.CreateTenantIntegration(t.Context(), actor, "webhook", "https://hooks.customer.test/"+id, []string{"test.event"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range jobs {
		event := fmt.Sprintf("%s-event-%03d", id, i)
		payload, _ := json.Marshal(models.TenantWebhookJob{IntegrationID: integration.ID, EventID: event, EventType: "test.event", Payload: json.RawMessage(`{"ok":true}`)})
		if err := s.EnqueueAsyncJob(t.Context(), actor.TenantScope(), models.AsyncJob{ID: event, TenantID: id, Kind: "tenant_webhook_delivery", IdempotencyKey: event, PayloadJSON: string(payload), MaxAttempts: 8, CreatedAt: now.Add(time.Duration(i)), AvailableAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	return models.TenantScope{TenantID: id, UserID: "worker_consumer-test", MembershipID: "worker_process"}
}

func saasTLSClient(server *httptest.Server) *http.Client {
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "example.com" // httptest certificate identity
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func waitSaaSTest(t *testing.T, ctx context.Context, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(message, ctx.Err())
	}
}

func startSaaSTestCallback(t *testing.T, s *Scheduler) <-chan scheduledJobResult {
	t.Helper()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.cron.Stop() // use the registered production callback, without wall-clock ticks
	t.Cleanup(s.Stop)
	done := make(chan scheduledJobResult, 1)
	go func() { done <- s.registeredDistributed["saas_async_jobs"]() }()
	return done
}

func TestSaaSConsumerSlowTenantDoesNotBlockFastTenant(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	seedSaaSConsumerTenant(t, raw, store, "a_slow", 2)
	seedSaaSConsumerTenant(t, raw, store, "b_fast", 5)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	fast := make(chan struct{}, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	var count atomic.Int32
	var mu sync.Mutex
	active := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active[r.URL.Path]++
		if active[r.URL.Path] > 1 {
			t.Error("multiple jobs in flight for one tenant")
		}
		mu.Unlock()
		defer func() { mu.Lock(); active[r.URL.Path]--; mu.Unlock() }()
		if r.URL.Path == "/a_slow" && strings.HasSuffix(r.Header.Get("X-Tutor-Event-ID"), "000") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		} else if r.URL.Path == "/b_fast" && count.Add(1) == 3 {
			fast <- struct{}{}
		}
		w.WriteHeader(204)
	}))
	defer func() { once.Do(func() { close(release) }); server.Close() }()
	s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
	s.integrationClient = saasTLSClient(server)
	done := startSaaSTestCallback(t, s)
	waitSaaSTest(t, ctx, started, "slow tenant never reached HTTP")
	waitSaaSTest(t, ctx, fast, "three fast jobs did not complete while first slow job was blocked")
	if err := store.RawDB().PingContext(ctx); err != nil {
		t.Fatal("HTTP pinned SQL pool", err)
	}
	once.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal("consumer did not drain", ctx.Err())
	}
	var completed int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM async_jobs WHERE status='completed'`).Scan(&completed); err != nil || completed != 7 {
		t.Fatalf("completed=%d err=%v", completed, err)
	}
}

func TestSaaSConsumerCapacityAcquiredBeforeClaimsAcrossWindows(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	for i := range 5 {
		seedSaaSConsumerTenant(t, raw, store, fmt.Sprintf("capacity_%d", i), 1)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer func() { once.Do(func() { close(release) }); server.Close() }()
	s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
	s.integrationClient = saasTLSClient(server)
	done := startSaaSTestCallback(t, s)
	for range 4 {
		waitSaaSTest(t, ctx, entered, "four workers did not enter HTTP")
	}
	for i := range 10 {
		s.runDistributedJob("saas_async_jobs", fmt.Sprint("overlap-", i), s.registeredDistributed["saas_async_jobs"])
	}
	var processing, pending, untouched int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM async_jobs WHERE status='processing'`).Scan(&processing); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN attempt_count=0 THEN 1 ELSE 0 END),0) FROM async_jobs WHERE status='pending'`).Scan(&pending, &untouched); err != nil {
		t.Fatal(err)
	}
	if processing != 4 || pending != 1 || untouched != 1 {
		t.Fatalf("preclaims escaped capacity: processing=%d pending=%d untouched=%d", processing, pending, untouched)
	}
	once.Do(func() { close(release) })
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type saasNoReadBody struct{ reads, closes atomic.Int32 }

func (b *saasNoReadBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, errors.New("body read failure")
}
func (b *saasNoReadBody) Close() error { b.closes.Add(1); return nil }

func TestSaaSConsumerHTTPStatusPolicyIsDurable(t *testing.T) {
	raw, store := saasConsumerTestStore(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		code   int
		after  string
		status string
		delay  time.Duration
	}{
		{"accepted_body_unread", 200, "", "completed", 0},
		{"accepted_empty", 204, "", "completed", 0},
		{"redirect", 302, "", "dead_letter", 0},
		{"bad_request_large_body", 400, "", "dead_letter", 0},
		{"timeout", 408, "", "pending", time.Minute},
		{"too_early", 425, "", "pending", time.Minute},
		{"rate_delta", 429, "300", "pending", 5 * time.Minute},
		{"rate_date", 429, now.Add(10 * time.Minute).Format(http.TimeFormat), "pending", 10 * time.Minute},
		{"rate_invalid", 429, "invalid", "pending", time.Minute},
		{"rate_huge", 429, strings.Repeat("9", 200), "pending", 24 * time.Hour},
		{"server", 503, "", "pending", time.Minute},
		{"invalid_status", 600, "", "dead_letter", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := seedSaaSConsumerTenant(t, raw, store, "policy_"+tc.name, 1)
			// Keep persistence on the same injected clock even under -race,
			// where later table cases can begin seconds after this test's now.
			if _, err := raw.Exec(`UPDATE async_jobs SET available_at=?,created_at=? WHERE tenant_id=?`, now, now, scope.TenantID); err != nil {
				t.Fatal(err)
			}
			s := NewTenantDistributedScheduler(store, quietLogger(), "consumer-test")
			s.currentTenantScope = &scope
			s.saasNow = func() time.Time { return now }
			s.saasJitter = func() float64 { return .5 }
			body := &saasNoReadBody{}
			var sends atomic.Int32
			s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(*http.Request) (*http.Response, error) {
				sends.Add(1)
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Retry-After": []string{tc.after}}, Body: body}, nil
			})}
			if result := s.consumeSaaSJobs(); result.failed() {
				t.Fatal(result)
			}
			if result := s.consumeSaaSJobs(); result.failed() {
				t.Fatal(result)
			}
			var status, deliveryStatus string
			var attempts int
			var available time.Time
			if err := raw.QueryRow(`SELECT status,attempt_count,available_at FROM async_jobs WHERE tenant_id=?`, scope.TenantID).Scan(&status, &attempts, &available); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT status FROM integration_deliveries WHERE tenant_id=?`, scope.TenantID).Scan(&deliveryStatus); err != nil {
				t.Fatal(err)
			}
			if status != tc.status || attempts != 1 || sends.Load() != 1 || body.reads.Load() != 0 || body.closes.Load() != 1 {
				t.Fatalf("status=%s attempt=%d sends=%d body read/close=%d/%d", status, attempts, sends.Load(), body.reads.Load(), body.closes.Load())
			}
			if tc.status == "pending" {
				if delta := available.Sub(now) - tc.delay; delta > time.Second || delta < -time.Second {
					t.Fatalf("retry available=%v want %v", available, now.Add(tc.delay))
				}
				if deliveryStatus != "failed" {
					t.Fatal(deliveryStatus)
				}
			} else if tc.status == "dead_letter" && deliveryStatus != "dead_letter" {
				t.Fatal(deliveryStatus)
			}
		})
	}
}

func TestSaaSRetryAfterBoundsAndJitter(t *testing.T) {
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	s := &Scheduler{}
	for _, sample := range []float64{0, .5, 1} {
		s.saasJitter = func() float64 { return sample }
		for _, attempt := range []int{-1, 1, 2, 7, 1000} {
			at := s.saasRetryAt(now, attempt, "")
			if at.Before(now) || at.After(now.Add(maxIntegrationBackoff)) {
				t.Fatal("jitter escaped backoff bound", at)
			}
			floor := now.Add(30 * time.Minute)
			if got := s.saasRetryAt(now, attempt, floor.Format(http.TimeFormat)); got.Before(floor) {
				t.Fatalf("jitter scheduled before Retry-After: %v", got)
			}
		}
	}
	for _, value := range []string{"-1", "1.5", "infinity", "2026" + strings.Repeat("x", 20)} {
		if _, ok := integrationRetryAfter(now, value); ok {
			t.Fatal("invalid Retry-After accepted", value)
		}
	}
	if got, ok := integrationRetryAfter(now, "184467440737095516160"); !ok || !got.Equal(now.Add(24*time.Hour)) {
		t.Fatal("overflow was not bounded", got, ok)
	}
}

var _ io.ReadCloser = (*saasNoReadBody)(nil)
