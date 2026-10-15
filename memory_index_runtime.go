package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/memory"
	"tutor-mcp/models"
)

var narrativeIndexWorker = models.WorkerPrincipal{ActorID: "narrative-index"}

func repairSessionIndexScope(ctx context.Context, persistence *db.Store, scope models.TenantScope, batch int) (db.SessionIndexBackfillReport, error) {
	total := db.SessionIndexBackfillReport{Remaining: -1}
	after := ""
	for {
		pageCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		page, next, err := persistence.BackfillSessionNarrativeIndex(pageCtx, scope, after, batch)
		cancel()
		total.Scanned += page.Scanned
		total.Indexed += page.Indexed
		total.Unresolved += page.Unresolved
		total.Remaining = page.Remaining
		total.RemainingKnown = page.RemainingKnown
		if err != nil {
			return total, err
		}
		if next == "" {
			if !page.RemainingKnown || page.Remaining != 0 {
				return total, memory.ErrSessionIndexIncomplete
			}
			return total, nil
		}
		after = next
	}
}

func repairNarrativeIndexPass(ctx context.Context, persistence *db.Store, database bool) (db.SessionIndexBackfillReport, error) {
	if !database {
		n, err := memory.RepairFileSessionIndexes(ctx)
		report := db.SessionIndexBackfillReport{Indexed: int64(n), RemainingKnown: err == nil}
		if err != nil {
			report.Remaining = -1
		}
		return report, err
	}
	total := db.SessionIndexBackfillReport{RemainingKnown: true}
	after := ""
	failed := false
	for {
		pageCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		scopes, next, err := persistence.ListWorkerTenantScopes(pageCtx, narrativeIndexWorker, after, 100)
		cancel()
		if err != nil {
			total.Remaining = -1
			total.RemainingKnown = false
			return total, err
		}
		for _, scope := range scopes {
			if err := ctx.Err(); err != nil {
				total.Remaining = -1
				total.RemainingKnown = false
				return total, err
			}
			report, err := repairSessionIndexScope(ctx, persistence, scope, 100)
			total.Scanned += report.Scanned
			total.Indexed += report.Indexed
			total.Unresolved += report.Unresolved
			if !report.RemainingKnown {
				total.RemainingKnown = false
				total.Remaining = -1
			} else if total.RemainingKnown {
				total.Remaining += report.Remaining
			}
			if err != nil {
				failed = true
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if failed {
		return total, memory.ErrSessionIndexIncomplete
	}
	return total, nil
}

func safeNarrativeIndexPass(ctx context.Context, persistence *db.Store, database bool) (report db.SessionIndexBackfillReport, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("memory: narrative index repair panic")
		}
	}()
	return repairNarrativeIndexPass(ctx, persistence, database)
}

// startNarrativeIndexRepair performs a bounded startup pass, then resumes all
// remaining pages outside requests. Exactly one goroutine owns each lifecycle.
// Its stop function cancels and joins before callers close the Store.
func startNarrativeIndexRepair(parent context.Context, persistence *db.Store, logger *slog.Logger, database bool, interval time.Duration) func() {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	initialCtx, initialCancel := context.WithTimeout(ctx, 2*time.Second)
	_, _ = safeNarrativeIndexPass(initialCtx, persistence, database)
	initialCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			report, err := safeNarrativeIndexPass(ctx, persistence, database)
			if ctx.Err() != nil {
				return
			}
			if logger != nil && (err != nil || report.Scanned > 0 || report.Indexed > 0) {
				status := "complete"
				if err != nil {
					status = "incomplete"
				}
				logger.Info("narrative session index repair", "status", status, "scanned", report.Scanned, "indexed", report.Indexed, "unresolved", report.Unresolved, "remaining", report.Remaining, "remaining_known", report.RemainingKnown)
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func runMemoryIndexCommand(ctx context.Context, options commandOptions, output io.Writer) error {
	if options.IndexBatch < 1 || options.IndexBatch > 100 {
		return fmt.Errorf("memory-index requires --batch-size 1-100")
	}
	// The command bypasses server configuration, but uses the same bounded
	// narrative reads/manifests as the runtime, including operator overrides.
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		return err
	}
	if err := memory.ConfigureLimits(limits); err != nil {
		return err
	}
	if options.IndexTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.IndexTimeout)
		defer cancel()
	}
	if options.IndexFilesystem {
		info, err := os.Stat(options.IndexRoot)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("memory-index requires an existing filesystem memory root")
		}
		n, err := memory.RebuildFileSessionIndexesAt(ctx, options.IndexRoot)
		if encodeErr := json.NewEncoder(output).Encode(map[string]any{"indexed": n, "complete": err == nil}); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	persistence, tenantID, err := openMemoryIndexStore(ctx, options)
	if err != nil {
		return err
	}
	defer persistence.Close()
	// Validate visibility against the existing global-root enumeration. A
	// misspelled/non-visible tenant must not be reported as an empty success.
	var selected models.TenantScope
	after := ""
	for {
		scopes, next, err := persistence.ListWorkerTenantScopes(ctx, narrativeIndexWorker, after, 100)
		if err != nil {
			return err
		}
		for _, scope := range scopes {
			if scope.TenantID == tenantID {
				selected = scope
				break
			}
		}
		if selected.TenantID != "" || next == "" {
			break
		}
		after = next
	}
	if selected.TenantID == "" {
		return fmt.Errorf("memory-index tenant is not an existing visible active tenant")
	}
	report, repairErr := repairSessionIndexScope(ctx, persistence, selected, options.IndexBatch)
	if err := json.NewEncoder(output).Encode(struct {
		db.SessionIndexBackfillReport
		Complete bool `json:"complete"`
	}{report, repairErr == nil}); err != nil {
		return err
	}
	return repairErr
}

func openMemoryIndexStore(ctx context.Context, options commandOptions) (*db.Store, string, error) {
	if options.Local || options.Profile == "hobby" {
		profile := "hobby"
		if options.Local {
			profile = "local"
		}
		dir, err := profileDataDir(profile, options.DataDir)
		if err != nil {
			return nil, "", err
		}
		persistence, _, err := openProfileStoreMode(ctx, profile, dir, "", false, false)
		if err != nil {
			return nil, "", err
		}
		if options.IndexTenant != "" && options.IndexTenant != models.LegacyTenantID {
			persistence.Close()
			return nil, "", fmt.Errorf("memory-index local/hobby tenant must be tenant_legacy")
		}
		return persistence, models.LegacyTenantID, nil
	}
	if options.IndexTenant == "" {
		return nil, "", fmt.Errorf("memory-index requires an explicit --tenant")
	}
	driver := strings.TrimSpace(os.Getenv("DB_DRIVER"))
	if driver == "" {
		driver = "sqlite"
	}
	if options.Profile == "institution" {
		driver = "postgres"
	}
	var persistence *db.Store
	switch driver {
	case "sqlite":
		path := strings.TrimSpace(os.Getenv("DB_PATH"))
		info, err := os.Lstat(path)
		if path == "" || err != nil || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("memory-index requires DB_PATH pointing to an existing SQLite installation")
		}
		database, err := db.OpenDB(path)
		if err != nil {
			return nil, "", err
		}
		var profile string
		if err := database.QueryRowContext(ctx, `SELECT profile FROM installation WHERE singleton=1`).Scan(&profile); err != nil || profile != "legacy" {
			database.Close()
			return nil, "", fmt.Errorf("memory-index requires an existing legacy installation (use --local or --profile hobby for profiles)")
		}
		if err := db.MigrateContext(ctx, database); err != nil {
			database.Close()
			return nil, "", err
		}
		persistence, err = db.NewSQLiteStore(database, path)
		if err != nil {
			_ = database.Close()
			return nil, "", err
		}
	case "postgres":
		dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if dsn == "" {
			return nil, "", fmt.Errorf("memory-index requires DATABASE_URL")
		}
		database, err := db.OpenPostgres(dsn, 2)
		if err != nil {
			return nil, "", err
		}
		if err := db.VerifyPostgresRuntimeRole(ctx, database); err != nil {
			database.Close()
			return nil, "", fmt.Errorf("memory-index requires a non-owner runtime PostgreSQL role: %w", err)
		}
		if err := db.VerifyPostgresSchemaCurrent(ctx, database); err != nil {
			database.Close()
			return nil, "", fmt.Errorf("memory-index requires current migrations: %w", err)
		}
		var profile string
		if err := database.QueryRowContext(ctx, `SELECT profile FROM installation WHERE singleton=1`).Scan(&profile); err != nil || (profile != "legacy" && profile != "institution") {
			database.Close()
			return nil, "", fmt.Errorf("memory-index requires an existing server installation")
		}
		persistence = db.NewStoreWithDialect(database, db.DialectPostgres)
	default:
		return nil, "", fmt.Errorf("memory-index DB_DRIVER must be sqlite or postgres")
	}
	keyring, err := db.NewIntegrationSecretKeyring(os.Getenv("INTEGRATION_SECRET_KEYS"), os.Getenv("INTEGRATION_SECRET_CURRENT_KEY_ID"))
	if err != nil {
		persistence.Close()
		return nil, "", fmt.Errorf("memory-index requires the existing narrative keyring")
	}
	persistence.SetIntegrationSecretKeyring(keyring)
	return persistence, options.IndexTenant, nil
}
