package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"tutor-mcp/internal/privatefs"
)

func configureProfileCommand(ctx context.Context, options commandOptions, output io.Writer) (bool, error) {
	if options.Profile == "institution" {
		// Reuse every production configuration gate, including separate runtime
		// roles, PostgreSQL credentials, asymmetric keys, SMTP and proxy policy.
		if err := os.Setenv("DEPLOYMENT_PROFILE", "production"); err != nil {
			return false, err
		}
		return false, os.Setenv("DB_DRIVER", "postgres")
	}
	if options.Profile != "hobby" {
		return false, nil
	}
	dir, err := profileDataDir("hobby", options.DataDir)
	if err != nil {
		return false, err
	}
	publicURL := options.PublicURL
	if options.Command == "init" {
		publicURL, err = normalizeBaseURL(publicURL)
		if err != nil || !strings.HasPrefix(publicURL, "https://") {
			return false, fmt.Errorf("init requires --public-url with an HTTPS origin, e.g. https://tutor.example.org")
		}
	}
	store, keys, err := openProfileStore(ctx, "hobby", dir, publicURL)
	if err != nil {
		return false, err
	}
	defer store.Close()
	if options.Command == "init" || options.Command == "users" {
		action := options.UserAction
		if options.Command == "init" {
			users, err := store.ListHobbyUsers(ctx)
			if err != nil {
				return false, err
			}
			if len(users) > 0 {
				fmt.Fprintln(output, "Already initialized. MCP URL:", keys.PublicURL+"/mcp")
				return true, nil
			}
			action = "invite"
		}
		switch action {
		case "invite", "reset":
			raw, err := store.CreateHobbyLink(ctx, action, options.LoginName)
			if err != nil {
				return false, err
			}
			expiry := "24 hours"
			if action == "reset" {
				expiry = "15 minutes"
			}
			fmt.Fprintf(output, "One-time %s link (expires in %s):\n%s/account/%s?token=%s\nMCP URL: %s/mcp\n", action, expiry, keys.PublicURL, action, raw, keys.PublicURL)
		case "list":
			users, err := store.ListHobbyUsers(ctx)
			if err != nil {
				return false, err
			}
			for _, user := range users {
				fmt.Fprintf(output, "%s\t%s\n", user.LoginName, user.Status)
			}
		case "disable":
			if err := store.DisableHobbyUser(ctx, options.LoginName); err != nil {
				return false, err
			}
			fmt.Fprintln(output, "Account disabled; existing grants revoked.")
		}
		return true, nil
	}
	seed, _ := base64.StdEncoding.DecodeString(keys.SigningSeed) // validated by openProfileStore
	privateKey := ed25519.NewKeyFromSeed(seed)
	jwtKeys, err := json.Marshal([]map[string]any{{"kid": "primary", "public_key": base64.StdEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)), "private_key": keys.SigningSeed, "active": true}})
	if err != nil {
		return false, err
	}
	for name, value := range map[string]string{
		"DEPLOYMENT_PROFILE": "development", "PROCESS_ROLE": "all", "DB_DRIVER": "sqlite",
		"DB_PATH": filepath.Join(dir, "runtime.db"), "BASE_URL": keys.PublicURL,
		"SCHEDULER_MODE": "inprocess", "RATELIMIT_BACKEND": "memory",
		"TUTOR_MCP_MEMORY_BACKEND": "database", "TUTOR_MCP_MEMORY_ROOT": filepath.Join(dir, "memory"),
		"JWT_ED25519_KEYS": string(jwtKeys), "INTEGRATION_SECRET_KEYS": "primary:" + keys.MemoryKey,
		"INTEGRATION_SECRET_CURRENT_KEY_ID": "primary", "SMTP_ADDR": "",
	} {
		if err := os.Setenv(name, value); err != nil {
			return false, err
		}
	}
	if strings.TrimSpace(os.Getenv("TRUSTED_PROXY_CIDRS")) == "" {
		if err := os.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1/32,::1/128"); err != nil {
			return false, err
		}
	}
	if err := validateTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS")); err != nil {
		return false, err
	}
	return false, nil
}

func httpListenAddress(options commandOptions, port string) string {
	if options.Profile == "hobby" || options.Profile == "institution" {
		if addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); addr != "" {
			return addr
		}
		return net.JoinHostPort("127.0.0.1", port)
	}
	return ":" + port
}

func lockHobbyServer(options commandOptions) (func(), error) {
	if options.Profile != "hobby" {
		return func() {}, nil
	}
	dir, err := profileDataDir("hobby", options.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := privatefs.Lock(ctx, filepath.Join(dir, "server.lock"))
	if err != nil {
		return nil, fmt.Errorf("hobby allows one HTTP server per installation; cannot acquire server.lock: %w", err)
	}
	return release, nil
}
