// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

// TestSaaSNoisyNeighbourLoadGate is the reproducible M5 staging gate:
//
//	TUTOR_SAAS_LOAD_TEST=1 TUTOR_TEST_PG_DSN=postgres://... \
//	  go test ./db -run TestSaaSNoisyNeighbourLoadGate -v -timeout 5m
//
// One tenant saturates the reservation path while a small tenant must retain
// bounded latency and its independent hard limit.
func TestSaaSNoisyNeighbourLoadGate(t *testing.T) {
	if os.Getenv("TUTOR_SAAS_LOAD_TEST") != "1" {
		t.Skip("set TUTOR_SAAS_LOAD_TEST=1")
	}
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Fatal("TUTOR_TEST_PG_DSN is required")
	}
	s := setupTestDB(t)
	if s.dialect != DialectPostgres {
		t.Fatal("SaaS load gate requires PostgreSQL")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	large := ownerPrincipal(t, s)
	if err := s.ConfigureEntitlement(ctx, large, "exports_month", 1000, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	operator := models.ControlPlanePrincipal{ActorID: "load-operator", Roles: []string{models.RolePlatformAdmin}, Reason: "noisy neighbour gate", RequestID: "load-gate"}
	smallTenant, err := s.ProvisionTenant(ctx, operator, fmt.Sprintf("small-%d", now.UnixNano()), "Small tenant", "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	smallScope := models.TenantScope{TenantID: smallTenant.ID, UserID: "load-small-user", MembershipID: "load-small-membership"}
	smallActor := models.Principal{TenantID: smallTenant.ID, UserID: smallScope.UserID, MembershipID: smallScope.MembershipID,
		Roles: []string{models.RoleBillingAdmin}, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1}
	if err := s.ConfigureEntitlement(ctx, smallActor, "exports_month", 20, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 400)
	for worker := 0; worker < 20; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for item := 0; item < 20; item++ {
				_, _, err := s.ReserveEntitlement(ctx, large.TenantScope(), fmt.Sprintf("large-%d-%d", worker, item), "exports_month", 1, now, time.Hour)
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	close(start)
	latencies := make([]time.Duration, 0, 20)
	for item := 0; item < 20; item++ {
		started := time.Now()
		if _, _, err := s.ReserveEntitlement(ctx, smallScope, fmt.Sprintf("small-%d", item), "exports_month", 1, now, time.Hour); err != nil {
			t.Fatal(err)
		}
		latencies = append(latencies, time.Since(started))
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[18]
	const p95Budget = 2 * time.Second
	if p95 > p95Budget {
		t.Fatalf("small-tenant reservation p95=%s exceeds %s", p95, p95Budget)
	}
	if _, _, err := s.ReserveEntitlement(ctx, smallScope, "small-over-limit", "exports_month", 1, now, time.Hour); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("small tenant limit escaped under load: %v", err)
	}
	var largeReserved, smallReserved int64
	if err := s.queryRow(ctx, `SELECT reserved_value FROM tenant_entitlements WHERE tenant_id = ? AND entitlement_key = 'exports_month'`, large.TenantID).Scan(&largeReserved); err != nil {
		t.Fatal(err)
	}
	if err := s.queryRow(ctx, `SELECT reserved_value FROM tenant_entitlements WHERE tenant_id = ? AND entitlement_key = 'exports_month'`, smallTenant.ID).Scan(&smallReserved); err != nil {
		t.Fatal(err)
	}
	if largeReserved != 400 || smallReserved != 20 {
		t.Fatalf("large/small reservations=%d/%d", largeReserved, smallReserved)
	}
	t.Logf("noisy-neighbour gate: large=400 concurrent reservations, small p95=%s budget=%s", p95, p95Budget)
}
