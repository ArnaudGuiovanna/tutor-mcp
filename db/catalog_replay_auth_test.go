// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestCatalogReplayRechecksPermissionsForEveryMutation(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	var versionID, cohortID string
	operations := []struct {
		name string
		call func(models.Principal, string) (bool, error)
	}{
		{"formation", func(actor models.Principal, key string) (bool, error) {
			_, version, replayed, err := s.CreateFormationDraftIdempotent(ctx, actor, key, "Replay authorization", "")
			if err == nil {
				versionID = version.ID
			}
			return replayed, err
		}},
		{"module", func(actor models.Principal, key string) (bool, error) {
			_, replayed, err := s.AddFormationModuleIdempotent(ctx, actor, key, versionID, models.FormationModuleInput{StableKey: "m", Title: "Module"})
			return replayed, err
		}},
		{"concept", func(actor models.Principal, key string) (bool, error) {
			_, replayed, err := s.AddFormationConceptIdempotent(ctx, actor, key, versionID, models.FormationConceptInput{ModuleStableKey: "m", StableKey: "c", Label: "Concept"})
			return replayed, err
		}},
		{"publish", func(actor models.Principal, key string) (bool, error) {
			_, replayed, err := s.PublishFormationVersionIdempotent(ctx, actor, key, versionID)
			return replayed, err
		}},
		{"cohort", func(actor models.Principal, key string) (bool, error) {
			cohort, replayed, err := s.CreateCohortIdempotent(ctx, actor, key, versionID, "Cohort", 2, nil, nil)
			if err == nil {
				cohortID = cohort.ID
			}
			return replayed, err
		}},
		{"enrollment", func(actor models.Principal, key string) (bool, error) {
			_, replayed, err := s.EnrollMembershipIdempotent(ctx, actor, key, cohortID, owner.MembershipID, `{}`)
			return replayed, err
		}},
	}
	for _, op := range operations {
		if replayed, err := op.call(owner, op.name); err != nil || replayed {
			t.Fatalf("seed %s: replayed=%v err=%v", op.name, replayed, err)
		}
	}
	for _, op := range operations {
		if replayed, err := op.call(owner, op.name); err != nil || !replayed {
			t.Fatalf("authorized replay %s: replayed=%v err=%v", op.name, replayed, err)
		}
	}
	readOnly := owner
	readOnly.Scopes = []string{models.OAuthScopeLearnerRead}
	for _, op := range operations {
		if replayed, err := op.call(readOnly, op.name); replayed || !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Fatalf("read-only replay %s: replayed=%v err=%v", op.name, replayed, err)
		}
	}
	// Another fully authorized actor cannot borrow a tenant-wide replay key.
	otherLearner, err := s.CreateLearner(ctx, "other-replayer@example.invalid", "hash", "learn", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.GetPrincipalForLearner(ctx, otherLearner.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, other.TenantScope(), models.MembershipStatusActive, []string{models.RolePedagogyManager}); err != nil {
		t.Fatal(err)
	}
	other, err = s.GetPrincipalForLearner(ctx, otherLearner.ID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if replayed, err := op.call(other, op.name); replayed || err == nil || !strings.Contains(err.Error(), "idempotency key conflict") {
			t.Fatalf("different actor replay %s: replayed=%v err=%v", op.name, replayed, err)
		}
	}
	if _, err := s.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	learner, err := s.GetPrincipalForLearner(ctx, owner.LearnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		for _, actor := range []models.Principal{learner, owner} {
			for _, key := range []string{op.name, "new-" + op.name} {
				if replayed, err := op.call(actor, key); replayed || !errors.Is(err, storeport.ErrInvalidPrincipal) {
					t.Fatalf("revoked/stale principal %s key=%s: replayed=%v err=%v", op.name, key, replayed, err)
				}
			}
		}
	}
}

func TestCatalogHTTPReplayAfterRoleDowngradeIsForbidden(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	owner := ownerPrincipal(t, s)
	if _, _, _, err := s.CreateFormationDraftIdempotent(ctx, owner, "known-key", "Course", "Description"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembershipAuthorization(ctx, owner.TenantScope(), models.MembershipStatusActive, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	learner, err := s.GetPrincipalForLearner(ctx, owner.LearnerID, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_ED25519_KEYS", "")
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("test-only-", 4))))
	if err := auth.LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	issuer := "https://catalog-test.invalid"
	token, err := auth.GenerateJWTForPrincipalAndScope(issuer, auth.MCPResource(issuer), "test-client", learner, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.BearerMiddlewareWithPrincipalValidator(issuer, models.OAuthScopeLearner, s, adminapi.New(s, nil).Handler())
	for _, key := range []string{"new-key", "known-key"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/catalog/formations", strings.NewReader(`{"name":"Course","description":"Description"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), `"formation"`) {
			t.Errorf("key=%s returned %d: %s", key, response.Code, response.Body.String())
		}
	}
}
