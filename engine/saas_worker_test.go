// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestSaaSWorkerDeliversSignedHTTPSAndCompletesDurableState(t *testing.T) {
	var secret string
	received := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := r.Header.Get("X-Tutor-Event-ID"); got != "event-worker-1" {
			t.Errorf("event header = %q", got)
		}
		if got := r.Header.Get("X-Tutor-Event-Type"); got != "formation.version.published" {
			t.Errorf("event type header = %q", got)
		}
		timestamp, err := strconv.ParseInt(r.Header.Get("X-Tutor-Timestamp"), 10, 64)
		if err != nil {
			t.Errorf("timestamp header: %v", err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + ".event-worker-1."))
		_, _ = mac.Write(body)
		if got, want := r.Header.Get("X-Tutor-Signature"), "v1="+hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		if got := r.Header.Get("X-Tutor-Secret-Version"); got != "1" {
			t.Errorf("secret version = %q", got)
		}
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	n := time.Now().UnixNano()
	raw, err := sql.Open("sqlite", fmt.Sprintf("file:saas_worker_%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", n))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(raw)
	keyring, err := db.NewIntegrationSecretKeyring(
		"webhooks:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "webhooks")
	if err != nil {
		t.Fatal(err)
	}
	store.SetIntegrationSecretKeyring(keyring)
	if err := db.ConfigureTenantIntegrationAllowedHosts(store, []string{"hooks.customer.test"}); err != nil {
		t.Fatal(err)
	}
	control := models.ControlPlanePrincipal{ActorID: "test-platform", Roles: []string{models.RolePlatformAdmin}, Reason: "worker test", RequestID: "worker-test-1"}
	tenant, err := store.ProvisionTenant(context.Background(), control, fmt.Sprintf("worker-%d", n), "Worker test", "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	userID, membershipID := fmt.Sprintf("user_%d", n), fmt.Sprintf("membership_%d", n)
	email := fmt.Sprintf("owner-%d@test.invalid", n)
	if _, err := raw.Exec(`INSERT INTO users (id, email, normalized_email, password_hash, status, token_version, created_at, updated_at)
		VALUES (?, ?, ?, 'not-a-login', 'active', 1, ?, ?)`, userID, email, email, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO tenant_memberships
		(id, tenant_id, user_id, roles_json, status, version, created_at, updated_at)
		VALUES (?, ?, ?, '["owner"]', 'active', 1, ?, ?)`, membershipID, tenant.ID, userID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	owner := models.Principal{TenantID: tenant.ID, UserID: userID, MembershipID: membershipID,
		Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1}
	integration, rawSecret, err := store.CreateTenantIntegration(context.Background(), owner, "webhook",
		"https://hooks.customer.test/events", []string{"formation.version.published"})
	if err != nil {
		t.Fatal(err)
	}
	secret = rawSecret
	now := time.Now().UTC()
	if err := store.AppendOutboxEvent(context.Background(), owner.TenantScope(), models.OutboxEvent{
		ID: "event-worker-1", TenantID: tenant.ID, AggregateType: "formation_version", AggregateID: "version-1",
		EventType: "formation.version.published", IdempotencyKey: "worker-event-1",
		PayloadJSON: `{"version_id":"version-1"}`, AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	scheduler := NewScheduler(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	scheduler.owner = "worker-test"
	scheduler.integrationClient = &http.Client{Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	scope := owner.TenantScope()
	scheduler.currentTenantScope = &scope
	if result := scheduler.relaySaaSOutbox(); result.failed() {
		t.Fatalf("relay failed: %s", result.FailureCode)
	}
	if result := scheduler.consumeSaaSJobs(); result.failed() {
		t.Fatalf("consumer failed: %s", result.FailureCode)
	}
	select {
	case <-received:
	default:
		t.Fatal("HTTPS integration did not receive the event")
	}
	var jobStatus, deliveryStatus string
	if err := raw.QueryRow(`SELECT status FROM async_jobs WHERE tenant_id = ?`, tenant.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT status FROM integration_deliveries
		WHERE tenant_id = ? AND integration_id = ?`, tenant.ID, integration.ID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "completed" || deliveryStatus != "delivered" {
		t.Fatalf("job/delivery status = %q/%q", jobStatus, deliveryStatus)
	}
}

func TestSaaSWorkerConsumesTenantDSARExportJob(t *testing.T) {
	n := time.Now().UnixNano()
	raw, err := sql.Open("sqlite", fmt.Sprintf("file:saas_dsar_worker_%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", n))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := raw.Exec(`INSERT INTO learners
		(id, email, password_hash, objective, created_at, email_verified_at)
		VALUES ('L1', 'dsar-worker@example.test', 'hash', 'test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(raw)
	if err := store.EnsureRecoveryEnrollment(context.Background(), "L1"); err != nil {
		t.Fatal(err)
	}
	learner, err := store.GetPrincipalForLearner(context.Background(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMembershipAuthorization(context.Background(), learner.TenantScope(),
		models.MembershipStatusActive, []string{models.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordMembershipMFAVerification(context.Background(), learner.TenantScope(), now); err != nil {
		t.Fatal(err)
	}
	owner, err := store.GetPrincipalForLearner(context.Background(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestTenantDSAR(context.Background(), owner, owner.LearnerID,
		"export", "verified learner request")
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewTenantDistributedScheduler(store, slog.New(slog.NewTextHandler(io.Discard, nil)), "dsar-worker")
	scheduler.owner = "dsar-worker-lease"
	scope := owner.TenantScope()
	scope.UserID = "worker_dsar-worker"
	scope.MembershipID = "worker_process"
	scheduler.currentTenantScope = &scope
	if result := scheduler.consumeSaaSJobs(); result.failed() {
		t.Fatalf("DSAR consumer failed: %s", result.FailureCode)
	}
	var requestStatus, jobStatus, resultJSON string
	if err := raw.QueryRow(`SELECT status, result_json FROM tenant_dsar_requests
		WHERE tenant_id = ? AND id = ?`, owner.TenantID, request.ID).Scan(&requestStatus, &resultJSON); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT status FROM async_jobs
		WHERE tenant_id = ? AND kind = 'tenant_dsar'`, owner.TenantID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if requestStatus != "completed" || jobStatus != "completed" || resultJSON == "{}" {
		t.Fatalf("DSAR request/job=%q/%q result=%s", requestStatus, jobStatus, resultJSON)
	}
}
