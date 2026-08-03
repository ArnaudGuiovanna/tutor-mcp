// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDBReadOnlyDoesNotMutateMainDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime ?# unicode-é.db")
	database, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(database); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO learners (id, email, password_hash, objective) VALUES ('readonly', 'ro@test.invalid', 'hash', 'learn')`,
	); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(beforeBytes)
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var learners, queryOnly int
	if err := readOnly.QueryRow(`SELECT COUNT(*) FROM learners`).Scan(&learners); err != nil {
		readOnly.Close()
		t.Fatal(err)
	}
	if learners != 1 {
		readOnly.Close()
		t.Fatalf("learner count=%d, want 1", learners)
	}
	if err := readOnly.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil {
		readOnly.Close()
		t.Fatal(err)
	}
	if queryOnly != 1 {
		readOnly.Close()
		t.Fatalf("PRAGMA query_only=%d, want 1", queryOnly)
	}
	if _, err := readOnly.ExecContext(context.Background(),
		`UPDATE learners SET objective = 'mutated' WHERE id = 'readonly'`); err == nil {
		readOnly.Close()
		t.Fatal("write through read-only connection unexpectedly succeeded")
	}
	duringInfo, err := os.Stat(path)
	if err != nil {
		readOnly.Close()
		t.Fatal(err)
	}
	if duringInfo.Size() != beforeInfo.Size() || !duringInfo.ModTime().Equal(beforeInfo.ModTime()) {
		readOnly.Close()
		t.Fatalf("read-only connection changed main database metadata: before=%v/%d during=%v/%d",
			beforeInfo.ModTime(), beforeInfo.Size(), duringInfo.ModTime(), duringInfo.Size())
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterHash := sha256.Sum256(afterBytes)
	if !bytes.Equal(afterHash[:], beforeHash[:]) {
		t.Fatal("read-only connection changed database bytes")
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterInfo.Size() != beforeInfo.Size() || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("read-only close changed main database metadata: before=%v/%d after=%v/%d",
			beforeInfo.ModTime(), beforeInfo.Size(), afterInfo.ModTime(), afterInfo.Size())
	}
}
