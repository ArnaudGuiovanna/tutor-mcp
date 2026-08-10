// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadStartupConfigDefaults(t *testing.T) {
	clearStartupEnv(t)

	cfg, err := loadStartupConfig("3000")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBDriver != "sqlite" || cfg.DBPath != "./data/runtime.db" {
		t.Fatalf("unexpected database defaults: %+v", cfg)
	}
	if cfg.BaseURL != "http://localhost:3000" {
		t.Fatalf("BaseURL=%q", cfg.BaseURL)
	}
	if cfg.SchedulerMode != "inprocess" || cfg.RateLimitBackend != "memory" {
		t.Fatalf("unexpected runtime defaults: %+v", cfg)
	}
	if cfg.DeploymentProfile != "development" || cfg.MCPMaxConcurrent != 128 || cfg.MCPMaxConcurrentUser != 8 {
		t.Fatalf("unexpected safety defaults: %+v", cfg)
	}
	if cfg.MCPMaxRequestBodyBytes != 1<<20 {
		t.Fatalf("MCPMaxRequestBodyBytes=%d, want %d", cfg.MCPMaxRequestBodyBytes, 1<<20)
	}
	if cfg.MCPToolCallTimeout != 30*time.Second {
		t.Fatalf("MCPToolCallTimeout=%v, want 30s", cfg.MCPToolCallTimeout)
	}
}

func TestLoadStartupConfigMCPTransportOverrides(t *testing.T) {
	clearStartupEnv(t)
	t.Setenv("MCP_MAX_REQUEST_BODY_BYTES", "2097152")
	t.Setenv("MCP_MAX_CONCURRENT", "64")
	t.Setenv("MCP_MAX_CONCURRENT_PER_LEARNER", "4")
	t.Setenv("MCP_TOOL_CALL_TIMEOUT_SECONDS", "45")

	cfg, err := loadStartupConfig("3000")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPMaxRequestBodyBytes != 2<<20 {
		t.Fatalf("MCPMaxRequestBodyBytes=%d, want %d", cfg.MCPMaxRequestBodyBytes, 2<<20)
	}
	if cfg.MCPMaxConcurrent != 64 || cfg.MCPMaxConcurrentUser != 4 {
		t.Fatalf("concurrency limits: %+v", cfg)
	}
	if cfg.MCPToolCallTimeout != 45*time.Second {
		t.Fatalf("MCPToolCallTimeout=%v, want 45s", cfg.MCPToolCallTimeout)
	}
}

func TestLoadStartupConfigNormalizesOriginAndAliases(t *testing.T) {
	clearStartupEnv(t)
	t.Setenv("BASE_URL", "https://Tutor.Example:8443/")
	t.Setenv("DB_DRIVER", " POSTGRES ")
	t.Setenv("RATELIMIT_BACKEND", " DB ")

	cfg, err := loadStartupConfig("3000")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://tutor.example:8443" {
		t.Fatalf("BaseURL=%q", cfg.BaseURL)
	}
	if cfg.DBDriver != "postgres" || cfg.RateLimitBackend != "postgres" {
		t.Fatalf("aliases were not normalized: %+v", cfg)
	}
}

func TestLoadStartupConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "database driver", env: map[string]string{"DB_DRIVER": "mysql"}, wantErr: "unknown DB_DRIVER"},
		{name: "scheduler", env: map[string]string{"SCHEDULER_MODE": "cluster"}, wantErr: "unknown SCHEDULER_MODE"},
		{name: "rate limit backend", env: map[string]string{"RATELIMIT_BACKEND": "redis"}, wantErr: "unknown RATELIMIT_BACKEND"},
		{name: "rate limit needs postgres", env: map[string]string{"RATELIMIT_BACKEND": "postgres"}, wantErr: "requires DB_DRIVER=postgres"},
		{name: "base URL scheme", env: map[string]string{"BASE_URL": "ftp://example.com"}, wantErr: "scheme must be http or https"},
		{name: "base URL host", env: map[string]string{"BASE_URL": "https:///mcp"}, wantErr: "host is required"},
		{name: "base URL path", env: map[string]string{"BASE_URL": "https://example.com/mcp"}, wantErr: "path is not allowed"},
		{name: "base URL query", env: map[string]string{"BASE_URL": "https://example.com?x=1"}, wantErr: "query and fragment are not allowed"},
		{name: "base URL empty fragment", env: map[string]string{"BASE_URL": "https://example.com#"}, wantErr: "query and fragment are not allowed"},
		{name: "base URL user info", env: map[string]string{"BASE_URL": "https://user@example.com"}, wantErr: "user information is not allowed"},
		{name: "MCP body limit is not an integer", env: map[string]string{"MCP_MAX_REQUEST_BODY_BYTES": "large"}, wantErr: "MCP_MAX_REQUEST_BODY_BYTES must be an integer"},
		{name: "MCP body limit is zero", env: map[string]string{"MCP_MAX_REQUEST_BODY_BYTES": "0"}, wantErr: "MCP_MAX_REQUEST_BODY_BYTES must be between"},
		{name: "MCP body limit exceeds ceiling", env: map[string]string{"MCP_MAX_REQUEST_BODY_BYTES": "67108865"}, wantErr: "MCP_MAX_REQUEST_BODY_BYTES must be between"},
		{name: "MCP concurrency invalid", env: map[string]string{"MCP_MAX_CONCURRENT": "0"}, wantErr: "MCP_MAX_CONCURRENT must be between"},
		{name: "MCP per learner exceeds global", env: map[string]string{"MCP_MAX_CONCURRENT": "2", "MCP_MAX_CONCURRENT_PER_LEARNER": "3"}, wantErr: "must not exceed"},
		{name: "MCP tool timeout is not an integer", env: map[string]string{"MCP_TOOL_CALL_TIMEOUT_SECONDS": "slow"}, wantErr: "MCP_TOOL_CALL_TIMEOUT_SECONDS must be an integer"},
		{name: "MCP tool timeout is zero", env: map[string]string{"MCP_TOOL_CALL_TIMEOUT_SECONDS": "0"}, wantErr: "MCP_TOOL_CALL_TIMEOUT_SECONDS must be between"},
		{name: "MCP tool timeout exceeds ceiling", env: map[string]string{"MCP_TOOL_CALL_TIMEOUT_SECONDS": "601"}, wantErr: "MCP_TOOL_CALL_TIMEOUT_SECONDS must be between"},
		{name: "deployment profile", env: map[string]string{"DEPLOYMENT_PROFILE": "staging"}, wantErr: "unknown DEPLOYMENT_PROFILE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearStartupEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			_, err := loadStartupConfig("3000")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func productionEnv() map[string]string {
	return map[string]string{
		"DEPLOYMENT_PROFILE":                "production",
		"BASE_URL":                          "https://tutor.example",
		"DB_DRIVER":                         "postgres",
		"RATELIMIT_BACKEND":                 "postgres",
		"DATABASE_URL":                      "postgres://user:pass@db.example/tutor?sslmode=verify-full&sslrootcert=%2Fetc%2Ftutor%2Fca.pem",
		"SMTP_ADDR":                         "smtp.example:587",
		"SMTP_FROM":                         "tutor@example.com",
		"INTEGRATION_SECRET_KEYS":           "k1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"INTEGRATION_SECRET_CURRENT_KEY_ID": "k1",
		"TRUSTED_PROXY_CIDRS":               "10.0.0.0/8",
	}
}

func TestProductionProfileFailsClosed(t *testing.T) {
	tests := []struct {
		name, key, value, want string
	}{
		{name: "happy path"},
		{name: "HTTP", key: "BASE_URL", value: "http://tutor.example", want: "HTTPS BASE_URL"},
		{name: "SQLite", key: "DB_DRIVER", value: "sqlite", want: "DB_DRIVER=postgres"},
		{name: "local rate limits", key: "RATELIMIT_BACKEND", value: "memory", want: "RATELIMIT_BACKEND=postgres"},
		{name: "TLS downgrade", key: "DATABASE_URL", value: "postgres://db.example/tutor?sslmode=disable", want: "sslmode=verify-full"},
		{name: "missing CA", key: "DATABASE_URL", value: "postgres://db.example/tutor?sslmode=verify-full", want: "sslrootcert"},
		{name: "missing SMTP", key: "SMTP_ADDR", value: "", want: "SMTP_ADDR"},
		{name: "missing encryption keys", key: "INTEGRATION_SECRET_KEYS", value: "", want: "encryption keys"},
		{name: "spoofable proxy", key: "TRUSTED_PROXY_CIDRS", value: "0.0.0.0/0", want: "catch-all"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearStartupEnv(t)
			for key, value := range productionEnv() {
				t.Setenv(key, value)
			}
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}
			cfg, err := loadStartupConfig("3000")
			if tc.want == "" {
				if err != nil || cfg.DeploymentProfile != "production" {
					t.Fatalf("production config=%+v err=%v", cfg, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadStartupConfigDistributedPrerequisites(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "postgres required",
			env: map[string]string{
				"SCHEDULER_MODE":           "distributed",
				"RATELIMIT_BACKEND":        "postgres",
				"TUTOR_MCP_MEMORY_ENABLED": "off",
			},
			wantErr: "requires DB_DRIVER=postgres",
		},
		{
			name: "shared rate limit required",
			env: map[string]string{
				"DB_DRIVER":                "postgres",
				"SCHEDULER_MODE":           "distributed",
				"TUTOR_MCP_MEMORY_ENABLED": "off",
			},
			wantErr: "requires RATELIMIT_BACKEND=postgres",
		},
		{
			name: "local narrative memory rejected",
			env: map[string]string{
				"DB_DRIVER":         "postgres",
				"SCHEDULER_MODE":    "distributed",
				"RATELIMIT_BACKEND": "postgres",
			},
			wantErr: "requires TUTOR_MCP_MEMORY_ENABLED=off",
		},
		{
			name: "consistent experimental profile",
			env: map[string]string{
				"DB_DRIVER":                "postgres",
				"SCHEDULER_MODE":           "distributed",
				"RATELIMIT_BACKEND":        "postgres",
				"TUTOR_MCP_MEMORY_ENABLED": "off",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearStartupEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			_, err := loadStartupConfig("3000")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestEnsureSQLiteParentUsesConfiguredPath(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "nested", "state", "runtime.db")

	if err := ensureSQLiteParent(dbPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Dir(dbPath))
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode=%04o, want 0700", got)
	}
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := dbInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode=%04o, want 0600", got)
	}
}

func clearStartupEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BASE_URL",
		"DEPLOYMENT_PROFILE",
		"DB_DRIVER",
		"DATABASE_URL",
		"DB_PATH",
		"MCP_MAX_REQUEST_BODY_BYTES",
		"MCP_MAX_CONCURRENT",
		"MCP_MAX_CONCURRENT_PER_LEARNER",
		"MCP_TOOL_CALL_TIMEOUT_SECONDS",
		"RATELIMIT_BACKEND",
		"SMTP_ADDR",
		"SMTP_FROM",
		"INTEGRATION_SECRET_KEYS",
		"INTEGRATION_SECRET_CURRENT_KEY_ID",
		"TRUSTED_PROXY_CIDRS",
		"SCHEDULER_MODE",
		"TUTOR_MCP_MEMORY_ENABLED",
	} {
		t.Setenv(name, "")
	}
}
