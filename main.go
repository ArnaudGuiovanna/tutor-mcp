// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// AI Learning MCP Server
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // Portable binaries must resolve learner IANA zones without a Go installation.

	"tutor-mcp/adminapi"
	"tutor-mcp/auth"
	"tutor-mcp/certification"
	"tutor-mcp/clientapi"
	"tutor-mcp/db"
	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
	"tutor-mcp/tools"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if shouldPrintVersion(os.Args[1:]) {
		fmt.Println(versionLine())
		return
	}

	options, err := parseCommand(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       parseLogLevel(os.Getenv("LOG_LEVEL")),
		ReplaceAttr: newPrivacySafeLogAttr(),
	}))
	// Packages using the package-level slog helpers must inherit the same
	// privacy and level policy as explicitly injected components.
	slog.SetDefault(logger)

	// run owns every resource and releases it through its own defers before
	// returning; main only turns the outcome into an exit status.
	if err := run(options, logger); err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

// errReported marks a failure that was already logged with its context.
var errReported = errors.New("failure already reported")

// reportError logs a startup failure through the privacy-safe logger and
// returns errReported, so the caller can unwind its deferred cleanup.
func reportError(logger *slog.Logger, msg string, err error, attrs ...any) error {
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	logger.Error(msg, attrs...)
	return errReported
}

func run(options commandOptions, logger *slog.Logger) error {
	if options.Command == "memory-index" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runMemoryIndexCommand(ctx, options, os.Stdout)
	}
	if options.Local {
		if err := runLocal(options, logger); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("local startup/session: %w", err)
		}
		return nil
	}
	if handled, err := configureProfileCommand(context.Background(), options, os.Stdout); err != nil || handled {
		return err
	}
	releaseServer, err := lockHobbyServer(options)
	if err != nil {
		return err
	}
	defer releaseServer()
	return runServer(options, logger)
}

// runServer starts the process role selected by PROCESS_ROLE: migrator,
// worker, api, or all (development).
func runServer(options commandOptions, logger *slog.Logger) error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	cfg, err := loadStartupConfig(port)
	if err != nil {
		return reportError(logger, "invalid startup configuration", err)
	}
	shutdownTelemetry, err := observability.Setup(context.Background(), mcpVersion(), logger)
	if err != nil {
		logger.Error("initialize OpenTelemetry", "error_type", "otel_initialization")
		return errReported
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Error("shutdown OpenTelemetry", "error_type", "otel_shutdown")
		}
	}()
	if err := configureProcessLimits(cfg, logger); err != nil {
		return err
	}
	store, closeStore, err := openStore(cfg, options, logger)
	if err != nil {
		return err
	}
	defer closeStore()
	if cfg.ProcessRole == "migrator" {
		logger.Info("database migrations complete", "driver", cfg.DBDriver)
		return nil
	}
	if err := configureStoredSecrets(store, cfg, logger); err != nil {
		return err
	}
	if memory.Enabled() && cfg.ProcessRole != "worker" {
		stopRepair := startNarrativeIndexRepair(context.Background(), store, logger, cfg.NarrativeMemoryBackend == "database", 30*time.Second)
		defer stopRepair() // cancel and join before the database's deferred Close
	}
	if cfg.ProcessRole == "worker" {
		return runWorkerUntilSignal(store, logger, cfg.SchedulerMode, schedulerOptions(cfg))
	}
	return serveAPI(store, cfg, options, logger)
}

// configureProcessLimits applies process-wide budgets before any request or
// database work: bcrypt concurrency, narrative memory limits and JWT keys.
func configureProcessLimits(cfg startupConfig, logger *slog.Logger) error {
	runsAPI := cfg.ProcessRole == "api" || cfg.ProcessRole == "all"
	if runsAPI {
		if err := auth.ConfigureBcryptMaxConcurrent(cfg.AuthBcryptMaxConcurrent); err != nil {
			return reportError(logger, "configure bcrypt concurrency budget", err)
		}
		logger.Info("bcrypt concurrency budget", "max_concurrent", cfg.AuthBcryptMaxConcurrent)
	}
	memoryLimits, err := memory.LimitsFromEnv()
	if err != nil {
		return reportError(logger, "invalid narrative memory limits", err)
	}
	if err := memory.ConfigureLimits(memoryLimits); err != nil {
		return reportError(logger, "configure narrative memory limits", err)
	}
	logger.Info("narrative memory limits",
		"max_write_bytes", memoryLimits.MaxWriteBytes,
		"max_file_bytes", memoryLimits.MaxFileBytes,
		"max_learner_bytes", memoryLimits.MaxLearnerBytes,
		"max_files_per_learner", memoryLimits.MaxFilesPerLearner,
		"max_concurrent_writes", memoryLimits.MaxConcurrentWrites,
	)
	if runsAPI {
		if err := auth.LoadJWTSecret(); err != nil {
			return reportError(logger, "failed to load JWT secret", err)
		}
	}
	return nil
}

// openStore opens, migrates or verifies the configured database and records
// the installation profile. Default: single-node SQLite (no external deps);
// DB_DRIVER=postgres + DATABASE_URL for horizontal deployments. The returned
// close function releases pool metrics and the database.
func openStore(cfg startupConfig, options commandOptions, logger *slog.Logger) (*db.Store, func(), error) {
	runsMigrations := cfg.ProcessRole == "migrator" || cfg.ProcessRole == "all"
	ctx := context.Background()
	var store *db.Store
	switch cfg.DBDriver {
	case "postgres":
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return nil, nil, reportError(logger, "DB_DRIVER=postgres requires DATABASE_URL", nil)
		}
		database, err := db.OpenPostgres(dsn, envInt("DB_MAX_CONNS", 10))
		if err != nil {
			return nil, nil, reportError(logger, "failed to open postgres", err)
		}
		if err := preparePostgres(ctx, database, cfg, runsMigrations); err != nil {
			_ = database.Close()
			return nil, nil, reportError(logger, err.Error(), errors.Unwrap(err))
		}
		store = db.NewStoreWithDialect(database, db.DialectPostgres)
		logger.Info("database ready", "driver", "postgres")
	case "sqlite":
		if err := ensureSQLiteParent(cfg.DBPath); err != nil {
			return nil, nil, reportError(logger, "failed to prepare sqlite directory", err)
		}
		database, err := db.OpenDB(cfg.DBPath)
		if err != nil {
			return nil, nil, reportError(logger, "failed to open database", err)
		}
		if runsMigrations {
			err = db.Migrate(database)
		} else {
			err = db.VerifySQLiteSchemaCurrent(ctx, database)
		}
		if err != nil {
			_ = database.Close()
			msg := "incompatible SQLite schema; run the migrator first"
			if runsMigrations {
				msg = "failed to migrate database"
			}
			return nil, nil, reportError(logger, msg, err)
		}
		store, err = db.NewSQLiteStore(database, cfg.DBPath)
		if err != nil {
			_ = database.Close()
			return nil, nil, reportError(logger, "open SQLite reader pool", err)
		}
		logger.Info("database ready", "driver", "sqlite", "path", cfg.DBPath)
	default:
		return nil, nil, reportError(logger, "unknown DB_DRIVER (want sqlite|postgres)", nil, "driver", cfg.DBDriver)
	}
	unregisterPoolMetrics, err := db.RegisterStoreDBPools(store)
	if err != nil {
		_ = store.Close()
		logger.Error("register database pool metrics", "error_type", "otel_db_pool")
		return nil, nil, errReported
	}
	closeStore := func() {
		_ = unregisterPoolMetrics()
		_ = store.Close()
	}
	store.SetHostLLMDemonstrationPolicy(cfg.AssessmentHostLLMDemonstrates)
	store.SetMaxActiveDomainsPerLearner(cfg.MaxActiveDomainsPerLearner)
	logger.Info("assessment evidence policy", "host_llm_demonstrates", cfg.AssessmentHostLLMDemonstrates)
	installationProfile := "legacy"
	if cfg.DeploymentProfile == "production" {
		installationProfile = "institution"
	}
	if options.Profile == "hobby" {
		installationProfile = "hobby"
	}
	if err := store.EnsureInstallation(ctx, installationProfile); err != nil {
		closeStore()
		return nil, nil, err
	}
	return store, closeStore, nil
}

// postgresStartupError carries the operator message of a PostgreSQL startup
// gate; the wrapped error is logged only through the privacy-safe handler.
type postgresStartupError struct {
	msg string
	err error
}

func (e *postgresStartupError) Error() string { return e.msg }
func (e *postgresStartupError) Unwrap() error { return e.err }

func preparePostgres(ctx context.Context, database *sql.DB, cfg startupConfig, runsMigrations bool) error {
	if runsMigrations {
		if err := db.MigratePostgres(ctx, database); err != nil {
			return &postgresStartupError{"failed to migrate postgres", err}
		}
	} else if err := db.VerifyPostgresSchemaCurrent(ctx, database); err != nil {
		return &postgresStartupError{"incompatible postgres schema; run the migrator first", err}
	}
	if !runsMigrations && cfg.DeploymentProfile == "production" {
		if err := db.VerifyPostgresRuntimeRole(ctx, database); err != nil {
			return &postgresStartupError{"unsafe postgres runtime credentials", err}
		}
	}
	return nil
}

// configureStoredSecrets installs the tenant integration allowlist, the
// integration secret keyring (rotating stored envelopes on the API) and the
// narrative memory backend.
func configureStoredSecrets(store *db.Store, cfg startupConfig, logger *slog.Logger) error {
	if err := db.ConfigureTenantIntegrationAllowedHosts(store, cfg.TenantIntegrationHosts); err != nil {
		return reportError(logger, "invalid tenant integration allowlist", err)
	}
	integrationEncryptionEnabled := false
	var narrativeSecretsRotated int64
	if encodedKeys := strings.TrimSpace(os.Getenv("INTEGRATION_SECRET_KEYS")); encodedKeys != "" {
		keyring, err := db.NewIntegrationSecretKeyring(encodedKeys, os.Getenv("INTEGRATION_SECRET_CURRENT_KEY_ID"))
		if err != nil {
			return reportError(logger, "invalid integration secret keyring", err)
		}
		store.SetIntegrationSecretKeyring(keyring)
		integrationEncryptionEnabled = true
		if rotatesSecretsAtStartup(cfg.ProcessRole) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rotated, err := store.RotateIntegrationSecrets(ctx)
			cancel()
			if err != nil {
				return reportError(logger, "failed to rotate integration secrets", err)
			}
			logger.Info("integration secret encryption enabled", "rotated_records", rotated)
			narrativeRotateCtx, narrativeRotateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			narrativeSecretsRotated, err = store.RotateNarrativeSecrets(narrativeRotateCtx)
			narrativeRotateCancel()
			if err != nil {
				return reportError(logger, "failed to authenticate or rotate narrative objects", err)
			}
		} else {
			logger.Info("integration secret encryption enabled", "rotation", "delegated to the api process")
		}
	} else {
		logger.Warn("integration secret encryption disabled; webhook credentials remain plaintext")
	}
	if !memory.Enabled() || cfg.NarrativeMemoryBackend != "database" {
		memory.ConfigureNarrativeStore(nil)
		logger.Info("narrative memory backend ready", "backend", cfg.NarrativeMemoryBackend, "enabled", memory.Enabled())
		return nil
	}
	if !integrationEncryptionEnabled {
		return reportError(logger, "database narrative memory requires the integration secret keyring", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	backfill, backfillErr := memory.BackfillLocalNarratives(ctx, store, memory.Root())
	cancel()
	if backfillErr != nil {
		return reportError(logger, "failed to reconcile local narrative memory", backfillErr,
			"scanned", backfill.Scanned, "imported", backfill.Imported,
			"reconciled", backfill.Reconciled, "conflicts", backfill.Conflicts,
		)
	}
	memory.ConfigureNarrativeStore(store)
	logger.Info("narrative memory backend ready",
		"backend", "database", "encrypted", true, "rotated", narrativeSecretsRotated,
		"backfill_scanned", backfill.Scanned, "backfill_imported", backfill.Imported,
		"backfill_reconciled", backfill.Reconciled,
	)
	return nil
}

// serveAPI wires the HTTP surface of an api (or all) process and serves it
// until SIGINT/SIGTERM.
func serveAPI(store *db.Store, cfg startupConfig, options commandOptions, logger *slog.Logger) error {
	logger.Info("MCP transport limits",
		"mode", "stateless",
		"max_request_body_bytes", cfg.MCPMaxRequestBodyBytes,
		"max_concurrent", cfg.MCPMaxConcurrent,
		"max_concurrent_per_learner", cfg.MCPMaxConcurrentUser,
		"tool_call_timeout", cfg.MCPToolCallTimeout,
	)
	concurrency, err := auth.NewPrincipalConcurrencyLimiter(cfg.MCPMaxConcurrent, cfg.MCPMaxConcurrentUser)
	if err != nil {
		return reportError(logger, "invalid MCP concurrency limits", err)
	}
	oauthServer, err := newOAuthServer(store, cfg, options, logger)
	if err != nil {
		return err
	}
	var nativeIdentity *auth.NativeIdentityServer
	if cfg.NativeIdentity {
		nativeIdentity, err = auth.NewNativeIdentityServer(store, auth.NativeIdentityOptions{
			BaseURL: cfg.BaseURL, InstitutionID: cfg.InstitutionID, InstitutionName: cfg.InstitutionName,
		})
		if err != nil {
			return reportError(logger, "configure native key identity", err)
		}
		defer nativeIdentity.Stop()
		logger.Info("institution identity ready", "method", "nostr-key")
	}

	// Warn loudly if the deployment looks public but TRUSTED_PROXY_CIDRS is
	// unset - without it every IP-limited request shares one bucket under the
	// proxy's loopback IP (issue #37).
	auth.WarnRateLimiterMisconfig(cfg.BaseURL)
	limiters := newRouteLimiters(cfg)
	defer limiters.Stop()
	if cfg.RateLimitBackend == "postgres" {
		// RATELIMIT_BACKEND=postgres (alias "db") opts every shared limiter,
		// the native challenge budgets and the per-account login-failure
		// tracker into one fleet-wide, DB-backed view. The default "memory"
		// keeps independent per-process counters (single-node deployments).
		backend := db.NewRateLimitBackend(store)
		limiters.SetBackend(backend)
		nativeIdentity.SetRateLimitBackend(backend)
		oauthServer.SetLoginFailureBackend(db.NewLoginFailureBackend(store))
		logger.Info("rate limit backend", "mode", "postgres (shared, fleet-wide)")
	}

	mux := http.NewServeMux()
	// Liveness never depends on an external service. Readiness is bounded and
	// reflects whether this instance can safely receive traffic. /health
	// remains a backwards-compatible alias for readiness.
	mux.HandleFunc("GET /live", livenessHandler)
	ready := readinessHandler(store)
	mux.HandleFunc("GET /ready", ready)
	mux.HandleFunc("GET /health", ready)
	mountOAuthRoutes(mux, oauthServer, nativeIdentity, limiters, cfg, options)
	guards := mcpHTTPGuards{
		IP: limiters.mcpIP, Learner: limiters.mcpLearner, Concurrency: concurrency,
		NativeOnly: cfg.NativeIdentity,
	}
	mountMCPRoutes(mux, store, logger, cfg, options, guards)
	if err := mountAdminRoutes(mux, store, logger, cfg, guards); err != nil {
		return err
	}

	if cfg.ProcessRole == "all" {
		// Development PROCESS_ROLE=all preserves the one-process experience.
		scheduler, err := startScheduler(store, logger, cfg.SchedulerMode, schedulerOptions(cfg))
		if err != nil {
			return err
		}
		defer scheduler.Stop()
	}

	server := &http.Server{
		Addr:              httpListenAddress(options, cfg.Port),
		Handler:           wrapHTTPHandler(logger, cfg.BaseURL, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
	logger.Info("tutor mcp starting", "port", cfg.Port, "base_url", cfg.BaseURL)
	return serveUntilSignal(server, oauthServer, cfg.MCPToolCallTimeout, logger)
}

// newOAuthServer configures the authorization server: hobby accounts, scope
// issuance, dynamic client registration, CIMD and account e-mail delivery.
func newOAuthServer(store *db.Store, cfg startupConfig, options commandOptions, logger *slog.Logger) (*auth.OAuthServer, error) {
	oauthServer := auth.NewOAuthServer(store, cfg.BaseURL, logger)
	if options.Profile == "hobby" {
		if err := oauthServer.EnableHobbyAccounts(); err != nil {
			return nil, err
		}
	}
	oauthServer.SetGranularScopesEnabled(cfg.OAuthGranularScopes)
	if err := oauthServer.ConfigureDynamicClientRegistration(cfg.OAuthDCRMode, cfg.OAuthDCRInitialTokenHash); err != nil {
		return nil, reportError(logger, "configure dynamic client registration", err)
	}
	logger.Info("dynamic client registration policy", "mode", cfg.OAuthDCRMode)
	oauthServer.ConfigureCIMD(cfg.OAuthCIMD)
	smtpAddress := strings.TrimSpace(os.Getenv("SMTP_ADDR"))
	switch {
	case cfg.NativeIdentity:
		// Native identities have no mailbox or password workflow.
	case smtpAddress != "":
		emailSender, err := auth.NewSMTPEmailSender(auth.SMTPConfig{
			Address:    smtpAddress,
			ServerName: strings.TrimSpace(os.Getenv("SMTP_SERVER_NAME")),
			From:       strings.TrimSpace(os.Getenv("SMTP_FROM")),
			Username:   os.Getenv("SMTP_USERNAME"),
			Password:   os.Getenv("SMTP_PASSWORD"),
		})
		if err != nil {
			return nil, reportError(logger, "invalid SMTP configuration", err)
		}
		oauthServer.SetEmailSender(emailSender)
		logger.Info("account email delivery enabled", "transport", "smtp_starttls")
	case options.Profile != "hobby":
		logger.Warn("account email delivery disabled; public registration and password recovery cannot complete")
	}
	return oauthServer, nil
}

// routeLimiters are the per-route token buckets of the API process.
type routeLimiters struct {
	authorize *auth.RateLimiter // consent/login page loads
	login     *auth.RateLimiter // password verification
	token     *auth.RateLimiter // code exchange and refresh
	register  *auth.RateLimiter // dynamic client registration
	// Process-local only: it runs before IAT admission, so an anonymous
	// request reaches neither the database nor the shared limiter backend.
	registerAdmission *auth.RateLimiter
	registerDaily     *auth.RateLimiter
	account           *auth.RateLimiter // verification and recovery
	// Native ceremonies prove a key rather than a guessable password. A normal
	// connection, grant change or logout each needs a challenge and a signature.
	native     *auth.RateLimiter
	mcpIP      *auth.RateLimiter
	mcpLearner *auth.RateLimiter
}

func newRouteLimiters(cfg startupConfig) *routeLimiters {
	mcpRatePerMinute := envFloat("MCP_RATE_LIMIT_PER_MIN", 60)
	mcpBurst := envInt("MCP_RATE_LIMIT_BURST", 60)
	return &routeLimiters{
		authorize:         auth.NewRateLimiterWithNamespace("oauth_authorize", 30.0/60, 10),
		login:             auth.NewRateLimiterWithNamespace("oauth_login", 5.0/60, 5),
		token:             auth.NewRateLimiterWithNamespace("oauth_token", 20.0/60, 10),
		register:          auth.NewRateLimiterWithNamespace("oauth_register", 5.0/60, 5),
		registerAdmission: auth.NewRateLimiterWithNamespace("oauth_register_admission", 30.0/60, 30),
		registerDaily: auth.NewRateLimiterWithNamespace("oauth_register_daily",
			float64(cfg.OAuthDCRMaxRegistrationsPerIPPerDay)/(24*60*60), cfg.OAuthDCRMaxRegistrationsPerIPPerDay),
		account:    auth.NewRateLimiterWithNamespace("oauth_account", 5.0/60, 5),
		native:     auth.NewRateLimiterWithNamespace("native_identity", 30.0/60, 20),
		mcpIP:      auth.NewRateLimiterWithNamespace("mcp_ip", mcpRatePerMinute/60, mcpBurst),
		mcpLearner: auth.NewRateLimiterWithNamespace("mcp_learner", mcpRatePerMinute/60, mcpBurst),
	}
}

// shared lists the limiters that may use the fleet-wide backend.
func (l *routeLimiters) shared() []*auth.RateLimiter {
	return []*auth.RateLimiter{l.authorize, l.login, l.token, l.register, l.registerDaily, l.account, l.native, l.mcpIP, l.mcpLearner}
}

// SetBackend moves every shared limiter to backend. registerAdmission stays
// process-local by design.
func (l *routeLimiters) SetBackend(backend auth.RateLimitBackend) {
	for _, limiter := range l.shared() {
		limiter.SetBackend(backend)
	}
}

// Stop releases every limiter's background cleanup.
func (l *routeLimiters) Stop() {
	for _, limiter := range append(l.shared(), l.registerAdmission) {
		limiter.Stop()
	}
}

// mountOAuthRoutes serves discovery and either the native key ceremonies or
// the password OAuth flow with its account pages.
func mountOAuthRoutes(mux *http.ServeMux, oauthServer *auth.OAuthServer, nativeIdentity *auth.NativeIdentityServer, limiters *routeLimiters, cfg startupConfig, options commandOptions) {
	limit := func(limiter *auth.RateLimiter, handler http.HandlerFunc) http.Handler {
		return auth.RateLimitMiddleware(limiter, handler)
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauthServer.HandleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/jwks.json", auth.HandleJWKS)
	if nativeIdentity != nil {
		mountNativeIdentity(mux, nativeIdentity, limiters.authorize, limiters.native)
		return
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthServer.HandleAuthServerMetadata)
	mux.Handle("GET /authorize", limit(limiters.authorize, oauthServer.HandleAuthorizeGet))
	mux.Handle("POST /authorize", limit(limiters.login, oauthServer.HandleAuthorizePost))
	mux.Handle("POST /token", limit(limiters.token, oauthServer.HandleToken))
	mountDynamicClientRegistration(mux, cfg.OAuthDCRMode, oauthServer, dcrRouteLimiters{
		Admission: limiters.registerAdmission, Requests: limiters.register, Daily: limiters.registerDaily,
	})
	if options.Profile == "hobby" {
		for _, path := range []string{"/account/invite", "/account/reset"} {
			mux.Handle("GET "+path, limit(limiters.authorize, oauthServer.HandleHobbyAccountGet))
			mux.Handle("POST "+path, limit(limiters.account, oauthServer.HandleHobbyAccountPost))
		}
		return
	}
	mux.Handle("GET /verify-email", limit(limiters.authorize, oauthServer.HandleVerifyEmailGet))
	mux.Handle("POST /verify-email", limit(limiters.account, oauthServer.HandleVerifyEmailPost))
	mux.Handle("GET /recover", limit(limiters.authorize, oauthServer.HandleRecoverGet))
	mux.Handle("POST /recover", limit(limiters.account, oauthServer.HandleRecoverPost))
	mux.Handle("GET /reset-password", limit(limiters.authorize, oauthServer.HandleResetPasswordGet))
	mux.Handle("POST /reset-password", limit(limiters.account, oauthServer.HandleResetPasswordPost))
	mux.Handle("GET /login-challenge", limit(limiters.authorize, oauthServer.HandleLoginChallengeGet))
	mux.Handle("POST /login-challenge", limit(limiters.account, oauthServer.HandleLoginChallengePost))
}

// mountMCPRoutes serves /mcp and the native client view. On /mcp the per-IP
// shield runs before auth, then per-learner limiting after auth. The body
// limit runs after both guards so rejected callers cannot make the process
// buffer request bodies, while valid POSTs are bounded before the SDK.
func mountMCPRoutes(mux *http.ServeMux, store *db.Store, logger *slog.Logger, cfg startupConfig, options commandOptions, guards mcpHTTPGuards) {
	mux.Handle("/mcp", newMCPHTTPHandler(&tools.Deps{
		Store: store, Logger: logger, BaseURL: cfg.BaseURL, Institution: options.Profile == "institution",
		OAuthGranularScopes: cfg.OAuthGranularScopes,
	}, cfg.MCPMaxRequestBodyBytes, cfg.MCPToolCallTimeout, guards))
	mountClientAPI(mux, store, logger, cfg.BaseURL, guards)
}

// mountAdminRoutes serves the bearer-authenticated administrative APIs.
// Native institutions delegate them through narrowly scoped key grants.
func mountAdminRoutes(mux *http.ServeMux, store *db.Store, logger *slog.Logger, cfg startupConfig, guards mcpHTTPGuards) error {
	initialOAuthScope := initialMCPOAuthScope(cfg.OAuthGranularScopes)
	mountAdmin := func(prefix string, handler http.Handler) {
		authenticate := func(next http.Handler) http.Handler {
			if cfg.NativeIdentity {
				return auth.BearerMiddlewareWithNativePrincipalValidator(cfg.BaseURL, initialOAuthScope, store,
					nativeAdministrativeScope(prefix, next))
			}
			return auth.BearerMiddlewareWithPrincipalValidator(cfg.BaseURL, initialOAuthScope, store, next)
		}
		mux.Handle(prefix, administrativeHandler(guards.IP, guards.Learner, guards.Concurrency, authenticate, handler))
	}
	mountAdmin("/admin/catalog/", adminapi.New(store, logger).Handler())
	// Raw response review and opinion recording are separate administrative
	// capabilities; the MCP handler never receives a trusted evaluation port.
	assessmentCertifiers, err := certification.New(strings.TrimRight(cfg.BaseURL, "/")+"/admin/assessment-reviews", os.Getenv("ASSESSMENT_CERTIFIERS_JSON"))
	if err != nil {
		logger.Error("invalid independent assessment authority configuration")
		return errReported
	}
	mountAdmin("/admin/assessment-reviews/", adminapi.NewAssessmentReview(store, logger, db.NewAssessmentAdjudicator(store, assessmentCertifiers)).Handler())
	mountAdmin("/admin/curriculum-reviews/", adminapi.NewCurriculumReview(store, logger).Handler())
	return nil
}

// serveUntilSignal serves until SIGINT/SIGTERM, then drains in-flight
// requests within gracefulShutdownTimeout and flushes queued account e-mail.
func serveUntilSignal(server *http.Server, oauthServer *auth.OAuthServer, toolCallTimeout time.Duration, logger *slog.Logger) error {
	shutdownErr := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	stopWaiting := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			logger.Info("shutdown signal received", "signal", sig.String())
		case <-stopWaiting:
		}
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout(toolCallTimeout))
		defer cancel()
		err := server.Shutdown(ctx)
		if drainErr := oauthServer.DrainBackgroundMail(ctx); drainErr != nil {
			logger.Warn("account emails still pending at shutdown", "err", drainErr)
		}
		shutdownErr <- err
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// Still drain mail already accepted by an earlier request.
		close(stopWaiting)
		<-shutdownErr
		return reportError(logger, "server failed", err)
	}
	if err := <-shutdownErr; err != nil {
		return reportError(logger, "graceful shutdown failed", err)
	}
	logger.Info("server stopped cleanly")
	return nil
}

const (
	minGracefulShutdownTimeout = 30 * time.Second
	gracefulShutdownMargin     = 10 * time.Second
)

// gracefulShutdownTimeout lets an in-flight MCP tool call finish within its
// configured deadline before the HTTP server stops waiting for it. Process
// supervisors must allow more than this before SIGKILL: deploy/ uses 50 s for
// the default 30 s tool deadline (see docs/configuration.md).
func gracefulShutdownTimeout(toolCallTimeout time.Duration) time.Duration {
	return max(minGracefulShutdownTimeout, toolCallTimeout+gracefulShutdownMargin)
}

// The native client view and MCP tools are two interfaces of the same OAuth
// resource. Tokens retain their /mcp audience; reading this view does not grant
// the independent administrative capabilities.
func mountClientAPI(mux *http.ServeMux, backend interface {
	storeport.IdentityStore
	storeport.TenantReadTxStore
}, logger *slog.Logger, baseURL string, guards mcpHTTPGuards) {
	mux.Handle("/api/client/v1/", clientapi.NoStore(administrativeHandler(
		guards.IP, guards.Learner, guards.Concurrency,
		func(next http.Handler) http.Handler {
			if guards.NativeOnly {
				validator, ok := backend.(auth.NativePrincipalValidator)
				if !ok {
					panic("native client store does not validate key sessions")
				}
				return auth.BearerMiddlewareWithNativePrincipalValidator(baseURL, clientapi.RequiredScope, validator, next)
			}
			return auth.BearerMiddlewareWithPrincipalValidator(baseURL, clientapi.RequiredScope, backend, next)
		}, clientapi.New(backend, logger).Handler(),
	)))
}

// rotatesSecretsAtStartup reports whether this process re-encrypts stored
// integration and narrative secrets with the current key. Only the API (or
// the single development process) does: the institutional worker's
// least-privilege role can read what it delivers but deliberately cannot
// rewrite narrative objects (deploy/postgres-roles.sql). Rotation is
// idempotent and compare-and-swap, so several API instances may run it
// concurrently.
func rotatesSecretsAtStartup(processRole string) bool {
	return processRole != "worker"
}

// schedulerOptions carries the scheduler settings parsed by loadStartupConfig.
func schedulerOptions(cfg startupConfig) engine.SchedulerOptions {
	return engine.SchedulerOptions{ConsolidationDeliveryTimeout: cfg.ConsolidationDeliveryTimeout}
}

func startScheduler(store *db.Store, logger *slog.Logger, mode string, options engine.SchedulerOptions) (*engine.Scheduler, error) {
	scheduler := engine.NewScheduler(store, logger)
	if mode == "distributed" {
		scheduler = engine.NewDistributedScheduler(store, logger)
	}
	scheduler.Configure(options)
	if err := scheduler.Start(); err != nil {
		return nil, reportError(logger, "failed to start worker scheduler", err, "mode", mode)
	}
	logger.Info("worker scheduler ready", "mode", mode)
	return scheduler, nil
}

func runWorkerUntilSignal(store *db.Store, logger *slog.Logger, mode string, options engine.SchedulerOptions) error {
	if mode != "distributed" {
		return reportError(logger, "dedicated tenant worker requires distributed scheduler mode", nil)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	scheduler := engine.NewTenantDistributedScheduler(store, logger, "saas_worker")
	scheduler.Configure(options)
	if err := scheduler.Start(); err != nil {
		return reportError(logger, "failed to start tenant worker scheduler", err)
	}
	logger.Info("tenant worker scheduler ready")
	sig := <-sigCh
	logger.Info("worker drain requested", "signal", sig.String())
	scheduler.Stop()
	logger.Info("worker drained")
	return nil
}

// dcrRouteLimiters are the per-IP budgets of POST /register. Admission must be
// process-local: it bounds anonymous requests before the IAT lookup. Requests
// and Daily run after admission and may use the shared backend.
type dcrRouteLimiters struct {
	Admission *auth.RateLimiter
	Requests  *auth.RateLimiter
	Daily     *auth.RateLimiter
}

func mountDynamicClientRegistration(mux *http.ServeMux, mode string, oauthServer *auth.OAuthServer, limiters dcrRouteLimiters) {
	if mode == auth.DCRModeDisabled {
		return
	}
	// IAT admission deliberately wraps the shared limiters: an anonymous
	// invalid request must not mutate the shared PostgreSQL rate-limit backend.
	mux.Handle("POST /register", auth.RateLimitMiddleware(limiters.Admission,
		oauthServer.DynamicClientRegistrationAdmission(
			auth.RateLimitMiddleware(limiters.Requests,
				auth.RateLimitMiddleware(limiters.Daily, http.HandlerFunc(oauthServer.HandleRegister)),
			),
		),
	))
}

var pseudonymizedLogKeys = map[string]struct{}{
	"learner": {}, "learner_id": {}, "email": {},
	"session": {}, "session_id": {},
	"domain": {}, "domain_id": {}, "concept": {},
	"assessment_attempt": {}, "evaluator_id": {}, "prediction_id": {},
	"redirect_uri": {}, "webhook_url": {}, "url": {}, "path": {},
	"dsn": {}, "database_url": {}, "authorization": {}, "token": {}, "secret": {},
	"stack": {}, "panic": {}, "panic_value": {},
	"ua": {}, "client": {}, "client_id": {}, "client_name": {}, "domain_name": {},
}

// newPrivacySafeLogAttr creates process-local, keyed pseudonyms for structured
// learning identifiers. The random key is intentionally not persisted: logs
// remain correlatable during one process lifetime without exposing stable
// cross-restart identifiers or dictionary-hashable concept labels.
func newPrivacySafeLogAttr() func([]string, slog.Attr) slog.Attr {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		key = nil
	}
	return func(_ []string, attr slog.Attr) slog.Attr {
		if strings.EqualFold(attr.Key, "err") || strings.EqualFold(attr.Key, "error") {
			// Never ask an error or Stringer for its text: network errors can
			// embed a credential-bearing URL, database errors can embed a DSN,
			// and filesystem errors can embed an absolute path. The dynamic type
			// is stable enough to route diagnostics without disclosing values.
			return slog.String(attr.Key, "class:"+safeLogValueClass(attr.Value))
		}
		if _, sensitive := pseudonymizedLogKeys[strings.ToLower(attr.Key)]; !sensitive {
			return attr
		}
		if attr.Value.Kind() != slog.KindString || attr.Value.String() == "" || len(key) == 0 {
			return slog.String(attr.Key, "[redacted]")
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(attr.Value.String()))
		return slog.String(attr.Key, "anon:"+hex.EncodeToString(mac.Sum(nil)[:8]))
	}
}

func safeLogValueClass(value slog.Value) string {
	if value.Kind() == slog.KindAny {
		return fmt.Sprintf("%T", value.Any())
	}
	return value.Kind().String()
}

func sanitizeHTTPLogField(value string) string {
	value = strings.ReplaceAll(value, "\n", "")
	return strings.ReplaceAll(value, "\r", "")
}

func shouldPrintVersion(args []string) bool {
	return len(args) == 1 && (args[0] == "--version" || args[0] == "-version" || args[0] == "version")
}

func versionLine() string {
	return fmt.Sprintf("tutor-mcp %s (commit %s, built %s)", version, commit, buildDate)
}

func mcpVersion() string {
	out := strings.TrimSpace(version)
	out = strings.TrimPrefix(out, "v")
	if out == "" {
		return "dev"
	}
	return out
}

type readinessPinger interface {
	Ping(context.Context) error
}

type schemaCompatibilityVerifier interface {
	VerifySchemaCurrent(context.Context) error
}

func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"live"}`))
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController discover optional capabilities on the
// real net/http writer, including per-response deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush preserves http.Flusher for the MCP SDK, which checks the interface
// directly before emitting SSE events.
func (r *statusRecorder) Flush() {
	_ = r.FlushError()
}

func (r *statusRecorder) FlushError() error {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// withoutWriteTimeout is scoped to /mcp. Read/header/idle timeouts and the
// global write timeout remain defensive defaults for every non-stream route.
func withoutWriteTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http installs Server.WriteTimeout before entering the handler.
		// A zero deadline disables it for this long-lived stream only.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
}

// mcpRequestBodyLimitMiddleware bounds POST bodies before they reach the MCP
// SDK. Pre-reading is intentional: the SDK may translate a MaxBytesReader error
// into a generic 400, whereas this layer must reliably return 413 for both a
// declared Content-Length and an unknown-length/chunked body.
func mcpRequestBodyLimitMiddleware(maxBytes int64, next http.Handler) http.Handler {
	if maxBytes <= 0 {
		panic("MCP request body limit must be positive")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > maxBytes {
			_ = r.Body.Close()
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		limitedBody := http.MaxBytesReader(w, r.Body, maxBytes)
		body, err := io.ReadAll(limitedBody)
		_ = limitedBody.Close()
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, tools.WithBoundedMCPRequestBody(r, body))
	})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(observability.WithRequestID(r.Context()))
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		attributes := []any{
			"request_id", observability.RequestID(r.Context()),
			"method", sanitizeHTTPLogField(r.Method),
			"path", sanitizeHTTPLogField(r.URL.Path),
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ua", sanitizeHTTPLogField(r.UserAgent()),
		}
		if traceID := observability.TraceID(r.Context()); traceID != "" {
			attributes = append(attributes, "trace_id", sanitizeHTTPLogField(traceID))
		}
		logger.Info("request", attributes...)
	})
}

const defaultMCPAllowedRequestHeaders = "Accept, Content-Type, Authorization, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID, Mcp-Method, Mcp-Name"

func corsMiddleware(allowedOrigins []string, allowedSuffixes []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool)
	for _, o := range allowedOrigins {
		allowed[o] = true
	}
	originAllowed := func(origin string) bool {
		if allowed[origin] {
			return true
		}
		for _, s := range allowedSuffixes {
			if strings.HasSuffix(origin, s) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		if originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if headers, ok := allowedMCPRequestHeaders(r.Header.Get("Access-Control-Request-Headers")); ok {
				w.Header().Set("Access-Control-Allow-Headers", headers)
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		if r.Method == "OPTIONS" {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedMCPRequestHeaders validates a browser preflight before reflecting it.
// MCP 2026-07-28 adds Mcp-Method/Mcp-Name and optional, schema-driven
// Mcp-Param-* headers, so a fixed allow-list alone cannot be protocol-complete.
func allowedMCPRequestHeaders(requested string) (string, bool) {
	if strings.TrimSpace(requested) == "" {
		return defaultMCPAllowedRequestHeaders, true
	}
	allowed := map[string]bool{
		"accept":               true,
		"authorization":        true,
		"content-type":         true,
		"last-event-id":        true,
		"mcp-method":           true,
		"mcp-name":             true,
		"mcp-protocol-version": true,
		"mcp-session-id":       true,
	}
	seen := make(map[string]bool)
	headers := make([]string, 0, 8)
	for _, value := range strings.Split(requested, ",") {
		header := strings.TrimSpace(value)
		normalized := strings.ToLower(header)
		if !allowed[normalized] && !(strings.HasPrefix(normalized, "mcp-param-") && len(normalized) > len("mcp-param-") && validHTTPHeaderName(header)) {
			return "", false
		}
		if !seen[normalized] {
			seen[normalized] = true
			headers = append(headers, header)
		}
	}
	if len(headers) == 0 {
		return "", false
	}
	return strings.Join(headers, ", "), true
}

func validHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// securityHeaders applies baseline browser-security headers on every response.
// HSTS is only emitted when BASE_URL is HTTPS so local HTTP development isn't pinned.
func securityHeaders(baseURL string, next http.Handler) http.Handler {
	hsts := strings.HasPrefix(baseURL, "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if hsts {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware turns a panic in any downstream handler into a 500
// instead of taking the whole process down. Only keyed fingerprints of the
// path and stack are logged; neither raw value is returned or persisted.
func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stackDigest := sha256.Sum256(debug.Stack())
				pathDigest := sha256.Sum256([]byte(r.URL.EscapedPath()))
				logger.Error("panic recovered",
					"component", "http_recovery",
					"path_fingerprint", hex.EncodeToString(pathDigest[:8]),
					"method", r.Method,
					"panic_type", fmt.Sprintf("%T", rec),
					"stack_fingerprint", hex.EncodeToString(stackDigest[:8]),
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"internal_error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
