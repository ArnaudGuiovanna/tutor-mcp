// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/db"
)

const (
	defaultMCPMaxRequestBodyBytes int64 = 1 << 20 // 1 MiB
	maxMCPRequestBodyBytes        int64 = 64 << 20
	defaultMCPToolCallTimeout           = 30 * time.Second
	maxMCPToolCallTimeoutSeconds  int64 = 600
)

type startupConfig struct {
	DeploymentProfile      string
	DBDriver               string
	DBPath                 string
	BaseURL                string
	SchedulerMode          string
	RateLimitBackend       string
	MCPMaxRequestBodyBytes int64
	MCPMaxConcurrent       int
	MCPMaxConcurrentUser   int
	MCPToolCallTimeout     time.Duration

	// OAuthGranularScopes gates publication and issuance of per-tool
	// learner:read / learner:write grants. It defaults to false so a schema
	// expansion can be deployed across the fleet before behavior changes.
	// main passes this value to the OAuth and MCP configuration boundaries.
	OAuthGranularScopes bool
}

func loadStartupConfig(port string) (startupConfig, error) {
	cfg := startupConfig{
		DeploymentProfile: normalizedEnv("DEPLOYMENT_PROFILE", "development"),
		DBDriver:          normalizedEnv("DB_DRIVER", "sqlite"),
		DBPath:            strings.TrimSpace(os.Getenv("DB_PATH")),
		SchedulerMode:     normalizedEnv("SCHEDULER_MODE", "inprocess"),
		RateLimitBackend:  normalizedEnv("RATELIMIT_BACKEND", "memory"),
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/runtime.db"
	}
	oauthGranularScopes, err := onOffEnv("OAUTH_GRANULAR_SCOPES", false)
	if err != nil {
		return startupConfig{}, err
	}
	cfg.OAuthGranularScopes = oauthGranularScopes
	mcpBodyLimit, err := int64EnvInRange(
		"MCP_MAX_REQUEST_BODY_BYTES",
		defaultMCPMaxRequestBodyBytes,
		1,
		maxMCPRequestBodyBytes,
	)
	if err != nil {
		return startupConfig{}, err
	}
	cfg.MCPMaxRequestBodyBytes = mcpBodyLimit
	mcpMaxConcurrent, err := int64EnvInRange("MCP_MAX_CONCURRENT", 128, 1, 4096)
	if err != nil {
		return startupConfig{}, err
	}
	mcpMaxConcurrentUser, err := int64EnvInRange("MCP_MAX_CONCURRENT_PER_LEARNER", 8, 1, 4096)
	if err != nil {
		return startupConfig{}, err
	}
	if mcpMaxConcurrentUser > mcpMaxConcurrent {
		return startupConfig{}, fmt.Errorf("MCP_MAX_CONCURRENT_PER_LEARNER must not exceed MCP_MAX_CONCURRENT")
	}
	cfg.MCPMaxConcurrent = int(mcpMaxConcurrent)
	cfg.MCPMaxConcurrentUser = int(mcpMaxConcurrentUser)
	mcpToolCallTimeoutSeconds, err := int64EnvInRange(
		"MCP_TOOL_CALL_TIMEOUT_SECONDS",
		int64(defaultMCPToolCallTimeout/time.Second),
		1,
		maxMCPToolCallTimeoutSeconds,
	)
	if err != nil {
		return startupConfig{}, err
	}
	cfg.MCPToolCallTimeout = time.Duration(mcpToolCallTimeoutSeconds) * time.Second

	switch cfg.DBDriver {
	case "sqlite", "postgres":
	default:
		return startupConfig{}, fmt.Errorf("unknown DB_DRIVER %q (want sqlite or postgres)", cfg.DBDriver)
	}
	switch cfg.DeploymentProfile {
	case "development", "production":
	default:
		return startupConfig{}, fmt.Errorf("unknown DEPLOYMENT_PROFILE %q (want development or production)", cfg.DeploymentProfile)
	}

	switch cfg.SchedulerMode {
	case "inprocess", "distributed":
	default:
		return startupConfig{}, fmt.Errorf("unknown SCHEDULER_MODE %q (want inprocess or distributed)", cfg.SchedulerMode)
	}

	if cfg.RateLimitBackend == "db" {
		cfg.RateLimitBackend = "postgres"
	}
	switch cfg.RateLimitBackend {
	case "memory", "postgres":
	default:
		return startupConfig{}, fmt.Errorf("unknown RATELIMIT_BACKEND %q (want memory or postgres)", cfg.RateLimitBackend)
	}

	rawBaseURL := strings.TrimSpace(os.Getenv("BASE_URL"))
	if rawBaseURL == "" {
		rawBaseURL = fmt.Sprintf("http://localhost:%s", port)
	}
	baseURL, err := normalizeBaseURL(rawBaseURL)
	if err != nil {
		return startupConfig{}, err
	}
	cfg.BaseURL = baseURL

	if cfg.RateLimitBackend == "postgres" && cfg.DBDriver != "postgres" {
		return startupConfig{}, fmt.Errorf("RATELIMIT_BACKEND=postgres requires DB_DRIVER=postgres")
	}
	if cfg.SchedulerMode == "distributed" {
		if cfg.DBDriver != "postgres" {
			return startupConfig{}, fmt.Errorf("SCHEDULER_MODE=distributed requires DB_DRIVER=postgres")
		}
		if cfg.RateLimitBackend != "postgres" {
			return startupConfig{}, fmt.Errorf("SCHEDULER_MODE=distributed requires RATELIMIT_BACKEND=postgres")
		}
		if !memoryExplicitlyDisabled(os.Getenv("TUTOR_MCP_MEMORY_ENABLED")) {
			return startupConfig{}, fmt.Errorf("SCHEDULER_MODE=distributed requires TUTOR_MCP_MEMORY_ENABLED=off because narrative memory is node-local")
		}
	}
	if cfg.DeploymentProfile == "production" {
		if err := validateProductionConfig(cfg); err != nil {
			return startupConfig{}, err
		}
	}

	return cfg, nil
}

func validateProductionConfig(cfg startupConfig) error {
	if !strings.HasPrefix(cfg.BaseURL, "https://") {
		return fmt.Errorf("production requires an HTTPS BASE_URL")
	}
	if cfg.DBDriver != "postgres" {
		return fmt.Errorf("production requires DB_DRIVER=postgres")
	}
	if cfg.RateLimitBackend != "postgres" {
		return fmt.Errorf("production requires RATELIMIT_BACKEND=postgres")
	}
	if err := validateProductionPostgresDSN(os.Getenv("DATABASE_URL")); err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv("SMTP_ADDR")) == "" || strings.TrimSpace(os.Getenv("SMTP_FROM")) == "" {
		return fmt.Errorf("production requires SMTP_ADDR and SMTP_FROM")
	}
	if strings.TrimSpace(os.Getenv("INTEGRATION_SECRET_KEYS")) == "" ||
		strings.TrimSpace(os.Getenv("INTEGRATION_SECRET_CURRENT_KEY_ID")) == "" {
		return fmt.Errorf("production requires integration secret encryption keys")
	}
	if err := validateTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS")); err != nil {
		return fmt.Errorf("production TRUSTED_PROXY_CIDRS: %w", err)
	}
	return nil
}

func validateProductionPostgresDSN(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("production requires DATABASE_URL")
	}
	dsn, err := url.Parse(raw)
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") || dsn.Hostname() == "" {
		return fmt.Errorf("production DATABASE_URL must be a postgres URL")
	}
	if dsn.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("production DATABASE_URL requires sslmode=verify-full")
	}
	if strings.TrimSpace(dsn.Query().Get("sslrootcert")) == "" {
		return fmt.Errorf("production DATABASE_URL requires an explicit sslrootcert CA")
	}
	return nil
}

func validateTrustedProxyCIDRs(raw string) error {
	parts := strings.Split(raw, ",")
	valid := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			return fmt.Errorf("invalid CIDR %q", part)
		}
		if ones, _ := cidr.Mask.Size(); ones == 0 {
			return fmt.Errorf("catch-all CIDR %q is unsafe", part)
		}
		valid++
	}
	if valid == 0 {
		return fmt.Errorf("at least one trusted reverse-proxy CIDR is required")
	}
	return nil
}

func normalizedEnv(name, fallback string) string {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value
}

// onOffEnv parses an operator-facing feature switch. Unknown values fail at
// boot instead of silently selecting a security-sensitive OAuth mode.
func onOffEnv(name string, fallback bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return fallback, nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be on or off", name)
	}
}

func int64EnvInRange(name string, fallback, min, max int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer between %d and %d: %w", name, min, max, err)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid BASE_URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("invalid BASE_URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("invalid BASE_URL %q: host is required", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid BASE_URL %q: user information is not allowed", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", fmt.Errorf("invalid BASE_URL %q: query and fragment are not allowed", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("invalid BASE_URL %q: path is not allowed", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func memoryExplicitlyDisabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

func ensureSQLiteParent(dbPath string) error {
	return db.PreparePrivateSQLitePath(filepath.Clean(dbPath))
}
