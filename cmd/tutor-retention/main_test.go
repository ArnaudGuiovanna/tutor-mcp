// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
)

func TestParseOptions_EnvironmentAndFlagOverride(t *testing.T) {
	getenv := mapEnv(map[string]string{
		"DB_DRIVER":                        "SQLITE",
		"DB_PATH":                          "/data/runtime.db",
		"TUTOR_MCP_RETENTION_WEBHOOK_DAYS": "45",
		"TUTOR_MCP_RETENTION_ASSESSMENT_PLAINTEXT_DAYS": "90",
		"TUTOR_MCP_RETENTION_IDEMPOTENCY_RESPONSE_DAYS": "75",
		"TUTOR_MCP_RETENTION_WEBHOOK_LIVE_DAYS":         "14",
		"TUTOR_MCP_RETENTION_ASSESSMENT_ABANDONED_DAYS": "21",
		"TUTOR_MCP_RETENTION_CONSOLIDATION_DAYS":        "180",
		"TUTOR_MCP_RETENTION_MEMORY_DAYS":               "365",
	})
	opts, err := parseOptions([]string{"--webhook-days=30", "--idempotency-response-days=60", "--snapshot-days=120"}, &bytes.Buffer{}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Driver != "sqlite" || opts.DBPath != "/data/runtime.db" {
		t.Fatalf("database options = %+v", opts)
	}
	if opts.Policy.WebhookTerminalDays != 30 ||
		opts.Policy.AssessmentPlaintextDays != 90 ||
		opts.Policy.IdempotencyResponseDays != 60 ||
		opts.Policy.PedagogicalSnapshotDays != 120 ||
		opts.Policy.WebhookLiveDays != 14 ||
		opts.Policy.AssessmentAbandonedDays != 21 ||
		opts.Policy.CompletedConsolidationDays != 180 ||
		opts.Policy.NarrativeMemoryDays != 365 {
		t.Fatalf("policy = %+v", opts.Policy)
	}
}

func TestRun_NarrativeMemoryDryRunThenApply(t *testing.T) {
	memoryRoot := t.TempDir()
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", memoryRoot)
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	path := filepath.Join(t.TempDir(), "runtime.db")
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	memoryFile := filepath.Join(memoryRoot, "learners", "L1", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(memoryFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memoryFile, []byte("old narrative"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -60)
	if err := os.Chtimes(memoryFile, old, old); err != nil {
		t.Fatal(err)
	}
	previousNow := timeNowUTC
	timeNowUTC = func() time.Time { return now }
	t.Cleanup(func() { timeNowUTC = previousNow })
	getenv := mapEnv(map[string]string{"DB_PATH": path})

	var dryOut bytes.Buffer
	if err := run([]string{"--memory-days=30"}, &dryOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	var dry db.RetentionReport
	if err := json.Unmarshal(dryOut.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if dry.NarrativeMemoryFiles.Eligible != 1 || dry.NarrativeMemoryFiles.Applied != 0 {
		t.Fatalf("memory dry-run report = %+v", dry.NarrativeMemoryFiles)
	}
	if _, err := os.Stat(memoryFile); err != nil {
		t.Fatalf("dry run removed memory: %v", err)
	}

	var applyOut bytes.Buffer
	if err := run([]string{"--memory-days=30", "--apply"}, &applyOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	var applied db.RetentionReport
	if err := json.Unmarshal(applyOut.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.NarrativeMemoryFiles.Applied != 1 {
		t.Fatalf("memory apply report = %+v", applied.NarrativeMemoryFiles)
	}
	if _, err := os.Stat(memoryFile); !os.IsNotExist(err) {
		t.Fatalf("old narrative remains after apply: %v", err)
	}
}

func TestParseOptions_RejectsInvalidEnvironment(t *testing.T) {
	_, err := parseOptions(nil, &bytes.Buffer{}, mapEnv(map[string]string{
		"TUTOR_MCP_RETENTION_EVENT_DAYS": "thirty",
	}))
	if err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("error = %v, want invalid integer", err)
	}
}

func TestRun_RefusesMissingSQLiteTarget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	err := run([]string{"--event-days=30"}, &bytes.Buffer{}, &bytes.Buffer{}, mapEnv(map[string]string{
		"DB_PATH": missing,
	}))
	if err == nil || !strings.Contains(err.Error(), "refusing to create") {
		t.Fatalf("error = %v, want missing-target refusal", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("maintenance command created missing database: %v", statErr)
	}
}

func TestRun_DryRunThenApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	if _, err := database.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, ?, ?, ?)`,
		"L1", "retention@test.invalid", "hash", "learn", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO scheduled_alerts (learner_id, alert_type, scheduled_at, created_at) VALUES (?, ?, ?, ?)`,
		"L1", "old", now.AddDate(0, 0, -60), now.AddDate(0, 0, -60),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	previousNow := timeNowUTC
	timeNowUTC = func() time.Time { return now }
	t.Cleanup(func() { timeNowUTC = previousNow })
	getenv := mapEnv(map[string]string{"DB_PATH": path})

	var dryOut bytes.Buffer
	if err := run([]string{"--event-days=30"}, &dryOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	var dry db.RetentionReport
	if err := json.Unmarshal(dryOut.Bytes(), &dry); err != nil {
		t.Fatalf("decode dry-run report: %v\n%s", err, dryOut.String())
	}
	if !dry.DryRun || dry.ScheduledAlertEvents.Eligible != 1 || dry.ScheduledAlertEvents.Applied != 0 {
		t.Fatalf("dry-run report = %+v", dry)
	}
	if got := countScheduledAlerts(t, path); got != 1 {
		t.Fatalf("dry-run changed rows: count=%d", got)
	}

	var applyOut bytes.Buffer
	if err := run([]string{"--event-days=30", "--apply"}, &applyOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var applied db.RetentionReport
	if err := json.Unmarshal(applyOut.Bytes(), &applied); err != nil {
		t.Fatalf("decode apply report: %v\n%s", err, applyOut.String())
	}
	if applied.DryRun || applied.ScheduledAlertEvents.Applied != 1 {
		t.Fatalf("apply report = %+v", applied)
	}
	if got := countScheduledAlerts(t, path); got != 0 {
		t.Fatalf("apply left rows: count=%d", got)
	}
}

func countScheduledAlerts(t *testing.T, path string) int {
	t.Helper()
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := db.NewStore(database)
	if err := store.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM scheduled_alerts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func mapEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}
