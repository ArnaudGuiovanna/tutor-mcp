// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func nativeTestKey(index byte) string {
	secret := make([]byte, 32)
	secret[31] = index
	_, pub := btcec.PrivKeyFromBytes(secret)
	return hex.EncodeToString(schnorr.SerializePubKey(pub))
}
func nativeTestActor() models.ControlPlanePrincipal {
	return models.ControlPlanePrincipal{ActorID: "native-test-operator", Roles: []string{models.RolePlatformAdmin}, Reason: "native identity acceptance", RequestID: "native-test"}
}
func nativeTestTenant(t *testing.T, s *Store, slug string) string {
	t.Helper()
	tenant, err := s.ProvisionTenant(t.Context(), nativeTestActor(), slug, slug, "test", "plan_legacy")
	if err != nil {
		t.Fatal(err)
	}
	return tenant.ID
}
func nativeTestMember(t *testing.T, s *Store, tenant string, index byte, roles []string) *models.NativeIdentity {
	t.Helper()
	n, err := s.ProvisionNativeMember(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: tenant, DisplayName: "Élève test", PublicKey: nativeTestKey(index), RecoveryPublicKey: nativeTestKey(index + 1)}, roles)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func nativeTestSession(n *models.NativeIdentity, id string, scopes []string) *models.NativeSession {
	now := time.Now().UTC()
	return &models.NativeSession{ID: id, IdentityID: n.ID, TenantID: n.TenantID, UserID: n.UserID, MembershipID: n.MembershipID, LearnerID: n.LearnerID, KeyVersion: n.Version, Scopes: scopes, CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
}
func nativeTestChallenge(n *models.NativeIdentity, label string) *models.NativeChallenge {
	now := time.Now().UTC()
	sum := sha256.Sum256([]byte(label))
	payload := sha256.Sum256([]byte("{}"))
	return &models.NativeChallenge{TokenHash: hex.EncodeToString(sum[:]), IdentityID: n.ID, TenantID: n.TenantID, PublicKey: n.PublicKey, Action: "session", Scopes: []string{models.OAuthScopeLearnerRead}, KeyVersion: n.Version, URL: "https://institution.test/api/native/session", Method: "POST", PayloadHash: hex.EncodeToString(payload[:]), CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
}

func TestNativeOwnerBootstrapHasNoEmailPasswordOrLearner(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-owner")
	request := models.NativeOwnerProvision{TenantID: tenant, DisplayName: "Direction", PublicKey: nativeTestKey(1), RecoveryPublicKey: nativeTestKey(2)}
	if _, err := s.BootstrapNativeOwner(t.Context(), models.ControlPlanePrincipal{}, request); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("nonoperator=%v", err)
	}
	n, err := s.BootstrapNativeOwner(t.Context(), nativeTestActor(), request)
	if err != nil {
		t.Fatal(err)
	}
	if n.LearnerID != "" {
		t.Fatal("owner has a fabricated learner")
	}
	if _, err = s.BootstrapNativeOwner(t.Context(), nativeTestActor(), request); !errors.Is(err, storeport.ErrAlreadyMember) {
		t.Fatalf("second owner=%v", err)
	}
	var email, normalized, password, login sql.NullString
	var mode string
	if err = s.queryRow(t.Context(), `SELECT email,normalized_email,password_hash,login_name,identity_mode FROM users WHERE id=?`, n.UserID).Scan(&email, &normalized, &password, &login, &mode); err != nil {
		t.Fatal(err)
	}
	if email.Valid || normalized.Valid || password.Valid || login.Valid || mode != "pubkey" {
		t.Fatalf("native credentials not NULL: email=%v normalized=%v password=%v login=%v mode=%s", email.Valid, normalized.Valid, password.Valid, login.Valid, mode)
	}
	session := nativeTestSession(n, "owner-session", []string{models.OAuthScopeFormationWrite})
	principal, err := s.CreateNativeSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if !principal.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: tenant}) {
		t.Fatal("owner cannot author formations")
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, principal, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	staff := nativeTestMember(t, s, tenant, 3, []string{models.RoleTrainer})
	if staff.LearnerID != "" {
		t.Fatal("staff has a fabricated learner")
	}
}

func TestNativeLearnerSessionGrantRevocationAndTenantIsolation(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-learner")
	otherTenant := nativeTestTenant(t, s, "native-other")
	n := nativeTestMember(t, s, tenant, 5, []string{models.RoleLearner})
	if n.LearnerID == "" {
		t.Fatal("explicit learner has no learner")
	}
	learner, err := s.GetLearnerByID(t.Context(), n.LearnerID)
	if err != nil || learner.IdentityMode != "pubkey" || learner.Email != "" || learner.PasswordHash != "" {
		t.Fatalf("learner=%#v err=%v", learner, err)
	}
	session := nativeTestSession(n, "learner-session", []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite})
	p, err := s.CreateNativeSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	bad := *session
	bad.ID = "escalation"
	bad.Scopes = []string{models.OAuthScopeFormationWrite}
	if _, err = s.CreateNativeSession(t.Context(), &bad); !errors.Is(err, storeport.ErrInvalidOAuthScope) {
		t.Fatalf("scope escalation=%v", err)
	}
	bad = *session
	bad.ID = "cross-tenant"
	bad.TenantID = otherTenant
	if _, err = s.CreateNativeSession(t.Context(), &bad); err == nil {
		t.Fatal("cross-tenant grant accepted")
	}
	forged := p
	forged.Scopes = []string{models.OAuthScopeLearnerRead}
	if err = s.ValidateNativeSession(t.Context(), session.ID, forged, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("narrowed JWT differs from durable grant=%v", err)
	}
	if _, err = s.GetNativeIdentity(t.Context(), otherTenant, n.PublicKey, "session"); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("unrelated tenant lookup=%v", err)
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, p, session.ExpiresAt); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("expired session=%v", err)
	}
	if err = s.RevokeNativeSession(t.Context(), n.ID, session.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, p, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("revoked session=%v", err)
	}
	session.ID = "suspended-session"
	p, err = s.CreateNativeSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetMembershipAuthorization(t.Context(), p.TenantScope(), models.MembershipStatusSuspended, p.Roles); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateNativeSession(t.Context(), session.ID, p, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("suspended member=%v", err)
	}
}

func TestNativeChallengeSingleWinnerExpiryBoundsAndRecoveryPurpose(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-challenge")
	n := nativeTestMember(t, s, tenant, 7, []string{models.RoleLearner})
	c := nativeTestChallenge(n, "race")
	if err := s.CreateNativeChallenge(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ConsumeNativeChallenge(context.Background(), c.TokenHash, time.Now().UTC())
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("challenge winners=%d", successes)
	}
	expired := nativeTestChallenge(n, "expired")
	expired.CreatedAt = time.Now().UTC().Add(-3 * time.Minute)
	expired.ExpiresAt = expired.CreatedAt.Add(2 * time.Minute)
	if err := s.CreateNativeChallenge(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeNativeChallenge(t.Context(), expired.TokenHash, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
		t.Fatalf("expired challenge=%v", err)
	}
	oversized := nativeTestChallenge(n, "long")
	oversized.ExpiresAt = oversized.CreatedAt.Add(2*time.Minute + time.Nanosecond)
	if err := s.CreateNativeChallenge(t.Context(), oversized); !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
		t.Fatalf("long challenge=%v", err)
	}
	recover := nativeTestChallenge(n, "recovery")
	recover.Action = "recover"
	if err := s.CreateNativeChallenge(t.Context(), recover); !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
		t.Fatalf("recovery with identity key=%v", err)
	}
	recover.PublicKey = n.RecoveryPublicKey
	if err := s.CreateNativeChallenge(t.Context(), recover); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRotationRevokesAllTenantsAndReservesRetiredKeys(t *testing.T) {
	s := setupTestDB(t)
	a := nativeTestTenant(t, s, "native-rotate-a")
	b := nativeTestTenant(t, s, "native-rotate-b")
	n := nativeTestMember(t, s, a, 9, []string{models.RoleLearner})
	nB, err := s.ProvisionNativeMember(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: b, DisplayName: "Même personne", PublicKey: n.PublicKey, RecoveryPublicKey: n.RecoveryPublicKey}, []string{models.RoleLearner})
	if err != nil || nB.UserID != n.UserID || nB.ID != n.ID {
		t.Fatalf("shared identity=%#v err=%v", nB, err)
	}
	sessionA := nativeTestSession(n, "rotation-a", []string{models.OAuthScopeLearnerRead})
	pA, err := s.CreateNativeSession(t.Context(), sessionA)
	if err != nil {
		t.Fatal(err)
	}
	sessionB := nativeTestSession(nB, "rotation-b", []string{models.OAuthScopeLearnerRead})
	pB, err := s.CreateNativeSession(t.Context(), sessionB)
	if err != nil {
		t.Fatal(err)
	}
	c := nativeTestChallenge(n, "stale-challenge")
	if err = s.CreateNativeChallenge(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotateNativeIdentity(t.Context(), n.ID, n.Version, nativeTestKey(11), nativeTestKey(12), time.Now().UTC())
	if err != nil || rotated.Version != n.Version+1 {
		t.Fatalf("rotation=%#v err=%v", rotated, err)
	}
	for _, v := range []struct {
		id string
		p  models.Principal
	}{{sessionA.ID, pA}, {sessionB.ID, pB}} {
		if err = s.ValidateNativeSession(t.Context(), v.id, v.p, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeSession) {
			t.Fatalf("rotated grant=%v", err)
		}
		if err = s.ValidatePrincipal(t.Context(), v.p); !errors.Is(err, storeport.ErrInvalidPrincipal) {
			t.Fatalf("rotated principal=%v", err)
		}
	}
	if _, err = s.ConsumeNativeChallenge(t.Context(), c.TokenHash, time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
		t.Fatalf("rotated challenge=%v", err)
	}
	if _, err = s.GetNativeIdentity(t.Context(), a, n.PublicKey, "session"); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("old identity still active=%v", err)
	}
	if _, err = s.GetNativeIdentity(t.Context(), a, rotated.RecoveryPublicKey, "recover"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RotateNativeIdentity(t.Context(), n.ID, n.Version, nativeTestKey(13), nativeTestKey(14), time.Now().UTC()); !errors.Is(err, storeport.ErrInvalidNativeIdentity) {
		t.Fatalf("stale rotation=%v", err)
	}
	if _, err = s.ProvisionNativeMember(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: b, DisplayName: "Key reuse", PublicKey: n.PublicKey, RecoveryPublicKey: nativeTestKey(15)}, []string{models.RoleLearner}); !errors.Is(err, storeport.ErrNativeKeyConflict) {
		t.Fatalf("retired key reused=%v", err)
	}
}

func TestNormalizeNativePublicKeyNIP19Vectors(t *testing.T) {
	const key = "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"
	const npub = "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"
	for _, value := range []string{key, npub, strings.ToUpper(npub)} {
		got, err := NormalizeNativePublicKey(value)
		if err != nil || got != key {
			t.Fatalf("NIP19 public vector=%s err=%v", got, err)
		}
	}
	for _, value := range []string{"nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5", npub[:62] + "q", "nPub" + npub[4:], strings.Repeat("f", 64), "", strings.Repeat("0", 64)} {
		if _, err := NormalizeNativePublicKey(value); err == nil {
			t.Fatalf("invalid key accepted: %s", value)
		}
	}
}

func TestNativeLimitsBoundChallengesAndSessions(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "native-limits")
	n := nativeTestMember(t, s, tenant, 17, []string{models.RoleLearner})
	for i := range 16 {
		if err := s.CreateNativeChallenge(t.Context(), nativeTestChallenge(n, fmt.Sprintf("challenge-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateNativeChallenge(t.Context(), nativeTestChallenge(n, "excess")); !errors.Is(err, storeport.ErrInvalidNativeChallenge) {
		t.Fatalf("challenge cap=%v", err)
	}
	for i := range 32 {
		if _, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, fmt.Sprintf("session-%d", i), []string{models.OAuthScopeLearnerRead})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, "excess", []string{models.OAuthScopeLearnerRead})); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("session cap=%v", err)
	}
	long := nativeTestSession(n, "too-long", []string{models.OAuthScopeLearnerRead})
	long.ExpiresAt = long.CreatedAt.Add(31 * time.Minute)
	if _, err := s.CreateNativeSession(t.Context(), long); !errors.Is(err, storeport.ErrInvalidNativeSession) {
		t.Fatalf("long session=%v", err)
	}
}

func TestNativeBindPreservesLearningAndRevokesLegacyAuthentication(t *testing.T) {
	s := setupTestDB(t)
	old, err := s.GetPrincipalForLearner(t.Context(), "L1", []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateAuthCodeForPrincipal(t.Context(), "native-old-code", old, "challenge", "S256", "client", "https://client.test/cb", testOAuthResource, models.OAuthScopeLearner, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetLearnerByID(t.Context(), "L1")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.BindNativeIdentity(t.Context(), nativeTestActor(), old.UserID, models.NativeOwnerProvision{TenantID: old.TenantID, DisplayName: "Migration", PublicKey: nativeTestKey(19), RecoveryPublicKey: nativeTestKey(20)})
	if err != nil {
		t.Fatal(err)
	}
	if n.LearnerID != old.LearnerID || n.UserID != old.UserID || n.MembershipID != old.MembershipID {
		t.Fatal("migration replaced existing identity tuple")
	}
	after, err := s.GetLearnerByID(t.Context(), "L1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Objective != before.Objective || after.ProfileJSON != before.ProfileJSON || after.Email != "" || after.PasswordHash != "" || after.IdentityMode != "pubkey" {
		t.Fatalf("migration lost learning profile or retained legacy auth: %#v", after)
	}
	if err = s.ValidatePrincipal(t.Context(), old); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("legacy JWT survived migration=%v", err)
	}
	if _, err = s.GetAuthCode(t.Context(), "native-old-code", "client"); err == nil {
		t.Fatal("legacy authorization survived migration")
	}
	if _, err = s.GetLocalUserByEmail(t.Context(), before.Email); !errors.Is(err, storeport.ErrNotFound) {
		t.Fatalf("email auth survived=%v", err)
	}
	if _, err = s.CreateNativeSession(t.Context(), nativeTestSession(n, "migrated-session", []string{models.OAuthScopeLearnerRead})); err != nil {
		t.Fatal(err)
	}
}
