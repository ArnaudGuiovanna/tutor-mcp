// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"slices"
	"testing"

	"tutor-mcp/models"
)

// Maintenance enumeration covers every tenant root, whatever its status, so
// that the cleanup job can reach expired credentials hidden by FORCE RLS in a
// suspended or closed tenant. Business enumeration stays active-only.
func TestListWorkerMaintenanceTenantScopesIncludesInactiveTenants(t *testing.T) {
	s := setupTestDB(t)
	ctx := t.Context()
	active := nativeTestTenant(t, s, "maintenance-active")
	suspended := nativeTestTenant(t, s, "maintenance-suspended")
	closed := nativeTestTenant(t, s, "maintenance-closed")
	if err := s.SetTenantStatus(ctx, nativeTestActor(), suspended, models.TenantStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTenantStatus(ctx, nativeTestActor(), closed, models.TenantStatusClosed); err != nil {
		t.Fatal(err)
	}
	worker := models.WorkerPrincipal{ActorID: "maintenance-test"}
	if _, _, err := s.ListWorkerMaintenanceTenantScopes(ctx, models.WorkerPrincipal{}, "", 10); err == nil {
		t.Fatal("invalid worker enumerated tenants")
	}
	if _, _, err := s.ListWorkerMaintenanceTenantScopes(ctx, worker, "", 1001); err == nil {
		t.Fatal("oversized page accepted")
	}
	collect := func(list func(string) ([]models.TenantScope, string, error)) []string {
		t.Helper()
		var ids []string
		after := ""
		for {
			// A page of one exercises the cursor on both filters.
			scopes, next, err := list(after)
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range scopes {
				if scope.UserID != "worker_"+worker.ActorID || scope.MembershipID != "worker_process" {
					t.Fatalf("scope is not a worker scope: %+v", scope)
				}
				ids = append(ids, scope.TenantID)
			}
			if next == "" {
				return ids
			}
			after = next
		}
	}
	maintenance := collect(func(after string) ([]models.TenantScope, string, error) {
		return s.ListWorkerMaintenanceTenantScopes(ctx, worker, after, 1)
	})
	business := collect(func(after string) ([]models.TenantScope, string, error) {
		return s.ListWorkerTenantScopes(ctx, worker, after, 1)
	})
	for _, id := range []string{active, suspended, closed} {
		if !slices.Contains(maintenance, id) {
			t.Fatalf("maintenance scopes %v miss %s", maintenance, id)
		}
	}
	if !slices.Contains(business, active) || slices.Contains(business, suspended) || slices.Contains(business, closed) {
		t.Fatalf("business scopes %v must contain only active tenants", business)
	}
	if !slices.IsSorted(maintenance) || len(maintenance) != len(business)+2 {
		t.Fatalf("maintenance=%v business=%v", maintenance, business)
	}
}
