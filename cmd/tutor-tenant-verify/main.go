// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

type checksumSnapshot struct {
	TenantID   string          `json:"tenant_id"`
	CapturedAt time.Time       `json:"captured_at"`
	Tables     json.RawMessage `json:"tables"`
	Objects    json.RawMessage `json:"objects"`
}

func main() {
	mode := flag.String("mode", "snapshot", "snapshot or verify")
	tenantID := flag.String("tenant", "", "exact tenant ID")
	expectedPath := flag.String("expected", "", "snapshot JSON path for verify mode")
	backupID := flag.String("backup-id", "", "operator backup identifier")
	flag.Parse()
	if *tenantID == "" || (*mode != "snapshot" && *mode != "verify") || (*mode == "verify" && (*expectedPath == "" || *backupID == "")) {
		fatal("usage: tutor-tenant-verify -mode snapshot|verify -tenant ID [-expected FILE -backup-id ID]")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fatal("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw, err := db.OpenPostgres(dsn, 2)
	if err != nil {
		fatal(err.Error())
	}
	defer raw.Close()
	if err := db.VerifyPostgresSchemaCurrent(ctx, raw); err != nil {
		fatal(err.Error())
	}
	store := db.NewStoreWithDialect(raw, db.DialectPostgres)
	actor := models.ControlPlanePrincipal{ActorID: "tenant-restore-verifier", Roles: []string{models.RolePlatformAdmin},
		Reason: "isolated tenant restore verification", RequestID: "restore-verify-" + time.Now().UTC().Format("20060102T150405Z")}
	if *mode == "snapshot" {
		tables, objects, err := store.ComputeTenantChecksums(ctx, actor, *tenantID)
		if err != nil {
			fatal(err.Error())
		}
		writeJSON(checksumSnapshot{TenantID: *tenantID, CapturedAt: time.Now().UTC(), Tables: json.RawMessage(tables), Objects: json.RawMessage(objects)})
		return
	}
	encoded, err := os.ReadFile(*expectedPath)
	if err != nil {
		fatal(err.Error())
	}
	var expected checksumSnapshot
	if err := json.Unmarshal(encoded, &expected); err != nil || expected.TenantID != *tenantID || !json.Valid(expected.Tables) || !json.Valid(expected.Objects) {
		fatal("expected snapshot is invalid or belongs to another tenant")
	}
	manifest, err := store.RequestTenantRestoreVerification(ctx, actor, *tenantID, *backupID, string(expected.Tables), string(expected.Objects))
	if err != nil {
		fatal(err.Error())
	}
	matched, err := store.VerifyTenantRestore(ctx, actor, *tenantID, manifest.ID)
	if err != nil {
		fatal(err.Error())
	}
	if !matched {
		fatal("tenant restore checksum mismatch")
	}
	writeJSON(map[string]any{"tenant_id": *tenantID, "backup_id": *backupID, "manifest_id": manifest.ID, "verified": true})
}

func writeJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { _, _ = fmt.Fprintln(os.Stderr, message); os.Exit(1) }
