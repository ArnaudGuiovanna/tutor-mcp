// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Command tutor-retention previews or applies the explicitly configured data
// lifecycle policy. It is intentionally separate from the server process: no
// destructive maintenance runs merely because an application instance starts.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
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
	Driver          string
	DBPath          string
	MaxConns        int
	MemoryBackend   string
	Action          string
	Apply           bool
	Policy          db.RetentionPolicy
	JobID           string
	Actor           string
	BackupReference string
	BackupCreatedAt time.Time
	HoldID          string
	LearnerID       string
	Reason          string
	Limit           int
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := openStore(ctx, opts, getenv)
	if err != nil {
		return err
	}
	defer store.Close()
	if opts.MemoryBackend == "database" {
		memory.ConfigureNarrativeStore(store)
		defer memory.ConfigureNarrativeStore(nil)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")

	switch opts.Action {
	case "hold-list":
		holds, err := store.ListRetentionLegalHolds(ctx, false, opts.Limit)
		if err != nil {
			return err
		}
		return encoder.Encode(map[string]any{"action": opts.Action, "holds": holds})
	case "hold-create":
		hold := db.RetentionLegalHold{
			HoldID: opts.HoldID, LearnerID: opts.LearnerID,
			Reason: opts.Reason, CreatedBy: opts.Actor, CreatedAt: timeNowUTC(),
		}
		if opts.Apply {
			if err := store.CreateRetentionLegalHold(ctx, hold); err != nil {
				return err
			}
		}
		return encoder.Encode(map[string]any{"action": opts.Action, "applied": opts.Apply, "hold": hold})
	case "hold-release":
		applied := false
		if opts.Apply {
			applied, err = store.ReleaseRetentionLegalHold(ctx, opts.HoldID, opts.Actor, opts.Reason, timeNowUTC())
			if err != nil {
				return err
			}
		}
		return encoder.Encode(map[string]any{"action": opts.Action, "applied": applied, "hold_id": opts.HoldID})
	case "job-status":
		job, err := store.GetRetentionJob(ctx, opts.JobID)
		if err != nil {
			return err
		}
		return encoder.Encode(job)
	case "run":
	default:
		return fmt.Errorf("unsupported retention action %q", opts.Action)
	}

	if err := opts.Policy.Validate(); err != nil {
		return err
	}
	if !opts.Policy.Enabled() {
		return fmt.Errorf("all retention categories are disabled; set at least one *_DAYS value above zero")
	}
	asOf := timeNowUTC()
	if !opts.Apply {
		report, err := store.RunDataRetention(ctx, opts.Policy, asOf, false)
		if err != nil {
			return err
		}
		if opts.Policy.NarrativeMemoryDays > 0 {
			heldLearners, err := store.ActiveRetentionLegalHoldLearners(ctx)
			if err != nil {
				return err
			}
			memoryResult, err := memory.RunRetentionWithLegalHolds(
				asOf.AddDate(0, 0, -opts.Policy.NarrativeMemoryDays), false, heldLearners,
			)
			if err != nil {
				return err
			}
			report.NarrativeMemoryFiles = db.RetentionMetric{
				Eligible: memoryResult.Eligible, Applied: memoryResult.Applied, Held: memoryResult.Held,
			}
		}
		return encoder.Encode(report)
	}

	existing, existingErr := store.GetRetentionJob(ctx, opts.JobID)
	if existingErr == nil {
		asOf = existing.AsOf
		if opts.BackupReference == "" {
			opts.BackupReference = existing.BackupReference
			opts.BackupCreatedAt = existing.BackupCreatedAt
		}
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return existingErr
	}
	if opts.BackupReference == "" || opts.BackupCreatedAt.IsZero() {
		return fmt.Errorf("a new apply job requires backup-reference and backup-created-at")
	}
	job, err := store.CreateOrResumeRetentionJob(
		ctx, opts.JobID, opts.Policy, asOf, opts.BackupReference,
		opts.BackupCreatedAt, opts.Actor, timeNowUTC(),
	)
	if err != nil {
		return err
	}
	if job.Status == "completed" {
		return encoder.Encode(job)
	}
	ownerSuffix, err := randomOwnerSuffix()
	if err != nil {
		return err
	}
	owner := "retention:" + ownerSuffix
	job, err = store.ClaimRetentionJob(ctx, opts.JobID, owner, timeNowUTC())
	if err != nil {
		return err
	}

	if started, err := store.StartRetentionJobPhase(ctx, opts.JobID, owner, db.RetentionPhaseDatabase, timeNowUTC()); err != nil {
		return err
	} else if started {
		_, phaseErr := store.ApplyRetentionDatabaseJobPhase(ctx, opts.JobID, owner, opts.Policy, timeNowUTC())
		if phaseErr != nil {
			_ = store.FailRetentionJobPhase(ctx, opts.JobID, owner, db.RetentionPhaseDatabase, phaseErr, timeNowUTC())
			return phaseErr
		}
	}

	if started, err := store.StartRetentionJobPhase(ctx, opts.JobID, owner, db.RetentionPhaseNarrative, timeNowUTC()); err != nil {
		return err
	} else if started {
		heldLearners, phaseErr := store.ActiveRetentionLegalHoldLearners(ctx)
		if phaseErr != nil {
			_ = store.FailRetentionJobPhase(ctx, opts.JobID, owner, db.RetentionPhaseNarrative, phaseErr, timeNowUTC())
			return phaseErr
		}
		memoryResult, phaseErr := memory.RunRetentionWithLegalHolds(
			job.AsOf.AddDate(0, 0, -opts.Policy.NarrativeMemoryDays), true, heldLearners,
		)
		if phaseErr != nil {
			_ = store.FailRetentionJobPhase(ctx, opts.JobID, owner, db.RetentionPhaseNarrative, phaseErr, timeNowUTC())
			return phaseErr
		}
		phaseJSON, _ := json.Marshal(memoryResult)
		if err := store.CompleteRetentionJobPhase(
			ctx, opts.JobID, owner, db.RetentionPhaseNarrative,
			memoryResult.Eligible, memoryResult.Applied, memoryResult.Held,
			string(phaseJSON), timeNowUTC(),
		); err != nil {
			return err
		}
	}

	checkpointed, err := store.GetRetentionJob(ctx, opts.JobID)
	if err != nil {
		return err
	}
	finalJSON, _ := json.Marshal(checkpointed.Phases)
	if err := store.CompleteRetentionJob(ctx, opts.JobID, owner, string(finalJSON), timeNowUTC()); err != nil {
		return err
	}
	completed, err := store.GetRetentionJob(ctx, opts.JobID)
	if err != nil {
		return err
	}
	return encoder.Encode(completed)
}

func randomOwnerSuffix() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate retention lease owner: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
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
		Driver:          strings.ToLower(strings.TrimSpace(getenv("DB_DRIVER"))),
		DBPath:          strings.TrimSpace(getenv("DB_PATH")),
		MaxConns:        maxConns,
		Action:          "run",
		Actor:           strings.TrimSpace(getenv("RETENTION_ACTOR")),
		BackupReference: strings.TrimSpace(getenv("RETENTION_BACKUP_REFERENCE")),
		Limit:           100,
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
	if raw := strings.TrimSpace(getenv("TUTOR_MCP_MEMORY_BACKEND")); raw != "" {
		opts.MemoryBackend = strings.ToLower(raw)
	} else if opts.Driver == "postgres" {
		opts.MemoryBackend = "database"
	} else {
		opts.MemoryBackend = "local"
	}

	fs := flag.NewFlagSet("tutor-retention", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Driver, "db-driver", opts.Driver, "database driver: sqlite or postgres")
	fs.StringVar(&opts.DBPath, "db-path", opts.DBPath, "existing SQLite database path")
	fs.IntVar(&opts.MaxConns, "db-max-conns", opts.MaxConns, "PostgreSQL pool size")
	fs.StringVar(&opts.MemoryBackend, "memory-backend", opts.MemoryBackend, "narrative backend: local or database")
	fs.StringVar(&opts.Action, "action", opts.Action, "action: run, job-status, hold-list, hold-create, or hold-release")
	fs.BoolVar(&opts.Apply, "apply", false, "apply mutations; omitted means read-only dry-run")
	fs.StringVar(&opts.JobID, "job-id", "", "durable apply job ID or job-status target")
	fs.StringVar(&opts.Actor, "actor", opts.Actor, "operator identity recorded in the manifest/audit")
	fs.StringVar(&opts.BackupReference, "backup-reference", opts.BackupReference, "operator-verifiable backup ID/path/PITR marker")
	backupCreatedAt := strings.TrimSpace(getenv("RETENTION_BACKUP_CREATED_AT"))
	fs.StringVar(&backupCreatedAt, "backup-created-at", backupCreatedAt, "backup timestamp in RFC3339")
	fs.StringVar(&opts.HoldID, "hold-id", "", "legal-hold identifier")
	fs.StringVar(&opts.LearnerID, "learner", "", "learner protected by a legal hold")
	fs.StringVar(&opts.Reason, "reason", "", "legal-hold creation/release reason")
	fs.IntVar(&opts.Limit, "limit", opts.Limit, "maximum hold rows returned (1..1000)")
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
	opts.MemoryBackend = strings.ToLower(strings.TrimSpace(opts.MemoryBackend))
	opts.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	opts.JobID = strings.TrimSpace(opts.JobID)
	opts.Actor = strings.TrimSpace(opts.Actor)
	opts.BackupReference = strings.TrimSpace(opts.BackupReference)
	opts.HoldID = strings.TrimSpace(opts.HoldID)
	opts.LearnerID = strings.TrimSpace(opts.LearnerID)
	opts.Reason = strings.TrimSpace(opts.Reason)
	if opts.Driver != "sqlite" && opts.Driver != "postgres" {
		return options{}, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
	if opts.MaxConns <= 0 {
		return options{}, fmt.Errorf("db-max-conns must be greater than zero")
	}
	if opts.MemoryBackend != "local" && opts.MemoryBackend != "database" {
		return options{}, fmt.Errorf("memory-backend must be local or database")
	}
	if opts.Limit < 1 || opts.Limit > 1000 {
		return options{}, fmt.Errorf("limit must be between 1 and 1000")
	}
	if backupCreatedAt != "" {
		parsed, err := time.Parse(time.RFC3339, backupCreatedAt)
		if err != nil {
			return options{}, fmt.Errorf("backup-created-at must be RFC3339: %w", err)
		}
		opts.BackupCreatedAt = parsed.UTC()
	}
	switch opts.Action {
	case "run":
		if opts.HoldID != "" || opts.LearnerID != "" || opts.Reason != "" {
			return options{}, fmt.Errorf("run does not accept legal-hold flags")
		}
		if opts.Apply && (opts.JobID == "" || opts.Actor == "") {
			return options{}, fmt.Errorf("apply requires job-id and actor")
		}
	case "job-status":
		if opts.JobID == "" || opts.Apply {
			return options{}, fmt.Errorf("job-status requires job-id and is read-only")
		}
	case "hold-list":
		if opts.Apply || opts.HoldID != "" || opts.LearnerID != "" || opts.Reason != "" {
			return options{}, fmt.Errorf("hold-list is read-only and does not accept hold mutation flags")
		}
	case "hold-create":
		if opts.HoldID == "" || opts.LearnerID == "" || opts.Reason == "" || opts.Actor == "" {
			return options{}, fmt.Errorf("hold-create requires hold-id, learner, reason, and actor")
		}
	case "hold-release":
		if opts.HoldID == "" || opts.Reason == "" || opts.Actor == "" {
			return options{}, fmt.Errorf("hold-release requires hold-id, reason, and actor")
		}
		if opts.LearnerID != "" {
			return options{}, fmt.Errorf("hold-release does not accept learner")
		}
	default:
		return options{}, fmt.Errorf("unknown retention action %q", opts.Action)
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
