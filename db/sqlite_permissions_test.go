// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSQLitePathCreatesPrivateDirectoryAndFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "runtime.db")

	if err := PreparePrivateSQLitePath(path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Dir(path), 0o700)
	assertMode(t, path, 0o600)
}

func TestPrepareSQLitePathRejectsUnsafeExistingParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}

	err := PreparePrivateSQLitePath(filepath.Join(parent, "runtime.db"))
	if err == nil || !strings.Contains(err.Error(), "require 0700") {
		t.Fatalf("error=%v, want unsafe-parent rejection", err)
	}
}

func TestPrepareSQLitePathTightensExistingDatabaseAndSidecars(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.db")
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(candidate, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(candidate, 0o664); err != nil {
			t.Fatal(err)
		}
	}

	if err := PrepareSQLitePath(path); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		assertMode(t, candidate, 0o600)
	}
}

func TestPrepareSQLitePathRejectsDatabaseSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "runtime.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	err := PrepareSQLitePath(link)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("error=%v, want symlink rejection", err)
	}
}

func TestOpenDBLeavesSQLiteFilesPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	database, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.Exec(`CREATE TABLE permission_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := SecureSQLiteFiles(path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err == nil {
			assertMode(t, sidecar, 0o600)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%04o, want %04o", path, got, want)
	}
}
