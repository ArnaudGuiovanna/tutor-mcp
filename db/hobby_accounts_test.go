package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
)

func newProfileTestStore(t *testing.T, profile string) *Store {
	t.Helper()
	database, err := OpenDB(filepath.Join(t.TempDir(), "data", "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err = Migrate(database); err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)
	if err = s.EnsureInstallation(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocalIdentityConcurrentAndLearnerOnly(t *testing.T) {
	s := newProfileTestStore(t, "local")
	ctx := context.Background()
	ids := make(chan string, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			id, err := s.EnsureLocalIdentity(ctx)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		})
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("multiple local identities")
		}
	}
	principal, err := s.GetPrincipalForLearner(ctx, first, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	if len(principal.Roles) != 1 || principal.Roles[0] != models.RoleLearner {
		t.Fatalf("roles=%v", principal.Roles)
	}
	learner, err := s.GetLearnerByID(ctx, first)
	if err != nil || learner.EmailVerifiedAt != nil || learner.Email != "" || learner.IdentityMode != "local" {
		t.Fatalf("learner=%+v err=%v", learner, err)
	}
	if err := s.EnsureInstallation(ctx, "hobby"); err == nil {
		t.Fatal("converted a local installation")
	}
}

func TestHobbyInviteSingleUseExpiryAndNoEmail(t *testing.T) {
	s := newProfileTestStore(t, "hobby")
	ctx := context.Background()
	raw, err := s.CreateHobbyLink(ctx, "invite", "")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.root.QueryRow(`SELECT token_hash FROM hobby_account_links`).Scan(&stored); err != nil || stored == raw {
		t.Fatalf("stored raw link: %v", err)
	}
	if _, err := s.AcceptHobbyInvite(ctx, raw, "x", "$2dummy"); err == nil {
		t.Fatal("invalid name accepted")
	}
	if !s.HobbyLinkValid(ctx, raw, "invite") {
		t.Fatal("invalid form consumed invitation")
	}
	results := make(chan error, 8)
	for range 8 {
		go func() { _, err := s.AcceptHobbyInvite(ctx, raw, "ALICE", "$2dummy"); results <- err }()
	}
	wins := 0
	for range 8 {
		if <-results == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("invite winners=%d", wins)
	}
	user, err := s.GetUserByLoginName(ctx, "Alice")
	if err != nil || user.EmailVerifiedAt != nil || user.Email != "" || user.Status != "active" {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	if _, err := s.GetLocalUserByEmail(ctx, "identity:"+user.ID); err == nil {
		t.Fatal("non-email identity available via email lookup")
	}
	if _, err := s.root.Exec(`UPDATE users SET email_verified_at = CURRENT_TIMESTAMP WHERE id = ?`, user.ID); err == nil {
		t.Fatal("non-email mailbox marked verified")
	}
	if _, err := s.GetPrincipalForLearner(ctx, user.ID, []string{models.OAuthScopeLearner}); err != nil {
		t.Fatal(err)
	}
	expired, _ := s.CreateHobbyLink(ctx, "invite", "")
	if _, err := s.root.Exec(`UPDATE hobby_account_links SET expires_at = ? WHERE token_hash = ?`, time.Now().Add(-time.Minute), hobbyLinkHash(expired)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptHobbyInvite(ctx, expired, "bob", "$2dummy"); err == nil {
		t.Fatal("expired invitation accepted")
	}
}

func TestHobbyResetAndDisableRevokeEveryGrant(t *testing.T) {
	s := newProfileTestStore(t, "hobby")
	ctx := context.Background()
	raw, _ := s.CreateHobbyLink(ctx, "invite", "")
	id, err := s.AcceptHobbyInvite(ctx, raw, "alice", "$2old")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.GetPrincipalForLearner(ctx, id, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.CreateRefreshTokenForPrincipal(ctx, principal, "client", testOAuthResource, models.OAuthScopeLearner)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := s.CreateHobbyLink(ctx, "reset", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidatePrincipal(ctx, principal); err == nil {
		t.Fatal("old access principal survived reset issuance")
	}
	if _, err = s.RotateRefreshToken(ctx, rt.Token, "client", testOAuthResource); err == nil {
		t.Fatal("refresh survived reset")
	}
	if err = s.ResetHobbyPassword(ctx, reset, "$2new"); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetHobbyPassword(ctx, reset, "$2other"); err == nil {
		t.Fatal("replayed reset")
	}
	learner, err := s.GetLearnerByID(ctx, id)
	if err != nil || learner.EmailVerifiedAt != nil || learner.PasswordHash != "$2new" {
		t.Fatalf("reset learner=%+v err=%v", learner, err)
	}
	principal, err = s.GetPrincipalForLearner(ctx, id, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	reset, _ = s.CreateHobbyLink(ctx, "reset", "alice")
	if err = s.DisableHobbyUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidatePrincipal(ctx, principal); err == nil {
		t.Fatal("disabled principal accepted")
	}
	if err = s.ResetHobbyPassword(ctx, reset, "$2reactivate"); err == nil {
		t.Fatal("reset reactivated disabled account")
	}
}
