// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Existing DSAR mechanics fixtures erase their own owner. Keep an active
// successor so these tests exercise erasure rather than orphaning a tenant.
func ensureErasureReplacementOwner(t *testing.T, s *Store, owner models.Principal) {
	t.Helper()
	count, err := s.countActiveOwners(t.Context(), owner.TenantID, owner.MembershipID)
	if err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		return
	}
	next := formationStudent(t, s, "erasure-successor@example.test")
	if err = s.UpdateTenantMember(t.Context(), owner, next.MembershipID, models.MembershipStatusActive, []string{models.RoleOwner, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
}

func institutionCall(t *testing.T, s *Store, p models.Principal, action string, in models.InstitutionRequest) map[string]any {
	t.Helper()
	out, err := s.InstitutionAction(t.Context(), p, action, in)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	raw, _ := json.Marshal(out)
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestInstitutionNpubIdentifiersArePublicCanonicalAndChecksummed(t *testing.T) {
	known := "npub1neu20jws9dmrp6rzg4c4r5knjvlgwrqhnklrhvtk6ea60mgdypls5ulh2y"
	key, err := institutionPublicKey(known)
	if err != nil || key != "9e78a7c9d02b7630e862457151d2d3933e870c179dbe3bb176d67ba7ed0d207f" {
		t.Fatalf("key=%s err=%v", key, err)
	}
	for _, raw := range []string{known[:62] + "x", strings.Replace(known, "npub1n", "npub1N", 1), "nsec" + known[4:], strings.Repeat("0", 64)} {
		if _, err = institutionPublicKey(raw); err == nil {
			t.Fatalf("invalid identifier accepted %q", raw)
		}
	}
}

func TestInstitutionNativeMembersOwnershipAndSelfErasure(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-settings")
	owner := trainingPrincipal(t, s, tenant, 151, true)
	overview := institutionCall(t, s, owner, "overview", models.InstitutionRequest{})
	if overview["capabilities"].(map[string]any)["last_owner"] != true {
		t.Fatal("last owner was not identified")
	}
	if _, err := s.InstitutionAction(t.Context(), owner, "request_erasure", models.InstitutionRequest{Confirm: true, Key: "erase-last", Reason: "remove my account"}); err == nil {
		t.Fatal("last owner erased")
	}
	invite := models.InstitutionRequest{Npub: nativeTestKey(153), RecoveryNpub: nativeTestKey(154), Name: "Invited member", Roles: []string{models.RoleLearner}, Confirm: true, Key: "invite"}
	created := institutionCall(t, s, owner, "invite_member", invite)
	memberID := created["member_id"].(string)
	replayed := institutionCall(t, s, owner, "invite_member", invite)
	if replayed["member_id"] != memberID {
		t.Fatal("invitation retry created a second account")
	}
	invite.Key = "invite-again"
	if _, err := s.InstitutionAction(t.Context(), owner, "invite_member", invite); !errors.Is(err, storeport.ErrAlreadyMember) {
		t.Fatalf("duplicate=%v", err)
	}
	n, err := s.GetNativeIdentity(t.Context(), tenant, nativeTestKey(153), "session")
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	var email, password *string
	if err = s.queryRow(t.Context(), `SELECT identity_mode,email,password_hash FROM learners WHERE id=?`, n.LearnerID).Scan(&mode, &email, &password); err != nil {
		t.Fatal(err)
	}
	if mode != "pubkey" || email != nil || password != nil {
		t.Fatal("native invitation fabricated email credentials")
	}
	member, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, "settings-member", []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite, models.OAuthScopeFormationContribute}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.InstitutionAction(t.Context(), member, "list_members", models.InstitutionRequest{}); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("learner read member directory: %v", err)
	}
	page := institutionCall(t, s, owner, "list_members", models.InstitutionRequest{Limit: 1})
	if len(page["items"].([]any)) != 1 || page["next_after"] == "" {
		t.Fatal("member directory not paginated")
	}
	institutionCall(t, s, owner, "transfer_ownership", models.InstitutionRequest{MemberID: memberID, Confirm: true, Key: "transfer", Reason: "new institution owner"})
	var ownerRoles, memberRoles string
	if err = s.queryRow(t.Context(), `SELECT roles_json FROM tenant_memberships WHERE id=?`, owner.MembershipID).Scan(&ownerRoles); err != nil {
		t.Fatal(err)
	}
	if err = s.queryRow(t.Context(), `SELECT roles_json FROM tenant_memberships WHERE id=?`, memberID).Scan(&memberRoles); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ownerRoles, `"owner"`) || !strings.Contains(memberRoles, `"owner"`) {
		t.Fatal("ownership transfer was not atomic")
	}
	if err = s.ValidateNativeSession(t.Context(), "settings-member", member, time.Now().UTC()); err == nil {
		t.Fatal("recipient old session still authorized after role change")
	}
	previous, err := s.GetNativeIdentity(t.Context(), tenant, nativeTestKey(151), "session")
	if err != nil {
		t.Fatal(err)
	}
	former, err := s.CreateNativeSession(t.Context(), nativeTestSession(previous, "former-owner", []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite, models.OAuthScopeFormationContribute}))
	if err != nil {
		t.Fatal(err)
	}
	request := models.InstitutionRequest{Confirm: true, Key: "self-erasure", Reason: "remove my institution data"}
	accepted := institutionCall(t, s, former, "request_erasure", request)
	id := accepted["request"].(map[string]any)["id"].(string)
	if accepted["request"].(map[string]any)["status"] != "pending" {
		t.Fatal("asynchronous erasure announced completed")
	}
	replayed = institutionCall(t, s, former, "request_erasure", request)
	if replayed["request"].(map[string]any)["id"] != id {
		t.Fatal("erasure retried as a second job")
	}
	status := institutionCall(t, s, former, "get_erasure_status", models.InstitutionRequest{RequestID: id})
	if status["request"].(map[string]any)["status"] != "pending" {
		t.Fatal("erasure status was fabricated")
	}
	if _, err = s.InstitutionAction(t.Context(), former, "get_erasure_status", models.InstitutionRequest{RequestID: "another-request"}); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("foreign erasure=%v", err)
	}
	request.Reason = "changed intent"
	if _, err = s.InstitutionAction(t.Context(), former, "request_erasure", request); !errors.Is(err, storeport.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed idempotency payload=%v", err)
	}
}

func TestInstitutionActionsCannotChangeAnotherTenantOrCreateOwnerByInvitation(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "institution-a")
	foreign := nativeTestTenant(t, s, "institution-b")
	a := trainingPrincipal(t, s, tenant, 155, true)
	b := trainingPrincipal(t, s, foreign, 157, true)
	for _, action := range []string{"remove_member", "transfer_ownership", "set_member_roles"} {
		_, err := s.InstitutionAction(t.Context(), a, action, models.InstitutionRequest{MemberID: b.MembershipID, Roles: []string{models.RoleLearner}, Key: action, Confirm: true})
		if err == nil {
			t.Fatalf("%s changed another tenant", action)
		}
	}
	_, err := s.InstitutionAction(t.Context(), a, "invite_member", models.InstitutionRequest{Npub: nativeTestKey(159), RecoveryNpub: nativeTestKey(160), Name: "Escalation", Roles: []string{models.RoleOwner}, Key: "owner-invite", Confirm: true})
	if !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("owner invitation=%v", err)
	}
	if _, err = s.InstitutionAction(t.Context(), a, "remove_member", models.InstitutionRequest{MemberID: a.MembershipID, Key: "self-remove", Confirm: true}); err == nil {
		t.Fatal("last owner removed own membership")
	}
}

func TestInstitutionErasureRechecksLastOwnerAtWorkerBoundary(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "erasure-owner-race")
	owner := trainingPrincipal(t, s, tenant, 181, true)
	successor := trainingPrincipal(t, s, tenant, 183, false)
	if err := s.UpdateTenantMember(t.Context(), owner, successor.MembershipID, models.MembershipStatusActive, []string{models.RoleOwner, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	request, err := s.RequestTenantDSAR(t.Context(), owner, owner.LearnerID, "erase", "explicit account erasure")
	if err != nil {
		t.Fatal(err)
	}
	// Ownership can change while an asynchronous erasure waits in its queue.
	if err = s.UpdateTenantMember(t.Context(), owner, successor.MembershipID, models.MembershipStatusRevoked, []string{models.RoleOwner, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	scope, worker := erasureWorkerScope(owner)
	_, _, err = s.ProcessTenantDSARErasureBatch(t.Context(), scope, worker, request.ID, 1, time.Now().UTC())
	if !errors.Is(err, storeport.ErrTenantDSARBlocked) {
		t.Fatalf("last-owner erasure at worker boundary=%v", err)
	}
	if err = s.ValidatePrincipal(t.Context(), owner); err != nil {
		t.Fatalf("worker revoked the only owner before blocking: %v", err)
	}
}
