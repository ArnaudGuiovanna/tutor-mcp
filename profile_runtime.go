package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tutor-mcp/db"
	"tutor-mcp/internal/privatefs"
)

// profileKeys is an installation descriptor and keyring, committed before the
// database is created. Its private file must be backed up with runtime.db.
type profileKeys struct {
	Profile     string `json:"profile"`
	PublicURL   string `json:"public_url,omitempty"`
	MemoryKey   string `json:"memory_key"`
	SigningSeed string `json:"signing_seed,omitempty"`
}

func profileDataDir(profile, override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tutor-mcp", profile), nil
}

// openProfileStore holds a short OS lock across key creation, migrations and
// profile validation. Normal work uses SQLite transactions, not this lock.
func openProfileStore(ctx context.Context, profile, dir, publicURL string) (*db.Store, profileKeys, error) {
	var keys profileKeys
	if err := privatefs.PrepareDir(dir, true); err != nil {
		return nil, keys, err
	}
	release, err := privatefs.Lock(ctx, filepath.Join(dir, "startup.lock"))
	if err != nil {
		return nil, keys, err
	}
	defer release()
	keyPath := filepath.Join(dir, "keys.json")
	dbPath := filepath.Join(dir, "runtime.db")
	err = privatefs.SecureFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if _, statErr := os.Lstat(dbPath + suffix); !errors.Is(statErr, os.ErrNotExist) {
				return nil, keys, fmt.Errorf("keys.json is missing on an existing installation; restore it from backup (keys cannot be regenerated)")
			}
		}
		keys.Profile = profile
		keys.PublicURL = publicURL
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, keys, err
		}
		keys.MemoryKey = base64.StdEncoding.EncodeToString(key)
		if profile == "hobby" {
			if publicURL == "" {
				return nil, keys, fmt.Errorf("hobby requires init --profile hobby --public-url https://your-domain")
			}
			if _, err = rand.Read(key); err != nil {
				return nil, keys, err
			}
			keys.SigningSeed = base64.StdEncoding.EncodeToString(key)
		}
		data, err := json.MarshalIndent(keys, "", "  ")
		if err != nil {
			return nil, keys, err
		}
		// A partial file can never replace a valid keyring. The startup lock
		// excludes other creators; a crash leaves only an unused temp file.
		tmp, err := privatefs.CreateTemp(dir, ".keys-")
		if err != nil {
			return nil, keys, err
		}
		defer os.Remove(tmp.Name())
		if err = privatefs.SecureFile(tmp.Name()); err != nil {
			tmp.Close()
			return nil, keys, err
		}
		_, writeErr := tmp.Write(data)
		syncErr := tmp.Sync()
		closeErr := tmp.Close()
		if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
			return nil, keys, err
		}
		if err = os.Rename(tmp.Name(), keyPath); err != nil {
			return nil, keys, err
		}
		if err = privatefs.SyncDir(dir); err != nil {
			return nil, keys, err
		}
	} else if err != nil {
		return nil, keys, err
	} else {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, keys, err
		}
		if err = json.Unmarshal(data, &keys); err != nil {
			return nil, keys, fmt.Errorf("invalid keys.json; restore the original file from backup")
		}
	}
	if keys.Profile != profile {
		return nil, keys, fmt.Errorf("installation belongs to profile %s, requested %s", keys.Profile, profile)
	}
	if publicURL != "" && keys.PublicURL != publicURL {
		return nil, keys, fmt.Errorf("public URL differs from initialized installation")
	}
	keyring, err := db.NewIntegrationSecretKeyring("primary:"+keys.MemoryKey, "primary")
	if err != nil {
		return nil, keys, fmt.Errorf("invalid memory key in keys.json; restore it from backup")
	}
	if profile == "hobby" {
		seed, err := base64.StdEncoding.DecodeString(keys.SigningSeed)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, keys, fmt.Errorf("invalid signing key in keys.json; restore it from backup")
		}
	}
	database, err := db.OpenDB(dbPath)
	if err != nil {
		return nil, keys, err
	}
	if err = db.MigrateContext(ctx, database); err != nil {
		database.Close()
		return nil, keys, err
	}
	store := db.NewStore(database)
	if err = store.EnsureInstallation(ctx, profile); err != nil {
		store.Close()
		return nil, keys, err
	}
	store.SetIntegrationSecretKeyring(keyring)
	if _, err = store.RotateNarrativeSecrets(ctx); err != nil {
		store.Close()
		return nil, keys, fmt.Errorf("cannot authenticate narrative memory with keys.json: %w", err)
	}
	return store, keys, nil
}
