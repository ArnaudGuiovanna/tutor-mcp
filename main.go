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
	"encoding/hex"
	"errors"
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

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:       parseLogLevel(os.Getenv("LOG_LEVEL")),
		ReplaceAttr: newPrivacySafeLogAttr(),
	}))
	// Packages using the package-level slog helpers must inherit the same
	// privacy and level policy as explicitly injected components.
	slog.SetDefault(logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	cfg, err := loadStartupConfig(port)
	if err != nil {
		logger.Error("invalid startup configuration", "err", err)
		os.Exit(1)
	}
	dbPath := cfg.DBPath
	dbDriver := cfg.DBDriver
	baseURL := cfg.BaseURL
	memoryLimits, err := memory.LimitsFromEnv()
	if err != nil {
		logger.Error("invalid narrative memory limits", "err", err)
		os.Exit(1)
	}
	if err := memory.ConfigureLimits(memoryLimits); err != nil {
		logger.Error("configure narrative memory limits", "err", err)
		os.Exit(1)
	}
	logger.Info("narrative memory limits",
		"max_write_bytes", memoryLimits.MaxWriteBytes,
		"max_file_bytes", memoryLimits.MaxFileBytes,
		"max_learner_bytes", memoryLimits.MaxLearnerBytes,
		"max_files_per_learner", memoryLimits.MaxFilesPerLearner,
		"max_concurrent_writes", memoryLimits.MaxConcurrentWrites,
	)

	// Init JWT
	if err := auth.LoadJWTSecret(); err != nil {
		logger.Error("failed to load JWT secret", "err", err)
		os.Exit(1)
	}

	// Open the store. Default: single-node SQLite (no external deps). Set
	// DB_DRIVER=postgres + DATABASE_URL for horizontal Postgres deployments.
	var store *db.Store
	switch dbDriver {
	case "postgres":
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			logger.Error("DB_DRIVER=postgres requires DATABASE_URL")
			os.Exit(1)
		}
		database, err := db.OpenPostgres(dsn, envInt("DB_MAX_CONNS", 10))
		if err != nil {
			logger.Error("failed to open postgres", "err", err)
			os.Exit(1)
		}
		defer database.Close()
		if err := db.MigratePostgres(context.Background(), database); err != nil {
			logger.Error("failed to migrate postgres", "err", err)
			os.Exit(1)
		}
		store = db.NewStoreWithDialect(database, db.DialectPostgres)
		logger.Info("database ready", "driver", "postgres")
	case "sqlite":
		if err := ensureSQLiteParent(dbPath); err != nil {
			logger.Error("failed to prepare sqlite directory", "err", err)
			os.Exit(1)
		}
		database, err := db.OpenDB(dbPath)
		if err != nil {
			logger.Error("failed to open database", "err", err)
			os.Exit(1)
		}
		defer database.Close()
		if err := db.Migrate(database); err != nil {
			logger.Error("failed to migrate database", "err", err)
			os.Exit(1)
		}
		store = db.NewStore(database)
		logger.Info("database ready", "driver", "sqlite", "path", dbPath)
	default:
		logger.Error("unknown DB_DRIVER (want sqlite|postgres)", "driver", dbDriver)
		os.Exit(1)
	}
	if encodedKeys := strings.TrimSpace(os.Getenv("INTEGRATION_SECRET_KEYS")); encodedKeys != "" {
		keyring, err := db.NewIntegrationSecretKeyring(encodedKeys, os.Getenv("INTEGRATION_SECRET_CURRENT_KEY_ID"))
		if err != nil {
			logger.Error("invalid integration secret keyring", "err", err)
			os.Exit(1)
		}
		store.SetIntegrationSecretKeyring(keyring)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rotated, err := store.RotateIntegrationSecrets(ctx)
		cancel()
		if err != nil {
			logger.Error("failed to rotate integration secrets", "err", err)
			os.Exit(1)
		}
		logger.Info("integration secret encryption enabled", "rotated_records", rotated)
	} else {
		logger.Warn("integration secret encryption disabled; webhook credentials remain plaintext")
	}

	// Create MCP server
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "tutor-mcp",
		Version: mcpVersion(),
	}, nil)

	// Register tools
	deps := &tools.Deps{
		Store:               store,
		Logger:              logger,
		BaseURL:             baseURL,
		OAuthGranularScopes: cfg.OAuthGranularScopes,
	}
	tools.RegisterTools(mcpServer, deps)
	// Install the deadline after tool registration so it wraps the idempotency
	// middleware too: reservation, handler execution and replay finalization all
	// share the same bounded context. It applies only to tools/call, never to
	// initialization, discovery, prompts, or long-lived transport responses.
	mcpServer.AddReceivingMiddleware(mcpToolCallDeadlineMiddleware(cfg.MCPToolCallTimeout))

	// Create MCP handler — disable localhost protection (server is reached via a public reverse proxy)
	// and allow Claude.ai cross-origin requests
	cop := http.NewCrossOriginProtection()
	cop.AddTrustedOrigin("https://claude.ai")
	rawMCPHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true,
		MaxRequestBodyBytes:          cfg.MCPMaxRequestBodyBytes,
		PropagateRequestCancellation: true,
	})
	// Reject untrusted cross-origin POSTs before reading their bodies. Trusted
	// requests are then bounded once, inspected for a per-tool OAuth step-up,
	// and finally delegated to the SDK with the same bytes.
	var mcpHandler http.Handler = cop.Handler(
		mcpRequestBodyLimitMiddleware(cfg.MCPMaxRequestBodyBytes,
			tools.OAuthScopeHTTPMiddleware(
				baseURL, cfg.MCPMaxRequestBodyBytes, cfg.OAuthGranularScopes, rawMCPHandler,
			),
		),
	)
	logger.Info("MCP transport limits",
		"mode", "stateless",
		"max_request_body_bytes", cfg.MCPMaxRequestBodyBytes,
		"max_concurrent", cfg.MCPMaxConcurrent,
		"max_concurrent_per_learner", cfg.MCPMaxConcurrentUser,
		"tool_call_timeout", cfg.MCPToolCallTimeout,
	)
	mcpConcurrencyLimiter, err := auth.NewPrincipalConcurrencyLimiter(cfg.MCPMaxConcurrent, cfg.MCPMaxConcurrentUser)
	if err != nil {
		logger.Error("invalid MCP concurrency limits", "err", err)
		os.Exit(1)
	}

	// OAuth server
	oauthServer := auth.NewOAuthServer(store, baseURL, logger)
	oauthServer.SetGranularScopesEnabled(cfg.OAuthGranularScopes)
	if smtpAddress := strings.TrimSpace(os.Getenv("SMTP_ADDR")); smtpAddress != "" {
		emailSender, err := auth.NewSMTPEmailSender(auth.SMTPConfig{
			Address:    smtpAddress,
			ServerName: strings.TrimSpace(os.Getenv("SMTP_SERVER_NAME")),
			From:       strings.TrimSpace(os.Getenv("SMTP_FROM")),
			Username:   os.Getenv("SMTP_USERNAME"),
			Password:   os.Getenv("SMTP_PASSWORD"),
		})
		if err != nil {
			logger.Error("invalid SMTP configuration", "err", err)
			os.Exit(1)
		}
		oauthServer.SetEmailSender(emailSender)
		logger.Info("account email delivery enabled", "transport", "smtp_starttls")
	} else {
		logger.Warn("account email delivery disabled; public registration and password recovery cannot complete")
	}

	// HTTP mux
	mux := http.NewServeMux()

	// Liveness never depends on an external service. Readiness is bounded and
	// reflects whether this instance can safely receive traffic. /health remains
	// a backwards-compatible alias for readiness.
	mux.HandleFunc("GET /live", livenessHandler)
	mux.HandleFunc("GET /ready", readinessHandler(store))
	mux.HandleFunc("GET /health", readinessHandler(store))

	// Rate limiters (in-process). Warn loudly if the deployment looks public but
	// TRUSTED_PROXY_CIDRS is unset - without it every IP-limited request shares
	// one bucket under the proxy's loopback IP (issue #37).
	auth.WarnRateLimiterMisconfig(baseURL)
	authorizeLimiter := auth.NewRateLimiterWithNamespace("oauth_authorize", 30.0/60, 10) // consent/login page loads
	loginLimiter := auth.NewRateLimiterWithNamespace("oauth_login", 5.0/60, 5)           // password verification
	tokenLimiter := auth.NewRateLimiterWithNamespace("oauth_token", 20.0/60, 10)         // code exchange and refresh
	registerLimiter := auth.NewRateLimiterWithNamespace("oauth_register", 5.0/60, 5)     // dynamic client registration
	accountLimiter := auth.NewRateLimiterWithNamespace("oauth_account", 5.0/60, 5)       // verification and recovery
	mcpRatePerMinute := envFloat("MCP_RATE_LIMIT_PER_MIN", 60)
	mcpBurst := envInt("MCP_RATE_LIMIT_BURST", 60)
	mcpIPLimiter := auth.NewRateLimiterWithNamespace("mcp_ip", mcpRatePerMinute/60, mcpBurst)
	mcpLearnerLimiter := auth.NewRateLimiterWithNamespace("mcp_learner", mcpRatePerMinute/60, mcpBurst)

	// RATELIMIT_BACKEND=postgres (alias "db") opts every rate limiter and the
	// per-account login-failure tracker into a shared, DB-backed store so a
	// multi-instance fleet enforces one combined view of throttling/lockout
	// instead of independent per-process counters. Default "memory" leaves the
	// in-process behaviour byte-for-byte unchanged (single-node deployments).
	switch cfg.RateLimitBackend {
	case "postgres":
		rlBackend := db.NewRateLimitBackend(store)
		authorizeLimiter.SetBackend(rlBackend)
		loginLimiter.SetBackend(rlBackend)
		tokenLimiter.SetBackend(rlBackend)
		registerLimiter.SetBackend(rlBackend)
		accountLimiter.SetBackend(rlBackend)
		mcpIPLimiter.SetBackend(rlBackend)
		mcpLearnerLimiter.SetBackend(rlBackend)
		oauthServer.SetLoginFailureBackend(db.NewLoginFailureBackend(store))
		logger.Info("rate limit backend", "mode", "postgres (shared, fleet-wide)")
	case "", "memory":
		// In-process default; no shared state.
	}

	defer authorizeLimiter.Stop()
	defer loginLimiter.Stop()
	defer tokenLimiter.Stop()
	defer registerLimiter.Stop()
	defer accountLimiter.Stop()
	defer mcpIPLimiter.Stop()
	defer mcpLearnerLimiter.Stop()

	// OAuth routes — rate-limit sensitive endpoints
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthServer.HandleAuthServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauthServer.HandleProtectedResourceMetadata)
	mux.Handle("GET /authorize", auth.RateLimitMiddleware(authorizeLimiter, http.HandlerFunc(oauthServer.HandleAuthorizeGet)))
	mux.Handle("POST /authorize", auth.RateLimitMiddleware(loginLimiter, http.HandlerFunc(oauthServer.HandleAuthorizePost)))
	mux.Handle("POST /token", auth.RateLimitMiddleware(tokenLimiter, http.HandlerFunc(oauthServer.HandleToken)))
	mux.Handle("POST /register", auth.RateLimitMiddleware(registerLimiter, http.HandlerFunc(oauthServer.HandleRegister)))
	mux.Handle("GET /verify-email", auth.RateLimitMiddleware(authorizeLimiter, http.HandlerFunc(oauthServer.HandleVerifyEmailGet)))
	mux.Handle("POST /verify-email", auth.RateLimitMiddleware(accountLimiter, http.HandlerFunc(oauthServer.HandleVerifyEmailPost)))
	mux.Handle("GET /recover", auth.RateLimitMiddleware(authorizeLimiter, http.HandlerFunc(oauthServer.HandleRecoverGet)))
	mux.Handle("POST /recover", auth.RateLimitMiddleware(accountLimiter, http.HandlerFunc(oauthServer.HandleRecoverPost)))
	mux.Handle("GET /reset-password", auth.RateLimitMiddleware(authorizeLimiter, http.HandlerFunc(oauthServer.HandleResetPasswordGet)))
	mux.Handle("POST /reset-password", auth.RateLimitMiddleware(accountLimiter, http.HandlerFunc(oauthServer.HandleResetPasswordPost)))

	// MCP route: per-IP shield before auth, then per-learner limiting after auth.
	// The body limit runs after both guards so rejected callers cannot make the
	// process buffer request bodies, while valid POSTs are bounded before the SDK.
	initialOAuthScope := models.OAuthScopeLearner
	if cfg.OAuthGranularScopes {
		initialOAuthScope = models.OAuthScopeLearnerRead
	}
	mcpProtectedHandler := auth.RateLimitMiddleware(
		mcpIPLimiter,
		auth.BearerMiddlewareWithScopeHint(baseURL, initialOAuthScope,
			auth.LearnerRateLimitMiddleware(mcpLearnerLimiter,
				mcpConcurrencyLimiter.Middleware(
					mcpHandler,
				),
			),
		),
	)
	// The server keeps a bounded WriteTimeout for ordinary HTTP endpoints, but
	// streamable MCP responses (notably SSE GETs) must outlive it. Clear only
	// this response's deadline through ResponseController; requestLogger's
	// writer exposes Unwrap so the controller can reach net/http's writer.
	mux.Handle("/mcp", withoutWriteTimeout(mcpProtectedHandler))

	// Start scheduler. SCHEDULER_MODE=distributed uses a DB lease so at most
	// one fleet instance wins each run slot; it is not crash-safe exactly-once
	// delivery. The default "inprocess" mode runs every cron tick locally.
	scheduler := engine.NewScheduler(store, logger)
	if cfg.SchedulerMode == "distributed" {
		scheduler = engine.NewDistributedScheduler(store, logger)
		logger.Info("scheduler mode", "mode", "distributed")
	}
	if err := scheduler.Start(); err != nil {
		logger.Error("failed to start scheduler", "err", err)
		os.Exit(1)
	}
	defer scheduler.Stop()

	// Wrap with recovery + request logging + security headers + CORS.
	// Order: recovery outermost so panics in any inner middleware are caught.
	// CORS: allow chat-side origins (claude.ai, baseURL) by exact match.
	handler := recoveryMiddleware(logger, requestLogger(logger, securityHeaders(baseURL, corsMiddleware(
		[]string{"https://claude.ai", baseURL},
		nil,
		mux,
	))))

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	shutdownErr := make(chan error, 1)
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("shutdown signal received", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		shutdownErr <- server.Shutdown(ctx)
	}()

	logger.Info("tutor mcp starting", "port", port, "base_url", baseURL)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server failed", "err", err)
		os.Exit(1)
	}
	if err := <-shutdownErr; err != nil {
		logger.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	logger.Info("server stopped cleanly")
}

var pseudonymizedLogKeys = map[string]struct{}{
	"learner": {}, "learner_id": {}, "email": {},
	"session": {}, "session_id": {},
	"domain": {}, "domain_id": {}, "concept": {},
	"assessment_attempt": {}, "evaluator_id": {}, "prediction_id": {},
	"redirect_uri": {}, "webhook_url": {}, "url": {}, "path": {},
	"dsn": {}, "database_url": {}, "authorization": {}, "token": {}, "secret": {},
	"stack": {}, "panic": {}, "panic_value": {},
	"ua": {}, "client_name": {}, "domain_name": {},
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

func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"live"}`))
}

func readinessHandler(pinger readinessPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pinger.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready","error":"database unreachable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
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

var errMCPToolCallDeadline = errors.New("MCP tool call deadline exceeded")

// mcpToolCallDeadlineMiddleware bounds cooperative tool execution without
// imposing a deadline on Streamable HTTP itself. Store calls receive this
// context and can cancel in-flight database work; a handler that ignores its
// context cannot be detached safely because it may still commit a mutation.
func mcpToolCallDeadlineMiddleware(timeout time.Duration) mcp.Middleware {
	if timeout <= 0 {
		panic("MCP tool call timeout must be positive")
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			deadlineCtx, cancel := context.WithTimeoutCause(ctx, timeout, errMCPToolCallDeadline)
			defer cancel()
			result, err := next(deadlineCtx, method, req)
			if errors.Is(context.Cause(deadlineCtx), errMCPToolCallDeadline) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "tool call exceeded the server deadline; retry a mutation only by reusing its original idempotency_key"}},
					StructuredContent: map[string]any{
						"error": "tool_call_timeout",
						"retry_with_original_idempotency_key_only": true,
					},
					IsError: true,
				}, nil
			}
			return result, err
		}
	}
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
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ua", r.UserAgent(),
		)
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
