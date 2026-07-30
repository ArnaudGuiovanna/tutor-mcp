// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func clearStartupEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BASE_URL",
		"DB_DRIVER",
		"DB_PATH",
		"RATELIMIT_BACKEND",
		"SCHEDULER_MODE",
		"TUTOR_MCP_MEMORY_ENABLED",
	} {
		t.Setenv(name, "")
	}
}
