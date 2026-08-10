// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	privateDirMode  fs.FileMode = 0o700
	privateFileMode fs.FileMode = 0o600
)

// PrepareSQLitePath establishes a fail-closed file permission boundary before
// SQLite opens the database. In particular, it creates a missing database file
// itself with mode 0600 so there is no creation window where the process umask
// could make learner data readable by another local user. Existing database
// and sidecar files are exact targets and are safely tightened to 0600.
func PrepareSQLitePath(dbPath string) error {
	return prepareSQLitePath(dbPath, false)
}

// PreparePrivateSQLitePath additionally requires the database's direct parent
// directory to be private (0700). The server startup path uses this stricter
// boundary. Generic library callers may intentionally store a private 0600
// database below a shared parent, so OpenDB itself only invokes
// PrepareSQLitePath.
//
// Existing parent directories are never chmodded automatically: DB_PATH may
// point below a shared system directory and silently changing that directory's
// mode could break unrelated services. Instead, an unsafe existing directory
// is rejected with an actionable error.
func PreparePrivateSQLitePath(dbPath string) error {
	return prepareSQLitePath(dbPath, true)
}

func prepareSQLitePath(dbPath string, requirePrivateParent bool) error {
	cleanPath := filepath.Clean(dbPath)
	if cleanPath == "." || filepath.Base(cleanPath) == "." {
		return fmt.Errorf("SQLite DB_PATH must name a file")
	}

	parent := filepath.Dir(cleanPath)
	parentInfo, err := os.Lstat(parent)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(parent, privateDirMode); err != nil {
			return fmt.Errorf("create SQLite parent %q: %w", parent, err)
		}
		parentInfo, err = os.Lstat(parent)
	}
	if err != nil {
		return fmt.Errorf("inspect SQLite parent %q: %w", parent, err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("SQLite parent %q must be a real directory, not a symlink", parent)
	}
	if !ownedByCurrentUser(parentInfo) {
		return fmt.Errorf("SQLite parent %q is not owned by the current user", parent)
	}
	if got := parentInfo.Mode().Perm(); requirePrivateParent && got != privateDirMode {
		return fmt.Errorf("SQLite parent %q permissions are %04o; require 0700", parent, got)
	}

	if err := secureExistingSQLiteFile(cleanPath); errors.Is(err, fs.ErrNotExist) {
		file, createErr := os.OpenFile(cleanPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, privateFileMode)
		if createErr != nil {
			return fmt.Errorf("create SQLite database %q: %w", cleanPath, createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close new SQLite database %q: %w", cleanPath, closeErr)
		}
	} else if err != nil {
		return err
	}

	return SecureSQLiteFiles(cleanPath)
}

// SecureSQLiteFiles tightens the main database and every SQLite sidecar that
// currently exists. SQLite's Unix VFS derives new journal/WAL modes from the
// main file, so securing it before open also protects later sidecars.
func SecureSQLiteFiles(dbPath string) error {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if err := secureExistingSQLiteFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func secureExistingSQLiteFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("SQLite file %q must be a regular file, not a symlink", path)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("SQLite file %q is not owned by the current user", path)
	}
	if err := os.Chmod(path, privateFileMode); err != nil {
		return fmt.Errorf("chmod SQLite file %q to 0600: %w", path, err)
	}
	verified, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("verify SQLite file %q: %w", path, err)
	}
	if got := verified.Mode().Perm(); got != privateFileMode {
		return fmt.Errorf("SQLite file %q permissions remain %04o; require 0600", path, got)
	}
	return nil
}

func ownedByCurrentUser(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
