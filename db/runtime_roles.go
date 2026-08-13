// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"fmt"
)

// VerifyPostgresRuntimeRole rejects the privilege shapes that make FORCE RLS
// ineffective in practice. The migrator deliberately does not call this gate.
func VerifyPostgresRuntimeRole(ctx context.Context, database *sql.DB) error {
	var role string
	var superuser, bypassRLS, ownsRLSTable, createsInSchema bool
	if err := database.QueryRowContext(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls,
		EXISTS (
		  SELECT 1 FROM pg_class c JOIN pg_roles owner ON owner.oid = c.relowner
		  WHERE c.relnamespace = current_schema()::regnamespace
		    AND c.relkind = 'r' AND c.relrowsecurity AND owner.rolname = current_user
		),
		has_schema_privilege(current_user, current_schema(), 'CREATE')
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(
		&role, &superuser, &bypassRLS, &ownsRLSTable, &createsInSchema); err != nil {
		return fmt.Errorf("verify PostgreSQL runtime role: %w", err)
	}
	if superuser || bypassRLS || ownsRLSTable || createsInSchema {
		return fmt.Errorf("PostgreSQL runtime role %q violates least privilege (superuser=%t bypass_rls=%t owns_rls_table=%t schema_create=%t)",
			role, superuser, bypassRLS, ownsRLSTable, createsInSchema)
	}
	return nil
}
