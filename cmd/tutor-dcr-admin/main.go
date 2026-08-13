// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Command tutor-dcr-admin manages hashed OAuth Dynamic Client Registration
// Initial Access Tokens. Newly generated raw tokens are emitted once and are
// never accepted as command-line input or persisted.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"tutor-mcp/models"
)

type options struct {
	Driver           string
	DBPath           string
	MaxConns         int
	Action           string
	Limit            int
	TokenID          string
	PreviousTokenID  string
	Label            string
	Actor            string
	Reason           string
	MaxRegistrations int
	ExpiresIn        time.Duration
	Apply            bool
}

type safeToken struct {
	TokenID           string     `json:"token_id"`
	Label             string     `json:"label"`
	MaxRegistrations  int        `json:"max_registrations"`
	UsedRegistrations int        `json:"used_registrations"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	CreatedBy         string     `json:"created_by"`
}

type tokenListReport struct {
	Action string      `json:"action"`
	Tokens []safeToken `json:"tokens"`
}

type tokenMutationReport struct {
	Action             string     `json:"action"`
	Applied            bool       `json:"applied"`
	TokenID            string     `json:"token_id,omitempty"`
	PreviousTokenID    string     `json:"previous_token_id,omitempty"`
	Label              string     `json:"label,omitempty"`
	MaxRegistrations   int        `json:"max_registrations,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	InitialAccessToken string     `json:"initial_access_token,omitempty"`
}

type auditReport struct {
	Action string                 `json:"action"`
	Events []models.DCRAuditEvent `json:"events"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "tutor-dcr-admin:", err)
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

	switch opts.Action {
	case "list":
		tokens, err := store.ListDCRInitialAccessTokens(ctx, opts.Limit)
		if err != nil {
			return err
		}
		safe := make([]safeToken, 0, len(tokens))
		for _, token := range tokens {
			safe = append(safe, safeDCRToken(token))
		}
		return encoder.Encode(tokenListReport{Action: "list", Tokens: safe})
	case "audit":
		events, err := store.ListDCRAudit(ctx, opts.Limit)
		if err != nil {
			return err
		}
		return encoder.Encode(auditReport{Action: "audit", Events: events})
	case "revoke":
		report := tokenMutationReport{Action: "revoke", Applied: false, TokenID: opts.TokenID}
		if !opts.Apply {
			return encoder.Encode(report)
		}
		applied, err := store.RevokeDCRInitialAccessToken(ctx, opts.TokenID, opts.Actor, opts.Reason, time.Now().UTC())
		if err != nil {
			return err
		}
		report.Applied = applied
		return encoder.Encode(report)
	case "create", "rotate":
		report := tokenMutationReport{
			Action: opts.Action, Applied: false, TokenID: opts.TokenID,
			PreviousTokenID: opts.PreviousTokenID, Label: opts.Label,
			MaxRegistrations: opts.MaxRegistrations,
		}
		if !opts.Apply {
			return encoder.Encode(report)
		}
		rawToken, err := randomBase64URL(32)
		if err != nil {
			return fmt.Errorf("generate initial access token: %w", err)
		}
		if opts.TokenID == "" {
			suffix, err := randomBase64URL(12)
			if err != nil {
				return fmt.Errorf("generate token ID: %w", err)
			}
			opts.TokenID = "iat-" + suffix
			report.TokenID = opts.TokenID
		}
		now := time.Now().UTC()
		var expiresAt *time.Time
		if opts.ExpiresIn > 0 {
			value := now.Add(opts.ExpiresIn)
			expiresAt = &value
			report.ExpiresAt = expiresAt
		}
		hash := sha256.Sum256([]byte(rawToken))
		token := models.DCRInitialAccessToken{
			TokenID: opts.TokenID, TokenHash: fmt.Sprintf("%x", hash[:]),
			Label: opts.Label, MaxRegistrations: opts.MaxRegistrations,
			CreatedAt: now, ExpiresAt: expiresAt, CreatedBy: opts.Actor,
		}
		if err := store.CreateDCRInitialAccessToken(ctx, token, opts.PreviousTokenID); err != nil {
			return err
		}
		report.Applied = true
		report.InitialAccessToken = rawToken
		return encoder.Encode(report)
	default:
		return fmt.Errorf("unsupported action %q", opts.Action)
	}
}

func safeDCRToken(token models.DCRInitialAccessToken) safeToken {
	return safeToken{
		TokenID: token.TokenID, Label: token.Label,
		MaxRegistrations: token.MaxRegistrations, UsedRegistrations: token.UsedRegistrations,
		CreatedAt: token.CreatedAt, ExpiresAt: token.ExpiresAt,
		RevokedAt: token.RevokedAt, CreatedBy: token.CreatedBy,
	}
}

func randomBase64URL(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	maxConns, err := envInt(getenv, "DB_MAX_CONNS", 10)
	if err != nil {
		return options{}, err
	}
	opts := options{
		Driver: strings.ToLower(strings.TrimSpace(getenv("DB_DRIVER"))),
		DBPath: strings.TrimSpace(getenv("DB_PATH")), MaxConns: maxConns,
		Action: "list", Limit: 100, Actor: strings.TrimSpace(getenv("DCR_ADMIN_ACTOR")),
		MaxRegistrations: 1000,
	}
	if opts.Driver == "" {
		opts.Driver = "sqlite"
	}
	if opts.DBPath == "" {
		opts.DBPath = "./data/runtime.db"
	}
	var expiresIn string
	fs := flag.NewFlagSet("tutor-dcr-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.Driver, "db-driver", opts.Driver, "database driver: sqlite or postgres")
	fs.StringVar(&opts.DBPath, "db-path", opts.DBPath, "existing SQLite database path")
	fs.IntVar(&opts.MaxConns, "db-max-conns", opts.MaxConns, "PostgreSQL pool size")
	fs.StringVar(&opts.Action, "action", opts.Action, "action: list, audit, create, rotate, or revoke")
	fs.IntVar(&opts.Limit, "limit", opts.Limit, "maximum list/audit rows (1..1000)")
	fs.StringVar(&opts.TokenID, "token-id", "", "safe token identifier; generated for create/rotate when omitted")
	fs.StringVar(&opts.PreviousTokenID, "previous-token-id", "", "active token being rotated; it remains valid")
	fs.StringVar(&opts.Label, "label", "", "operator label for create/rotate")
	fs.StringVar(&opts.Actor, "actor", opts.Actor, "operator identity for the durable audit")
	fs.StringVar(&opts.Reason, "reason", "", "required revoke reason")
	fs.IntVar(&opts.MaxRegistrations, "max-registrations", opts.MaxRegistrations, "per-token registration quota (1..100000)")
	fs.StringVar(&expiresIn, "expires-in", "", "optional token lifetime, for example 168h")
	fs.BoolVar(&opts.Apply, "apply", false, "apply mutation; omitted means read-only preview")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	opts.Driver = strings.ToLower(strings.TrimSpace(opts.Driver))
	opts.Action = strings.ToLower(strings.TrimSpace(opts.Action))
	opts.TokenID = strings.TrimSpace(opts.TokenID)
	opts.PreviousTokenID = strings.TrimSpace(opts.PreviousTokenID)
	opts.Label = strings.TrimSpace(opts.Label)
	opts.Actor = strings.TrimSpace(opts.Actor)
	opts.Reason = strings.TrimSpace(opts.Reason)
	if opts.Driver != "sqlite" && opts.Driver != "postgres" {
		return options{}, fmt.Errorf("unknown database driver %q", opts.Driver)
	}
	if opts.MaxConns <= 0 {
		return options{}, fmt.Errorf("db-max-conns must be greater than zero")
	}
	if opts.Limit < 1 || opts.Limit > 1000 {
		return options{}, fmt.Errorf("limit must be between 1 and 1000")
	}
	switch opts.Action {
	case "list", "audit":
		if opts.Apply || opts.TokenID != "" || opts.PreviousTokenID != "" || opts.Label != "" || opts.Reason != "" || expiresIn != "" {
			return options{}, fmt.Errorf("%s does not accept mutation flags", opts.Action)
		}
		return opts, nil
	case "create", "rotate":
		if opts.Label == "" || opts.Actor == "" {
			return options{}, fmt.Errorf("%s requires label and actor", opts.Action)
		}
		if opts.MaxRegistrations < 1 || opts.MaxRegistrations > 100000 {
			return options{}, fmt.Errorf("max-registrations must be between 1 and 100000")
		}
		if opts.Action == "rotate" && opts.PreviousTokenID == "" {
			return options{}, fmt.Errorf("rotate requires previous-token-id")
		}
		if opts.Action == "create" && opts.PreviousTokenID != "" {
			return options{}, fmt.Errorf("create does not accept previous-token-id")
		}
		if opts.Reason != "" {
			return options{}, fmt.Errorf("%s does not accept reason", opts.Action)
		}
	case "revoke":
		if opts.TokenID == "" || opts.Actor == "" || opts.Reason == "" {
			return options{}, fmt.Errorf("revoke requires token-id, actor, and reason")
		}
		if opts.PreviousTokenID != "" || opts.Label != "" || expiresIn != "" {
			return options{}, fmt.Errorf("revoke does not accept create/rotate flags")
		}
		return opts, nil
	default:
		return options{}, fmt.Errorf("action must be list, audit, create, rotate, or revoke")
	}
	if expiresIn != "" {
		value, err := time.ParseDuration(expiresIn)
		if err != nil || value <= 0 || value > 365*24*time.Hour {
			return options{}, fmt.Errorf("expires-in must be a duration greater than zero and at most 8760h")
		}
		opts.ExpiresIn = value
	}
	return opts, nil
}

func openStore(ctx context.Context, opts options, getenv func(string) string) (*db.Store, error) {
	write := opts.Apply && opts.Action != "list" && opts.Action != "audit"
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
		if write {
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
