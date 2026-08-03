// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Command tutor-retention previews or applies the explicitly configured data
// lifecycle policy. It is intentionally separate from the server process: no
// destructive maintenance runs merely because an application instance starts.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
)

type options struct {
	Driver   string
	DBPath   string
	MaxConns int
	Apply    bool
	Policy   db.RetentionPolicy
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "tutor-retention:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	opts, err := parseOptions(args, stderr, getenv)
	if err != nil {
		return err
	}
	if err := opts.Policy.Validate(); err != nil {
		return err
	}
	if !opts.Policy.Enabled() {
		return fmt.Errorf("all retention categories are disabled; set at least one *_DAYS value above zero")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openStore(ctx, opts, getenv)
	if err != nil {
		return err
	}
	defer store.Close()

	report, err := store.RunDataRetention(ctx, opts.Policy, timeNowUTC(), opts.Apply)
	if err != nil {
		return err
	}
	if opts.Policy.NarrativeMemoryDays > 0 {
		memoryResult, memoryErr := memory.RunRetention(
			timeNowUTC().AddDate(0, 0, -opts.Policy.NarrativeMemoryDays), opts.Apply,
		)
		if memoryErr != nil {
			return memoryErr
		}
		report.NarrativeMemoryFiles = db.RetentionMetric{
			Eligible: memoryResult.Eligible,
			Applied:  memoryResult.Applied,
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// timeNowUTC is a seam for deterministic command tests without allowing an
// operator-supplied future timestamp to widen an apply run accidentally.
var timeNowUTC = func() time.Time { return time.Now().UTC() }

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	webhookDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_WEBHOOK_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	assessmentDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_ASSESSMENT_PLAINTEXT_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	abandonedAssessmentDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_ASSESSMENT_ABANDONED_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	webhookLiveDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_WEBHOOK_LIVE_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	idempotencyResponseDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_IDEMPOTENCY_RESPONSE_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	snapshotDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_SNAPSHOT_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	eventDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_EVENT_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	consolidationDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_CONSOLIDATION_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	memoryDays, err := envInt(getenv, "TUTOR_MCP_RETENTION_MEMORY_DAYS", 0)
	if err != nil {
		return options{}, err
	}
	maxConns, err := envInt(getenv, "DB_MAX_CONNS", 10)
	if err != nil {
		return options{}, err
	}

	opts := options{
		Driver:   strings.ToLower(strings.TrimSpace(getenv("DB_DRIVER"))),
		DBPath:   strings.TrimSpace(getenv("DB_PATH")),
		MaxConns: maxConns,
		Policy: db.RetentionPolicy{
			WebhookTerminalDays:        webhookDays,
			WebhookLiveDays:            webhookLiveDays,
			AssessmentPlaintextDays:    assessmentDays,
			AssessmentAbandonedDays:    abandonedAssessmentDays,
			IdempotencyResponseDays:    idempotencyResponseDays,
			PedagogicalSnapshotDays:    snapshotDays,
			OperationalEventLogDays:    eventDays,
			CompletedConsolidationDays: consolidationDays,
			NarrativeMemoryDays:        memoryDays,
		},
	}
	if opts.Driver == "" {
		opts.Driver = "sqlite"
	}
	if opts.DBPath == "" {
		opts.DBPath = "./data/runtime.db"
	}

	fs := flag.NewFlagSet("tutor-retention", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Driver, "db-driver", opts.Driver, "database driver: sqlite or postgres")
	fs.StringVar(&opts.DBPath, "db-path", opts.DBPath, "existing SQLite database path")
	fs.IntVar(&opts.MaxConns, "db-max-conns", opts.MaxConns, "PostgreSQL pool size")
	fs.BoolVar(&opts.Apply, "apply", false, "apply mutations; omitted means read-only dry-run")
	fs.IntVar(&opts.Policy.WebhookTerminalDays, "webhook-days", webhookDays, "delete terminal webhook queue rows older than N days")
	fs.IntVar(&opts.Policy.WebhookLiveDays, "webhook-live-days", webhookLiveDays, "terminalize pending/processing webhook rows older than N days")
	fs.IntVar(&opts.Policy.AssessmentPlaintextDays, "assessment-plaintext-days", assessmentDays, "redact safely hashed terminal assessment plaintext older than N days")
	fs.IntVar(&opts.Policy.AssessmentAbandonedDays, "assessment-abandoned-days", abandonedAssessmentDays, "cancel stale prepared/submitted assessments and redact safely hashed plaintext")
	fs.IntVar(&opts.Policy.IdempotencyResponseDays, "idempotency-response-days", idempotencyResponseDays, "redact completed idempotency response plaintext older than N days without releasing the key")
	fs.IntVar(&opts.Policy.PedagogicalSnapshotDays, "snapshot-days", snapshotDays, "delete pedagogical snapshots older than N days")
	fs.IntVar(&opts.Policy.OperationalEventLogDays, "event-days", eventDays, "delete notification event logs older than N days")
	fs.IntVar(&opts.Policy.CompletedConsolidationDays, "consolidation-days", consolidationDays, "delete completed consolidation markers older than N days")
	fs.IntVar(&opts.Policy.NarrativeMemoryDays, "memory-days", memoryDays, "delete narrative Markdown files older than N days")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	opts.Driver = strings.ToLower(strings.TrimSpace(opts.Driver))
	if opts.Driver != "sqlite" && opts.Driver != "postgres" {
		return options{}, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
	if opts.MaxConns <= 0 {
		return options{}, fmt.Errorf("db-max-conns must be greater than zero")
	}
	return opts, nil
}

func openStore(ctx context.Context, opts options, getenv func(string) string) (*db.Store, error) {
	switch opts.Driver {
	case "sqlite":
		info, err := os.Stat(opts.DBPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("SQLite database %q does not exist; refusing to create an empty maintenance target", opts.DBPath)
			}
			return nil, fmt.Errorf("stat SQLite database: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("SQLite database path %q is a directory", opts.DBPath)
		}
		var database *sql.DB
		if opts.Apply {
			database, err = db.OpenDB(opts.DBPath)
		} else {
			database, err = db.OpenDBReadOnly(opts.DBPath)
		}
		if err != nil {
			return nil, err
		}
		store := db.NewStore(database)
		if err := store.Ping(ctx); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("ping SQLite database: %w", err)
		}
		return store, nil
	case "postgres":
		dsn := strings.TrimSpace(getenv("DATABASE_URL"))
		if dsn == "" {
			return nil, fmt.Errorf("DB_DRIVER=postgres requires DATABASE_URL")
		}
		database, err := db.OpenPostgres(dsn, opts.MaxConns)
		if err != nil {
			return nil, err
		}
		store := db.NewStoreWithDialect(database, db.DialectPostgres)
		if err := store.Ping(ctx); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("ping PostgreSQL database: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
}

func envInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return value, nil
}
