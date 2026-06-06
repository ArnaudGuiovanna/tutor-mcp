// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// AI Learning MCP Server
package main

import (
	"context"
	"fmt"
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
		Level: parseLogLevel(os.Getenv("LOG_LEVEL")),
	}))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/runtime.db"
	}
	dbDriver := os.Getenv("DB_DRIVER")
	if dbDriver == "" {
		dbDriver = "sqlite"
	}
	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://localhost:%s", port)
	}

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
		os.MkdirAll("data", 0755)
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

	// Create MCP server
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "tutor-mcp",
		Version: mcpVersion(),
	}, nil)

	// Register tools
	deps := &tools.Deps{Store: store, Logger: logger, BaseURL: baseURL}
	tools.RegisterTools(mcpServer, deps)

	// Create MCP handler — disable localhost protection (server is reached via a public reverse proxy)
	// and allow Claude.ai cross-origin requests
	cop := http.NewCrossOriginProtection()
	cop.AddTrustedOrigin("https://claude.ai")
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection: true,
		CrossOriginProtection:      cop,
	})

	// OAuth server
	oauthServer := auth.NewOAuthServer(store, baseURL, logger)

	// HTTP mux
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := store.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unhealthy","error":"database unreachable"}`))
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Rate limiters (in-process). Warn loudly if the deployment looks public but
	// TRUSTED_PROXY_CIDRS is unset - without it every IP-limited request shares
	// one bucket under the proxy's loopback IP (issue #37).
	auth.WarnRateLimiterMisconfig(baseURL)
	authLimiter := auth.NewRateLimiter(10.0/60, 10)   // 10/min for auth endpoints
	registerLimiter := auth.NewRateLimiter(5.0/60, 5) // 5/min for client registration
	mcpRatePerMinute := envFloat("MCP_RATE_LIMIT_PER_MIN", 60)
	mcpBurst := envInt("MCP_RATE_LIMIT_BURST", 60)
	mcpIPLimiter := auth.NewRateLimiter(mcpRatePerMinute/60, mcpBurst)
	mcpLearnerLimiter := auth.NewRateLimiter(mcpRatePerMinute/60, mcpBurst)

	// RATELIMIT_BACKEND=postgres (alias "db") opts every rate limiter and the
	// per-account login-failure tracker into a shared, DB-backed store so a
	// multi-instance fleet enforces one combined view of throttling/lockout
	// instead of independent per-process counters. Default "memory" leaves the
	// in-process behaviour byte-for-byte unchanged (single-node deployments).
	switch os.Getenv("RATELIMIT_BACKEND") {
	case "postgres", "db":
		rlBackend := db.NewRateLimitBackend(store)
		authLimiter.SetBackend(rlBackend)
		registerLimiter.SetBackend(rlBackend)
		mcpIPLimiter.SetBackend(rlBackend)
		mcpLearnerLimiter.SetBackend(rlBackend)
		oauthServer.SetLoginFailureBackend(db.NewLoginFailureBackend(store))
		logger.Info("rate limit backend", "mode", "postgres (shared, fleet-wide)")
	case "", "memory":
		// In-process default; no shared state.
	default:
		logger.Warn("unknown RATELIMIT_BACKEND, using memory", "value", os.Getenv("RATELIMIT_BACKEND"))
	}

	defer authLimiter.Stop()
	defer registerLimiter.Stop()
	defer mcpIPLimiter.Stop()
	defer mcpLearnerLimiter.Stop()

	// OAuth routes — rate-limit sensitive endpoints
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", oauthServer.HandleAuthServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", oauthServer.HandleProtectedResourceMetadata)
	mux.Handle("GET /authorize", auth.RateLimitMiddleware(authLimiter, http.HandlerFunc(oauthServer.HandleAuthorizeGet)))
	mux.Handle("POST /authorize", auth.RateLimitMiddleware(authLimiter, http.HandlerFunc(oauthServer.HandleAuthorizePost)))
	mux.Handle("POST /token", auth.RateLimitMiddleware(authLimiter, http.HandlerFunc(oauthServer.HandleToken)))
	mux.Handle("POST /register", auth.RateLimitMiddleware(registerLimiter, http.HandlerFunc(oauthServer.HandleRegister)))

	// MCP route: per-IP shield before auth, then per-learner limiting after auth.
	mcpProtectedHandler := auth.RateLimitMiddleware(
		mcpIPLimiter,
		auth.BearerMiddleware(baseURL,
			auth.LearnerRateLimitMiddleware(mcpLearnerLimiter, mcpHandler),
		),
	)
	mux.Handle("/mcp", mcpProtectedHandler)

	// Start scheduler. SCHEDULER_MODE=distributed enables fleet-wide
	// exactly-once job execution via DB leases; default "inprocess" keeps
	// the single-node behaviour where every cron tick runs locally.
	scheduler := engine.NewScheduler(store, logger)
	if os.Getenv("SCHEDULER_MODE") == "distributed" {
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

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
		if originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
// instead of taking the whole process down. The stack trace is logged, never
// returned to the client.
func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"path", r.URL.Path,
					"method", r.Method,
					"panic", fmt.Sprintf("%v", rec),
					"stack", string(debug.Stack()),
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
