// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func institutionTestStore(t *testing.T) *Store {
	t.Helper()
	s := setupTestDB(t)
	keyring, err := NewIntegrationSecretKeyring(
		"mfa:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "mfa",
	)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(keyring)
	return s
}

var testOperator = models.ControlPlanePrincipal{
	ActorID: "operator", Roles: []string{models.RolePlatformAdmin},
	Reason: "test", RequestID: "req-1",
}

// seedInstitutionOwner signs up an institution and returns its owner scope.
func seedInstitutionOwner(t *testing.T, s *Store, slug, email string) models.TenantScope {
	t.Helper()
	ctx := context.Background()
	credential := models.SignupCredential{Token: "signup-" + slug}
	if err := s.CreatePendingSignup(ctx, credential, email, "Institution "+slug, slug, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	membership, err := s.CompleteSignup(ctx, credential, "plan_legacy", "", "bcrypt-hash")
	if err != nil {
		t.Fatal(err)
	}
	return models.TenantScope{TenantID: membership.TenantID, UserID: membership.UserID, MembershipID: membership.ID}
}

// verifiedOwner completes the owner's TOTP ceremony and returns its console
// principal together with the TOTP seed.
func verifiedOwner(t *testing.T, s *Store, scope models.TenantScope) (models.Principal, string) {
	t.Helper()
	ctx := context.Background()
	membership, err := s.GetConsoleMembership(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	principal := ConsolePrincipal(scope, *membership)
	credentialID, secret, err := s.BeginTOTPEnrollment(ctx, principal, "phone")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-2 * time.Minute)
	code, _ := totpCode(secret, at)
	if _, err := s.ConfirmTOTPEnrollment(ctx, principal, credentialID, code, at); err != nil {
		t.Fatal(err)
	}
	version, err := s.EnsureMembershipMFAVerified(ctx, scope, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	principal.TokenVersion = version
	return principal, secret
}

func TestTOTPCeremonyRecoveryCodesAndReplay(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	scope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	membership, err := s.GetConsoleMembership(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !membership.MFARequired || membership.MFAVerified || membership.MFAEnrolled {
		t.Fatalf("new owner MFA state = %+v", membership)
	}
	principal := ConsolePrincipal(scope, *membership)
	credentialID, secret, err := s.BeginTOTPEnrollment(ctx, principal, "phone")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.September, 29, 10, 0, 5, 0, time.UTC)
	code, _ := totpCode(secret, at)
	if err := s.VerifySecondFactor(ctx, scope, code, at); err == nil {
		t.Fatal("an unconfirmed authenticator verified a sign-in")
	}
	if enrolled, _ := s.HasConfirmedTOTP(ctx, scope); enrolled {
		t.Fatal("unconfirmed authenticator reported as enrolled")
	}
	if _, err := s.ConfirmTOTPEnrollment(ctx, principal, credentialID, "123456", at); err == nil {
		t.Fatal("wrong confirmation code accepted")
	}
	codes, err := s.ConfirmTOTPEnrollment(ctx, principal, credentialID, code, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != mfaRecoveryCodeCount {
		t.Fatalf("recovery codes = %d", len(codes))
	}
	if enrolled, _ := s.HasConfirmedTOTP(ctx, scope); !enrolled {
		t.Fatal("confirmed authenticator not reported")
	}
	// The confirmation burned its time step.
	if err := s.VerifySecondFactor(ctx, scope, code, at); err == nil {
		t.Fatal("confirmation code replayed at sign-in")
	}
	later := at.Add(time.Minute)
	next, _ := totpCode(secret, later)
	if err := s.VerifySecondFactor(ctx, scope, next, later); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifySecondFactor(ctx, scope, next, later); err == nil {
		t.Fatal("TOTP code replayed")
	}
	if err := s.VerifySecondFactor(ctx, scope, "  "+codes[0]+" ", later); err != nil {
		t.Fatalf("recovery code refused: %v", err)
	}
	if err := s.VerifySecondFactor(ctx, scope, codes[0], later); err == nil {
		t.Fatal("recovery code accepted twice")
	}
	other := seedInstitutionOwner(t, s, "other", "other@acme.test")
	if err := s.VerifySecondFactor(ctx, other, codes[1], later); err == nil {
		t.Fatal("recovery code accepted for another user")
	}

	// Re-enrolling revokes the previous authenticator once confirmed.
	newID, newSecret, err := s.BeginTOTPEnrollment(ctx, principal, "new phone")
	if err != nil {
		t.Fatal(err)
	}
	evenLater := later.Add(time.Minute)
	oldCode, _ := totpCode(secret, evenLater)
	newCode, _ := totpCode(newSecret, evenLater)
	if _, err := s.ConfirmTOTPEnrollment(ctx, principal, newID, newCode, evenLater); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifySecondFactor(ctx, scope, oldCode, evenLater.Add(30*time.Second)); err == nil {
		t.Fatal("revoked authenticator still accepted")
	}
	if err := s.VerifySecondFactor(ctx, scope, codes[2], evenLater); err == nil {
		t.Fatal("recovery codes survived re-enrollment")
	}
}

func TestEnsureMembershipMFAVerifiedBumpsVersionOnce(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	scope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	first, err := s.EnsureMembershipMFAVerified(ctx, scope, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureMembershipMFAVerified(ctx, scope, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != first {
		t.Fatalf("versions = %d then %d, want 2 then 2", first, second)
	}
}

func TestConsoleSessionLifecycle(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	scope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	credential := models.ConsoleSessionCredential{Token: "session-token"}
	if err := s.CreateConsoleSession(ctx, credential, scope, 1, nil, time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("expired session created")
	}
	if err := s.CreateConsoleSession(ctx, credential, scope, 1, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	session, err := s.GetConsoleSession(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if session.TenantScope() != scope || session.MFAVerifiedAt != nil || session.TokenHash == credential.Token {
		t.Fatalf("session = %+v", session)
	}
	verifiedAt := time.Now().UTC()
	if err := s.UpdateConsoleSession(ctx, credential, verifiedAt, 3, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	session, err = s.GetConsoleSession(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if session.MembershipVersion != 3 || session.MFAVerifiedAt == nil {
		t.Fatalf("updated session = %+v", session)
	}
	if err := s.DeleteConsoleSession(ctx, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConsoleSession(ctx, credential); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("deleted session lookup = %v", err)
	}
}

func TestInvitationAcceptanceCreatesAccountAndMembership(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	ownerScope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	owner, _ := verifiedOwner(t, s, ownerScope)

	_, rawToken, err := s.CreateTenantInvitation(ctx, owner, "Trainer@Acme.test",
		[]string{models.RolePedagogyManager, models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	credential := models.InvitationCredential{Token: rawToken}
	preview, err := s.PreviewTenantInvitation(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Email != "trainer@acme.test" || preview.ExistingUser || preview.TenantName != "Institution acme" {
		t.Fatalf("preview = %+v", preview)
	}
	membership, err := s.AcceptTenantInvitationWithNewUser(ctx, credential, "bcrypt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if membership.LearnerID == "" || !membership.MFARequired || membership.TenantID != owner.TenantID {
		t.Fatalf("membership = %+v", membership)
	}
	if _, err := s.AcceptTenantInvitationWithNewUser(ctx, credential, "bcrypt-hash"); err == nil {
		t.Fatal("invitation accepted twice")
	}
	user, err := s.GetLocalUserByEmail(ctx, "trainer@acme.test")
	if err != nil || user.ID != membership.UserID || user.EmailVerifiedAt == nil {
		t.Fatalf("invited user = %+v, %v", user, err)
	}
	memberships, err := s.ListActiveMembershipsForUser(ctx, user.ID)
	if err != nil || len(memberships) != 1 || !memberships[0].MFARequired {
		t.Fatalf("memberships = %+v, %v", memberships, err)
	}

	// A second invitation for an existing address must be accepted by signing in.
	_, second, err := s.CreateTenantInvitation(ctx, owner, "owner@acme.test",
		[]string{models.RoleLearner}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewTenantInvitation(ctx, models.InvitationCredential{Token: second})
	if err != nil || !preview.ExistingUser {
		t.Fatalf("existing-user preview = %+v, %v", preview, err)
	}
	if _, err := s.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: second}, "x"); !errors.Is(err, storeport.ErrAccountExists) {
		t.Fatalf("duplicate account error = %v", err)
	}
	if _, err := s.AcceptTenantInvitation(ctx, second, owner.UserID); !errors.Is(err, storeport.ErrAlreadyMember) {
		t.Fatalf("existing member error = %v", err)
	}

	invitations, err := s.ListTenantInvitations(ctx, owner)
	if err != nil || len(invitations) != 1 {
		t.Fatalf("pending invitations = %+v, %v", invitations, err)
	}
	if err := s.RevokeTenantInvitation(ctx, owner, invitations[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewTenantInvitation(ctx, models.InvitationCredential{Token: second}); err == nil {
		t.Fatal("revoked invitation still previewable")
	}
}

func TestOwnerInvitationBootstrapsProvisionedTenant(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	tenant, err := s.ProvisionTenant(ctx, testOperator, "school", "School", "eu", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwnerInvitation(ctx, testOperator, "tenant_missing", "a@b.test", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("owner invitation for an unknown tenant")
	}
	raw, err := s.CreateOwnerInvitation(ctx, testOperator, tenant.ID, "Head@School.test", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	membership, err := s.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: raw}, "bcrypt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(membership.Roles, []string{models.RoleOwner}) || membership.LearnerID != "" || !membership.MFARequired {
		t.Fatalf("owner membership = %+v", membership)
	}
}

func TestUpdateTenantMemberGuards(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	ownerScope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	owner, _ := verifiedOwner(t, s, ownerScope)

	invite := func(email string, roles ...string) *models.TenantMembership {
		t.Helper()
		_, raw, err := s.CreateTenantInvitation(ctx, owner, email, roles, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		membership, err := s.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: raw}, "hash")
		if err != nil {
			t.Fatal(err)
		}
		return membership
	}
	adminMembership := invite("admin@acme.test", models.RoleAdmin)
	learner := invite("alice@acme.test", models.RoleLearner)

	if err := s.UpdateTenantMember(ctx, owner, ownerScope.MembershipID, models.MembershipStatusSuspended, []string{models.RoleOwner}); err == nil {
		t.Fatal("owner suspended its own membership")
	}
	if err := s.UpdateTenantMember(ctx, owner, ownerScope.MembershipID, models.MembershipStatusActive, []string{models.RoleAdmin}); err == nil {
		t.Fatal("the last owner gave up the owner role")
	}

	adminScope := models.TenantScope{TenantID: owner.TenantID, UserID: adminMembership.UserID, MembershipID: adminMembership.ID}
	admin, _ := verifiedOwner(t, s, adminScope)
	if err := s.UpdateTenantMember(ctx, admin, ownerScope.MembershipID, models.MembershipStatusActive, []string{models.RoleAdmin}); err == nil {
		t.Fatal("an admin demoted the owner")
	}
	if err := s.UpdateTenantMember(ctx, admin, learner.ID, models.MembershipStatusActive, []string{models.RoleOwner}); err == nil {
		t.Fatal("an admin granted the owner role")
	}

	// Promoting a learner to trainer keeps the learner profile and requires MFA.
	if err := s.UpdateTenantMember(ctx, admin, learner.ID, models.MembershipStatusActive,
		[]string{models.RoleTrainer, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	learnerScope := models.TenantScope{TenantID: owner.TenantID, UserID: learner.UserID, MembershipID: learner.ID}
	state, err := s.GetConsoleMembership(ctx, learnerScope)
	if err != nil || !state.MFARequired || state.LearnerID != learner.LearnerID {
		t.Fatalf("promoted membership = %+v, %v", state, err)
	}

	// Granting the learner role to an admin creates a learner profile.
	if err := s.UpdateTenantMember(ctx, owner, adminMembership.ID, models.MembershipStatusActive,
		[]string{models.RoleAdmin, models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	state, err = s.GetConsoleMembership(ctx, adminScope)
	if err != nil || state.LearnerID == "" {
		t.Fatalf("admin learner profile = %+v, %v", state, err)
	}

	// Removing a member ends its console sessions and hides it from the list.
	credential := models.ConsoleSessionCredential{Token: "learner-session"}
	if err := s.CreateConsoleSession(ctx, credential, learnerScope, state.Version, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTenantMember(ctx, owner, learner.ID, models.MembershipStatusRevoked, []string{models.RoleLearner}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConsoleSession(ctx, credential); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("revoked member session = %v", err)
	}
	members, err := s.ListTenantMembers(ctx, owner)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	if _, err := s.GetConsoleMembership(ctx, learnerScope); err == nil {
		t.Fatal("revoked membership still loads")
	}

	learnerOnly := ConsolePrincipal(learnerScope, models.ConsoleMembership{Roles: []string{models.RoleLearner}, Version: 1})
	if _, err := s.ListTenantMembers(ctx, learnerOnly); err == nil {
		t.Fatal("a learner listed members")
	}
}

func TestSignupCreatesTenantOwnerAndRejectsReuse(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	if ok, _ := s.TenantSlugAvailable(ctx, "acme"); !ok {
		t.Fatal("free slug reported taken")
	}
	if ok, _ := s.TenantSlugAvailable(ctx, "Bad Slug"); ok {
		t.Fatal("invalid slug reported available")
	}
	if ok, _ := s.PlanIsActive(ctx, "plan_legacy"); !ok {
		t.Fatal("seeded plan inactive")
	}
	if ok, _ := s.PlanIsActive(ctx, "plan_missing"); ok {
		t.Fatal("unknown plan active")
	}
	scope := seedInstitutionOwner(t, s, "acme", "Owner@Acme.test")
	if ok, _ := s.TenantSlugAvailable(ctx, "acme"); ok {
		t.Fatal("used slug reported available")
	}
	credential := models.SignupCredential{Token: "signup-acme"}
	if _, err := s.GetPendingSignup(ctx, credential); err == nil {
		t.Fatal("consumed signup still pending")
	}
	if _, err := s.CompleteSignup(ctx, credential, "plan_legacy", "", "hash"); err == nil {
		t.Fatal("signup link reused")
	}
	membership, err := s.GetConsoleMembership(ctx, scope)
	if err != nil || !slices.Equal(membership.Roles, []string{models.RoleOwner}) || membership.Email != "owner@acme.test" {
		t.Fatalf("owner membership = %+v, %v", membership, err)
	}

	// Same slug from another signup fails without consuming the link.
	taken := models.SignupCredential{Token: "signup-taken"}
	if err := s.CreatePendingSignup(ctx, taken, "late@acme.test", "Acme bis", "acme", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteSignup(ctx, taken, "plan_legacy", "", "hash"); !errors.Is(err, storeport.ErrTenantSlugTaken) {
		t.Fatalf("taken slug error = %v", err)
	}
	if _, err := s.GetPendingSignup(ctx, taken); err != nil {
		t.Fatal("failed signup consumed its link")
	}

	// An existing account signs up a second institution with its own identity.
	second := models.SignupCredential{Token: "signup-second"}
	if err := s.CreatePendingSignup(ctx, second, "owner@acme.test", "Second", "second", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteSignup(ctx, second, "plan_legacy", "", "hash"); !errors.Is(err, storeport.ErrAccountExists) {
		t.Fatalf("duplicate account signup error = %v", err)
	}
	owned, err := s.CompleteSignup(ctx, second, "plan_legacy", scope.UserID, "")
	if err != nil {
		t.Fatal(err)
	}
	if owned.UserID != scope.UserID || owned.TenantID == scope.TenantID {
		t.Fatalf("second institution membership = %+v", owned)
	}
}

func TestUserPasswordResetRevokesEverySignIn(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	ownerScope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	owner, _ := verifiedOwner(t, s, ownerScope)
	session := models.ConsoleSessionCredential{Token: "owner-session"}
	if err := s.CreateConsoleSession(ctx, session, ownerScope, owner.TokenVersion, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if created, err := s.CreateUserPasswordReset(ctx, models.PasswordResetCredential{Token: "unknown"}, "nobody@acme.test"); err != nil || created {
		t.Fatalf("reset for an unknown address = %v, %v", created, err)
	}
	credential := models.PasswordResetCredential{Token: "reset-owner"}
	if created, err := s.CreateUserPasswordReset(ctx, credential, "Owner@Acme.test"); err != nil || !created {
		t.Fatalf("reset for the owner = %v, %v", created, err)
	}
	if !s.UserPasswordResetValid(ctx, credential) {
		t.Fatal("fresh reset link invalid")
	}
	if err := s.ResetUserPassword(ctx, credential, "new-hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetUserPassword(ctx, credential, "other-hash"); err == nil {
		t.Fatal("reset link reused")
	}
	user, err := s.GetLocalUserByEmail(ctx, "owner@acme.test")
	if err != nil || user.PasswordHash != "new-hash" {
		t.Fatalf("password after reset = %+v, %v", user, err)
	}
	if _, err := s.GetConsoleSession(ctx, session); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("console session survived the reset: %v", err)
	}
	if err := s.ValidatePrincipal(ctx, owner); err == nil {
		t.Fatal("pre-reset principal still valid")
	}
}

func TestInvitationOwnerRoleAndSingleUse(t *testing.T) {
	s := institutionTestStore(t)
	ctx := context.Background()
	ownerScope := seedInstitutionOwner(t, s, "acme", "owner@acme.test")
	owner, _ := verifiedOwner(t, s, ownerScope)
	_, raw, err := s.CreateTenantInvitation(ctx, owner, "admin@acme.test", []string{models.RoleAdmin}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	adminMembership, err := s.AcceptTenantInvitationWithNewUser(ctx, models.InvitationCredential{Token: raw}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := verifiedOwner(t, s, models.TenantScope{TenantID: owner.TenantID, UserID: adminMembership.UserID, MembershipID: adminMembership.ID})
	if _, _, err := s.CreateTenantInvitation(ctx, admin, "boss@acme.test", []string{models.RoleOwner}, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("an admin invited an owner")
	}
	if _, err := s.AcceptTenantInvitation(ctx, raw, adminMembership.UserID); err == nil {
		t.Fatal("an accepted invitation was accepted again")
	}
}
