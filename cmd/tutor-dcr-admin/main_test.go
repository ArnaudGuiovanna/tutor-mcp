// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
)

func dcrAdminEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func setupDCRAdminDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dcr-admin.db")
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDCRAdminCreateRotateRevokeAndAudit(t *testing.T) {
	path := setupDCRAdminDB(t)
	env := dcrAdminEnv(map[string]string{"DB_PATH": path, "DCR_ADMIN_ACTOR": "alice"})
	var preview bytes.Buffer
	if err := run([]string{
		"--action=create", "--token-id=old-token", "--label=old", "--max-registrations=2",
	}, &preview, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(preview.String(), "initial_access_token") {
		t.Fatal("read-only preview generated a credential")
	}
	database, err := db.OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM oauth_dcr_initial_access_tokens`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	if count != 0 {
		t.Fatalf("preview created %d token rows", count)
	}

	createOut := &bytes.Buffer{}
	if err := run([]string{
		"--action=create", "--token-id=old-token", "--label=old", "--max-registrations=2", "--apply",
	}, createOut, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	var created tokenMutationReport
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Applied || created.InitialAccessToken == "" || created.TokenID != "old-token" {
		t.Fatalf("create report=%+v", created)
	}
	database, err = db.OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var storedHash string
	if err := database.QueryRow(`SELECT token_hash FROM oauth_dcr_initial_access_tokens WHERE token_id = 'old-token'`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	if storedHash == created.InitialAccessToken || strings.Contains(storedHash, created.InitialAccessToken) {
		t.Fatal("raw initial access token was persisted")
	}

	rotateOut := &bytes.Buffer{}
	if err := run([]string{
		"--action=rotate", "--token-id=new-token", "--previous-token-id=old-token",
		"--label=new", "--max-registrations=3", "--expires-in=168h", "--apply",
	}, rotateOut, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	var rotated tokenMutationReport
	if err := json.Unmarshal(rotateOut.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if !rotated.Applied || rotated.InitialAccessToken == "" || rotated.PreviousTokenID != "old-token" {
		t.Fatalf("rotate report=%+v", rotated)
	}

	listOut := &bytes.Buffer{}
	if err := run([]string{"--action=list"}, listOut, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	var listed tokenListReport
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 2 || listed.Tokens[0].RevokedAt != nil || listed.Tokens[1].RevokedAt != nil {
		t.Fatalf("rotation did not leave overlap: %+v", listed.Tokens)
	}

	revokePreview := &bytes.Buffer{}
	if err := run([]string{
		"--action=revoke", "--token-id=old-token", "--reason=rotation-complete",
	}, revokePreview, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	var previewReport tokenMutationReport
	_ = json.Unmarshal(revokePreview.Bytes(), &previewReport)
	if previewReport.Applied {
		t.Fatal("revoke preview applied mutation")
	}
	if err := run([]string{
		"--action=revoke", "--token-id=old-token", "--reason=rotation-complete", "--apply",
	}, &bytes.Buffer{}, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}

	auditOut := &bytes.Buffer{}
	if err := run([]string{"--action=audit"}, auditOut, &bytes.Buffer{}, env); err != nil {
		t.Fatal(err)
	}
	var audit auditReport
	if err := json.Unmarshal(auditOut.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, event := range audit.Events {
		actions[event.Action] = true
	}
	for _, action := range []string{"token_created", "rotation_started", "token_revoked"} {
		if !actions[action] {
			t.Fatalf("audit action %q missing from %+v", action, audit.Events)
		}
	}
}

func TestDCRAdminRefusesMissingSQLiteTarget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	err := run([]string{"--action=list"}, &bytes.Buffer{}, &bytes.Buffer{}, dcrAdminEnv(map[string]string{"DB_PATH": missing}))
	if err == nil || !strings.Contains(err.Error(), "refusing to create") {
		t.Fatalf("missing target error=%v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("administration command created missing database: %v", statErr)
	}
}

func TestDCRAdminAuditRowsRemainAfterClientDeletion(t *testing.T) {
	path := setupDCRAdminDB(t)
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(database)
	if err := store.CreateOAuthClientWithSecretCappedTTL(
		context.Background(), "expired", "expired", `[]`, "", 10,
		time.Now().UTC().Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanupExpiredOAuthClients(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var clients, audits int
	if err := database.QueryRow(`SELECT COUNT(*) FROM oauth_clients`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM oauth_dcr_audit WHERE action = 'client_expired_deleted'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if clients != 0 || audits != 1 {
		t.Fatalf("clients=%d audits=%d, want 0/1", clients, audits)
	}
}
