// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Command tutor-webhook-admin lists and resolves outbound webhook attempts
// whose delivery outcome is uncertain. Resolution is deliberately separate
// from the server: delivery_unknown rows are never retried automatically.
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
	"tutor-mcp/models"
)

type options struct {
	Driver   string
	DBPath   string
	MaxConns int
	Action   string
	Learner  string
	Limit    int
	ID       int64
	EventID  string
	Outcome  string
	Apply    bool
}

type safeDelivery struct {
	ID                int64      `json:"id"`
	EventID           string     `json:"event_id"`
	LearnerID         string     `json:"learner_id"`
	Kind              string     `json:"kind"`
	DomainID          string     `json:"domain_id,omitempty"`
	Status            string     `json:"status"`
	DispatchStartedAt *time.Time `json:"dispatch_started_at,omitempty"`
	AttemptCount      int        `json:"attempt_count"`
	MaxAttempts       int        `json:"max_attempts"`
	LastError         string     `json:"last_error"`
}

type listReport struct {
	Action     string         `json:"action"`
	Deliveries []safeDelivery `json:"deliveries"`
}

type resolutionReport struct {
	Action   string       `json:"action"`
	Applied  bool         `json:"applied"`
	Outcome  string       `json:"outcome"`
	Delivery safeDelivery `json:"delivery"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "tutor-webhook-admin:", err)
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

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if opts.Action == "list" {
		items, err := store.GetWebhookDeliveryUnknown(ctx, opts.Learner, opts.Limit)
		if err != nil {
			return err
		}
		deliveries := make([]safeDelivery, 0, len(items))
		for _, item := range items {
			deliveries = append(deliveries, safeWebhookDelivery(item))
		}
		return encoder.Encode(listReport{Action: "list", Deliveries: deliveries})
	}

	item, err := findUnknownDelivery(ctx, store, opts.Learner, opts.ID, opts.EventID)
	if err != nil {
		return err
	}
	report := resolutionReport{
		Action: "resolve", Applied: opts.Apply, Outcome: opts.Outcome,
		Delivery: safeWebhookDelivery(item),
	}
	if !opts.Apply {
		return encoder.Encode(report)
	}
	if err := store.ResolveWebhookDeliveryUnknown(
		ctx, opts.ID, opts.Learner, opts.Outcome == "delivered", time.Now().UTC(),
	); err != nil {
		return err
	}
	return encoder.Encode(report)
}

func safeWebhookDelivery(item *models.WebhookQueueItem) safeDelivery {
	return safeDelivery{
		ID: item.ID, EventID: item.EventID, LearnerID: item.LearnerID,
		Kind: item.Kind, DomainID: item.DomainID, Status: item.Status,
		DispatchStartedAt: item.DispatchStartedAt, AttemptCount: item.AttemptCount,
		MaxAttempts: item.MaxAttempts, LastError: item.LastError,
	}
}

type webhookAdminStore interface {
	GetWebhookDeliveryUnknown(context.Context, string, int) ([]*models.WebhookQueueItem, error)
	ResolveWebhookDeliveryUnknown(context.Context, int64, string, bool, time.Time) error
}

func findUnknownDelivery(ctx context.Context, store webhookAdminStore, learnerID string, id int64, eventID string) (*models.WebhookQueueItem, error) {
	items, err := store.GetWebhookDeliveryUnknown(ctx, learnerID, 1000)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if item.EventID != eventID {
			return nil, fmt.Errorf("event-id does not match quarantined delivery %d", id)
		}
		return item, nil
	}
	return nil, fmt.Errorf("quarantined delivery %d not found for learner", id)
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	maxConns, err := envInt(getenv, "DB_MAX_CONNS", 10)
	if err != nil {
		return options{}, err
	}
	opts := options{
		Driver: strings.ToLower(strings.TrimSpace(getenv("DB_DRIVER"))),
		DBPath: strings.TrimSpace(getenv("DB_PATH")), MaxConns: maxConns,
		Action: "list", Limit: 100,
	}
	if opts.Driver == "" {
		opts.Driver = "sqlite"
	}
	if opts.DBPath == "" {
		opts.DBPath = "./data/runtime.db"
	}
	fs := flag.NewFlagSet("tutor-webhook-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Driver, "db-driver", opts.Driver, "database driver: sqlite or postgres")
	fs.StringVar(&opts.DBPath, "db-path", opts.DBPath, "existing SQLite database path")
	fs.IntVar(&opts.MaxConns, "db-max-conns", opts.MaxConns, "PostgreSQL pool size")
	fs.StringVar(&opts.Action, "action", opts.Action, "action: list or resolve")
	fs.StringVar(&opts.Learner, "learner", "", "owning learner ID")
	fs.IntVar(&opts.Limit, "limit", opts.Limit, "maximum rows returned by list (1..1000)")
	fs.Int64Var(&opts.ID, "id", 0, "queue row ID to resolve")
	fs.StringVar(&opts.EventID, "event-id", "", "stable event ID confirmation for resolve")
	fs.StringVar(&opts.Outcome, "outcome", "", "operator finding: delivered or not-delivered")
	fs.BoolVar(&opts.Apply, "apply", false, "apply the resolution; omitted means read-only preview")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	opts.Driver = strings.ToLower(strings.TrimSpace(opts.Driver))
	opts.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	opts.Learner = strings.TrimSpace(opts.Learner)
	opts.EventID = strings.TrimSpace(opts.EventID)
	opts.Outcome = strings.ToLower(strings.TrimSpace(opts.Outcome))
	if opts.Driver != "sqlite" && opts.Driver != "postgres" {
		return options{}, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
	if opts.MaxConns <= 0 {
		return options{}, fmt.Errorf("db-max-conns must be greater than zero")
	}
	if opts.Action != "list" && opts.Action != "resolve" {
		return options{}, fmt.Errorf("action must be list or resolve")
	}
	if opts.Learner == "" {
		return options{}, fmt.Errorf("learner is required")
	}
	if opts.Limit < 1 || opts.Limit > 1000 {
		return options{}, fmt.Errorf("limit must be between 1 and 1000")
	}
	if opts.Action == "list" {
		if opts.Apply || opts.ID != 0 || opts.EventID != "" || opts.Outcome != "" {
			return options{}, fmt.Errorf("list does not accept resolution flags")
		}
		return opts, nil
	}
	if opts.ID <= 0 || opts.EventID == "" {
		return options{}, fmt.Errorf("resolve requires positive id and event-id")
	}
	if opts.Outcome != "delivered" && opts.Outcome != "not-delivered" {
		return options{}, fmt.Errorf("resolve outcome must be delivered or not-delivered")
	}
	return opts, nil
}

func openStore(ctx context.Context, opts options, getenv func(string) string) (*db.Store, error) {
	switch opts.Driver {
	case "sqlite":
		info, err := os.Stat(opts.DBPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("SQLite database %q does not exist; refusing to create an empty administration target", opts.DBPath)
			}
			return nil, fmt.Errorf("stat SQLite database: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("SQLite database path %q is a directory", opts.DBPath)
		}
		var database *sql.DB
		if opts.Action == "resolve" && opts.Apply {
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
