// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestCleanupDeliveredWorkerRoleHasOnlyRequiredMetadata(t *testing.T) {
	if os.Getenv("TUTOR_TEST_PG_DSN") == "" {
		t.Skip("PostgreSQL required")
	}
	s := setupTestDB(t)
	now := time.Now().UTC()
	if _, err := s.root.Exec(`INSERT INTO oauth_clients(client_id,client_name,redirect_uris,created_at,expires_at) VALUES('expired-maintenance','fixture','[]',$1,$2)`, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	nativeDeliveredRoles(t, s)
	nativeAssumeRole(t, s, "tutor_worker")
	for _, query := range []string{`SELECT token FROM refresh_tokens LIMIT 1`, `SELECT code FROM oauth_codes LIMIT 1`, `SELECT token_hash FROM account_tokens LIMIT 1`, `SELECT client_secret_hash FROM oauth_clients LIMIT 1`, `SELECT password_hash FROM users LIMIT 1`} {
		rows, err := s.root.Query(query)
		if err == nil {
			rows.Close()
			t.Fatalf("worker gained secret access: %s", query)
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("unexpected denial: %v", err)
		}
	}
	if err := s.WithTenantTx(t.Context(), models.LegacyPrincipal("L1").TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if _, err := tx.CleanupExpiredCodes(ctx); err != nil {
			return err
		}
		if _, err := tx.CleanupExpiredRefreshTokens(ctx); err != nil {
			return err
		}
		if _, err := tx.CleanupExpiredAccountTokens(ctx); err != nil {
			return err
		}
		removed, err := tx.CleanupExpiredOAuthClients(ctx)
		if err != nil {
			return err
		}
		if removed != 1 {
			t.Fatalf("expired clients removed=%d", removed)
		}
		if _, _, err := tx.CleanupRateLimitState(ctx, now.Add(-24*time.Hour), now.Add(-24*time.Hour)); err != nil {
			return err
		}
		if _, err := tx.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now); err != nil {
			return err
		}
		if _, err := tx.ExpirePastWebhookMessages(ctx, now); err != nil {
			return err
		}
		_, err = tx.PurgeJobRunsBefore(ctx, now.Add(-7*24*time.Hour))
		return err
	}); err != nil {
		t.Fatalf("delivered worker cleanup failed: %v", err)
	}
}
