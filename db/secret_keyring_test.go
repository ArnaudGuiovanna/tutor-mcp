package db

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func encodedSecretKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func testSecretKeyring(t *testing.T, entries, current string) *IntegrationSecretKeyring {
	t.Helper()
	keyring, err := NewIntegrationSecretKeyring(entries, current)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	return keyring
}

func TestIntegrationSecretEncryptedAtRestAndDecryptedAtUse(t *testing.T) {
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "k1:"+encodedSecretKey(1), "k1"))
	const secret = "https://discord.com/api/webhooks/123/super-secret-token"
	learner, err := s.CreateLearner(context.Background(), "encrypted@example.test", "hash", "learn", secret)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.root.QueryRow(rb(s, `SELECT webhook_url FROM learners WHERE id = ?`), learner.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == secret || strings.Contains(stored, "super-secret-token") || !strings.HasPrefix(stored, integrationSecretPrefix+"k1:") {
		t.Fatalf("secret not safely enveloped at rest: %q", stored)
	}
	got, err := s.GetWebhookDispatchURL(context.Background(), learner.ID)
	if err != nil || got != secret {
		t.Fatalf("decrypted dispatch credential=%q err=%v", got, err)
	}
}

func TestIntegrationSecretRotationSupportsOldKeyWithoutDowntime(t *testing.T) {
	s := setupTestDB(t)
	oldRing := testSecretKeyring(t, "old:"+encodedSecretKey(2), "old")
	s.SetIntegrationSecretKeyring(oldRing)
	const secret = "https://discord.com/api/webhooks/456/rotation-token"
	learner, err := s.CreateLearner(context.Background(), "rotate@example.test", "hash", "learn", secret)
	if err != nil {
		t.Fatal(err)
	}

	rotatingRing := testSecretKeyring(t,
		"old:"+encodedSecretKey(2)+",new:"+encodedSecretKey(3), "new",
	)
	s.SetIntegrationSecretKeyring(rotatingRing)
	// Old envelopes remain readable while the rotation is rolling out.
	if got, err := s.GetWebhookDispatchURL(context.Background(), learner.ID); err != nil || got != secret {
		t.Fatalf("read with retained old key: credential=%q err=%v", got, err)
	}
	rotated, err := s.RotateIntegrationSecrets(context.Background())
	if err != nil || rotated != 1 {
		t.Fatalf("rotate count=%d err=%v", rotated, err)
	}
	var stored string
	if err := s.root.QueryRow(rb(s, `SELECT webhook_url FROM learners WHERE id = ?`), learner.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, integrationSecretPrefix+"new:") {
		t.Fatalf("rotated envelope=%q", stored)
	}
	if got, err := s.GetWebhookDispatchURL(context.Background(), learner.ID); err != nil || got != secret {
		t.Fatalf("read after rotation: credential=%q err=%v", got, err)
	}
}

func TestIntegrationSecretRotationEncryptsValidatedLegacyRows(t *testing.T) {
	s := setupTestDB(t)
	const secret = "https://discord.com/api/webhooks/789/legacy-token"
	now := time.Now().UTC()
	if _, err := s.root.Exec(
		rb(s, `INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at, email_verified_at)
		 VALUES ('legacy-secret', 'legacy-secret@example.test', 'hash', 'learn', ?, ?, ?)`),
		secret, now, now,
	); err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "current:"+encodedSecretKey(4), "current"))
	rotated, err := s.RotateIntegrationSecrets(context.Background())
	if err != nil || rotated != 1 {
		t.Fatalf("legacy rotation count=%d err=%v", rotated, err)
	}
	if got, err := s.GetWebhookDispatchURL(context.Background(), "legacy-secret"); err != nil || got != secret {
		t.Fatalf("legacy read credential=%q err=%v", got, err)
	}
}

func TestIntegrationSecretWrongKeyFailsWithoutDisclosingSecret(t *testing.T) {
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "same-id:"+encodedSecretKey(5), "same-id"))
	const secret = "https://discord.com/api/webhooks/999/do-not-disclose"
	learner, err := s.CreateLearner(context.Background(), "wrong-key@example.test", "hash", "learn", secret)
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "same-id:"+encodedSecretKey(6), "same-id"))
	// General profile and scheduler-page reads never touch the credential, so a
	// key outage cannot poison authentication or the bounded audience scan.
	if _, err := s.GetLearnerByID(context.Background(), learner.ID); err != nil {
		t.Fatalf("profile read unexpectedly decrypted the credential: %v", err)
	}
	page, pageErr := s.ListWebhookDispatchTargetsPage(context.Background(), "", 10)
	if pageErr != nil || len(page) != 1 || page[0].LearnerID != learner.ID {
		t.Fatalf("credential-free scheduler page=%+v err=%v", page, pageErr)
	}
	_, err = s.GetWebhookDispatchURL(context.Background(), learner.ID)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "do-not-disclose") {
		t.Fatalf("wrong-key error=%v", err)
	}
}

func TestIntegrationSecretRotationAuthenticatesCurrentEnvelope(t *testing.T) {
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "current:"+encodedSecretKey(7), "current"))
	learner, err := s.CreateLearner(
		context.Background(), "tampered@example.test", "hash", "learn",
		"https://discord.com/api/webhooks/100/tamper-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.root.QueryRow(rb(s, `SELECT webhook_url FROM learners WHERE id = ?`), learner.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	alterAt := len(integrationSecretPrefix) + len("current:") + 5
	last := stored[alterAt]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	tampered := stored[:alterAt] + string(replacement) + stored[alterAt+1:]
	if _, err := s.root.Exec(rb(s, `UPDATE learners SET webhook_url = ? WHERE id = ?`), tampered, learner.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RotateIntegrationSecrets(context.Background()); err == nil {
		t.Fatal("rotation accepted a tampered current-key envelope")
	}
}

func TestIntegrationSecretRotationIsAtomicWhenAnyLegacyRowIsInvalid(t *testing.T) {
	s := setupTestDB(t)
	s.SetIntegrationSecretKeyring(testSecretKeyring(t, "old:"+encodedSecretKey(8), "old"))
	learner, err := s.CreateLearner(
		context.Background(), "atomic@example.test", "hash", "learn",
		"https://discord.com/api/webhooks/101/atomic-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.root.Exec(
		rb(s, `INSERT INTO learners
			(id, email, password_hash, objective, webhook_url, created_at, email_verified_at)
		 VALUES ('invalid-legacy-secret', 'invalid-legacy@example.test', 'hash', 'learn', 'not-a-webhook', ?, ?)`),
		now, now,
	); err != nil {
		t.Fatal(err)
	}
	s.SetIntegrationSecretKeyring(testSecretKeyring(t,
		"old:"+encodedSecretKey(8)+",new:"+encodedSecretKey(9), "new",
	))

	if _, err := s.RotateIntegrationSecrets(context.Background()); err == nil {
		t.Fatal("rotation accepted an invalid legacy secret")
	}
	var stored string
	if err := s.root.QueryRow(rb(s, `SELECT webhook_url FROM learners WHERE id = ?`), learner.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, integrationSecretPrefix+"old:") {
		t.Fatalf("valid row was partially rotated despite validation failure: %q", stored)
	}
}

func TestIntegrationSecretKeyringValidation(t *testing.T) {
	for _, tc := range []struct{ entries, current string }{
		{"", "k1"},
		{"bad id:" + encodedSecretKey(1), "bad id"},
		{"k1:not-base64", "k1"},
		{"k1:" + encodedSecretKey(1), "missing"},
	} {
		if _, err := NewIntegrationSecretKeyring(tc.entries, tc.current); err == nil {
			t.Fatalf("invalid keyring accepted: %+v", tc)
		}
	}
}
