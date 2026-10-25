// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package db

import (
	"slices"
	"testing"
	"time"
	"tutor-mcp/models"
)

func TestNativeLearningOptInPreservesStaffAndRevokesPreviousSessions(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-learning")
	n, err := s.BootstrapNativeOwner(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: tenant, DisplayName: "Direction", PublicKey: nativeTestKey(61), RecoveryPublicKey: nativeTestKey(62)})
	if err != nil {
		t.Fatal(err)
	}
	session := nativeTestSession(n, "before-opt-in", []string{models.OAuthScopeFormationRead})
	actor, err := s.CreateNativeSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if s.dialect == DialectPostgres {
		nativeDeliveredRoles(t, s)
		nativeAssumeRole(t, s, "tutor_api")
	}
	forged := actor
	forged.MembershipID = "foreign-membership"
	if _, err = s.EnableNativeLearning(t.Context(), forged); err == nil {
		t.Fatal("accepted another membership")
	}
	result, err := s.EnableNativeLearning(t.Context(), actor)
	if err != nil || !result.Changed || result.LearnerID == "" || !slices.Equal(result.Roles, []string{models.RoleOwner, models.RoleLearner}) {
		t.Fatalf("opt-in=%#v err=%v", result, err)
	}
	if s.ValidateNativeSession(t.Context(), session.ID, actor, time.Now()) == nil {
		t.Fatal("old session survived membership change")
	}
	n, err = s.GetNativeIdentity(t.Context(), tenant, n.PublicKey, "session")
	if err != nil {
		t.Fatal(err)
	}
	next := nativeTestSession(n, "after-opt-in", []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite})
	learner, err := s.CreateNativeSession(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListLearnerFormations(t.Context(), learner, true, "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("opt-in enrolled person: %#v %v", page, err)
	}
	again, err := s.EnableNativeLearning(t.Context(), learner)
	if err != nil || again.Changed || again.LearnerID != result.LearnerID {
		t.Fatalf("non-idempotent opt-in: %#v %v", again, err)
	}
	if err = s.ValidateNativeSession(t.Context(), next.ID, learner, time.Now()); err != nil {
		t.Fatal("no-op revoked fresh session", err)
	}
}
