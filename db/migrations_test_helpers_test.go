// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
)

// These wrappers deliberately live in test code: production migration runs
// use one reserved BEGIN EXCLUSIVE transaction and call the *InTx helpers.
func ensureSchemaMigrationsTable(db *sql.DB) error {
	return ensureSchemaMigrationsTableInTx(context.Background(), db)
}

func applyMigration(db *sql.DB, m migration) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx %q: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := applyMigrationInTx(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit()
}
