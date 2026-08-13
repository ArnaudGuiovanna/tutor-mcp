// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func main() {
	mode := flag.String("mode", "export", "export or restore")
	tenantID := flag.String("tenant", "", "exact tenant ID (required for export)")
	archivePath := flag.String("archive", "", "absolute archive JSON path")
	reason := flag.String("reason", "", "operator-approved reason")
	requestID := flag.String("request-id", "", "operator request or change identifier")
	flag.Parse()
	if (*mode != "export" && *mode != "restore") || *archivePath == "" || (*archivePath != "-" && !filepath.IsAbs(*archivePath)) ||
		strings.TrimSpace(*reason) == "" || strings.TrimSpace(*requestID) == "" || (*mode == "export" && *tenantID == "") {
		fatal("usage: tutor-tenant-archive -mode export|restore -archive /absolute/path|- -reason TEXT -request-id ID [-tenant ID]")
	}
	if *mode == "restore" && os.Getenv("ALLOW_TENANT_LOGICAL_RESTORE") != "yes" {
		fatal("restore requires ALLOW_TENANT_LOGICAL_RESTORE=yes")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fatal("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	database, err := db.OpenPostgres(dsn, 2)
	if err != nil {
		fatal(err.Error())
	}
	defer database.Close()
	if err := db.VerifyPostgresSchemaCurrent(ctx, database); err != nil {
		fatal(err.Error())
	}
	store := db.NewStoreWithDialect(database, db.DialectPostgres)
	actor := models.ControlPlanePrincipal{
		ActorID: "tenant-archive-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: *reason, RequestID: *requestID,
	}
	if *mode == "export" {
		encoded, err := store.ExportPostgresTenantArchive(ctx, actor, *tenantID)
		if err != nil {
			fatal(err.Error())
		}
		if err := writeArchiveDestination(*archivePath, encoded, os.Stdout); err != nil {
			fatal(err.Error())
		}
		sum := sha256.Sum256(encoded)
		metadataOutput := io.Writer(os.Stdout)
		if *archivePath == "-" {
			metadataOutput = os.Stderr
		}
		writeJSONTo(metadataOutput, map[string]any{
			"mode": "export", "tenant_id": *tenantID, "archive": *archivePath,
			"bytes": len(encoded), "file_sha256": hex.EncodeToString(sum[:]),
		})
		return
	}
	encoded, err := readArchiveSource(*archivePath, os.Stdin)
	if err != nil {
		fatal(err.Error())
	}
	result, err := store.RestorePostgresTenantArchive(ctx, actor, encoded)
	if err != nil {
		fatal(err.Error())
	}
	writeJSON(result)
}

func writeArchiveExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create archive exclusively: %w", err)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	committed = true
	return nil
}

func writeArchiveDestination(path string, content []byte, output io.Writer) error {
	if path == "-" {
		_, err := output.Write(content)
		return err
	}
	return writeArchiveExclusive(path, content)
}

func readArchiveSource(path string, input io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(input, 1<<30))
	}
	return os.ReadFile(path)
}

func writeJSON(value any) {
	writeJSONTo(os.Stdout, value)
}

func writeJSONTo(output io.Writer, value any) {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { _, _ = fmt.Fprintln(os.Stderr, message); os.Exit(1) }
