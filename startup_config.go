// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type startupConfig struct {
	DBDriver         string
	DBPath           string
	BaseURL          string
	SchedulerMode    string
	RateLimitBackend string
}

func loadStartupConfig(port string) (startupConfig, error) {
	cfg := startupConfig{
		DBDriver:         normalizedEnv("DB_DRIVER", "sqlite"),
		DBPath:           strings.TrimSpace(os.Getenv("DB_PATH")),
		SchedulerMode:    normalizedEnv("SCHEDULER_MODE", "inprocess"),
		RateLimitBackend: normalizedEnv("RATELIMIT_BACKEND", "memory"),
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/runtime.db"
	}

	switch cfg.DBDriver {
	case "sqlite", "postgres":
	default:
		return startupConfig{}, fmt.Errorf("unknown DB_DRIVER %q (want sqlite or postgres)", cfg.DBDriver)
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

	return cfg, nil
}

func normalizedEnv(name, fallback string) string {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value
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
	parent := filepath.Dir(filepath.Clean(dbPath))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create DB_PATH parent %q: %w", parent, err)
	}
	return nil
}
