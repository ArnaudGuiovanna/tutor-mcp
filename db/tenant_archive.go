// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"
)

const tenantLogicalArchiveFormat = 1

var postgresIdentifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// TenantLogicalArchive is a portable, schema-exact PostgreSQL tenant archive.
// It contains no row from another tenant. Global rows are limited to the user,
// identity, OAuth-client and plan dependencies referenced by this tenant.
// Operators must encrypt the encoded archive before it reaches durable media.
type TenantLogicalArchive struct {
	FormatVersion int                        `json:"format_version"`
	TenantID      string                     `json:"tenant_id"`
	SchemaVersion string                     `json:"schema_version"`
	CapturedAt    time.Time                  `json:"captured_at"`
	TenantRow     json.RawMessage            `json:"tenant_row"`
	Globals       map[string]json.RawMessage `json:"globals"`
	Tables        map[string]json.RawMessage `json:"tables"`
	DataSHA256    string                     `json:"data_sha256"`
	ArchiveSHA256 string                     `json:"archive_sha256"`
}

type TenantLogicalRestoreResult struct {
	TenantID       string `json:"tenant_id"`
	SchemaVersion  string `json:"schema_version"`
	TableCount     int    `json:"table_count"`
	RestoredRows   int64  `json:"restored_rows"`
	DataSHA256     string `json:"data_sha256"`
	ArchiveSHA256  string `json:"archive_sha256"`
	ForeignKeysOK  bool   `json:"foreign_keys_ok"`
	IsolationCheck bool   `json:"isolation_check"`
}

var tenantArchiveGlobalTables = []string{
	"external_identities",
	"mfa_credentials",
	"oauth_clients",
	"plans",
	"users",
}

// ExportPostgresTenantArchive captures one repeatable-read snapshot. It is
// deliberately PostgreSQL-only: SQLite is not a supported multi-tenant SaaS
// authority and cannot provide the restore-role guarantees used below.
func (s *Store) ExportPostgresTenantArchive(ctx context.Context, actor models.ControlPlanePrincipal, tenantID string) ([]byte, error) {
	if s == nil || s.root == nil || s.dialect != DialectPostgres || !actor.Validate() || tenantID == "" {
		return nil, fmt.Errorf("export tenant archive: invalid PostgreSQL store, authority or tenant")
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("export tenant archive: begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', $1, true)`, tenantID); err != nil {
		return nil, fmt.Errorf("export tenant archive: set tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user', $1, true)`, actor.ActorID); err != nil {
		return nil, fmt.Errorf("export tenant archive: set actor: %w", err)
	}
	txs := &Store{db: tx, dialect: DialectPostgres, secretKeyring: s.secretKeyring}
	archive, err := txs.captureTenantLogicalArchive(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	archive.ArchiveSHA256, err = tenantArchiveDigest(archive, false)
	if err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export tenant archive: encode: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("export tenant archive: commit snapshot: %w", err)
	}
	return encoded, nil
}

func (s *Store) captureTenantLogicalArchive(ctx context.Context, tenantID string) (*TenantLogicalArchive, error) {
	version, err := s.currentPostgresSchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	tables, err := s.postgresTenantTables(ctx)
	if err != nil {
		return nil, err
	}
	tenantRows, err := s.exportPostgresRows(ctx, "tenants", `t.id = ?`, tenantID)
	if err != nil {
		return nil, err
	}
	var tenantSet []json.RawMessage
	if err := json.Unmarshal(tenantRows, &tenantSet); err != nil || len(tenantSet) != 1 {
		return nil, fmt.Errorf("export tenant archive: tenant not found or ambiguous")
	}
	archive := &TenantLogicalArchive{
		FormatVersion: tenantLogicalArchiveFormat,
		TenantID:      tenantID,
		SchemaVersion: version,
		CapturedAt:    time.Now().UTC(),
		TenantRow:     append(json.RawMessage(nil), tenantSet[0]...),
		Globals:       make(map[string]json.RawMessage, len(tenantArchiveGlobalTables)),
		Tables:        make(map[string]json.RawMessage, len(tables)),
	}
	for _, table := range tables {
		rows, err := s.exportPostgresRows(ctx, table, `t.tenant_id = ?`, tenantID)
		if err != nil {
			return nil, err
		}
		archive.Tables[table] = rows
	}
	globalPredicates := map[string]string{
		"users":               `t.id IN (SELECT user_id FROM tenant_memberships WHERE tenant_id = ?)`,
		"external_identities": `t.user_id IN (SELECT user_id FROM tenant_memberships WHERE tenant_id = ?)`,
		"mfa_credentials":     `t.user_id IN (SELECT user_id FROM tenant_memberships WHERE tenant_id = ?)`,
		"plans":               `t.id IN (SELECT plan_id FROM tenant_subscriptions WHERE tenant_id = ?)`,
		"oauth_clients": `t.client_id IN (
			SELECT client_id FROM learner_approved_clients WHERE tenant_id = ?
			UNION SELECT client_id FROM service_accounts WHERE tenant_id = ?
			UNION SELECT client_id FROM oauth_codes WHERE tenant_id = ?
			UNION SELECT client_id FROM refresh_tokens WHERE tenant_id = ?
		)`,
	}
	for _, table := range tenantArchiveGlobalTables {
		predicate := globalPredicates[table]
		args := []any{tenantID}
		if table == "oauth_clients" {
			args = []any{tenantID, tenantID, tenantID, tenantID}
		}
		rows, err := s.exportPostgresRows(ctx, table, predicate, args...)
		if err != nil {
			return nil, err
		}
		archive.Globals[table] = rows
	}
	archive.DataSHA256, err = tenantArchiveDigest(archive, true)
	if err != nil {
		return nil, err
	}
	return archive, nil
}

func tenantArchiveDigest(archive *TenantLogicalArchive, dataOnly bool) (string, error) {
	var value any
	if dataOnly {
		value = struct {
			TenantID  string                     `json:"tenant_id"`
			TenantRow json.RawMessage            `json:"tenant_row"`
			Tables    map[string]json.RawMessage `json:"tables"`
		}{archive.TenantID, archive.TenantRow, archive.Tables}
	} else {
		copyArchive := *archive
		copyArchive.ArchiveSHA256 = ""
		value = copyArchive
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("tenant archive digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) currentPostgresSchemaVersion(ctx context.Context) (string, error) {
	var version string
	if err := s.queryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version); err != nil {
		return "", fmt.Errorf("tenant archive schema version: %w", err)
	}
	return version, nil
}

func (s *Store) postgresTenantTables(ctx context.Context) ([]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT table_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND column_name = 'tenant_id'
		ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("tenant archive table inventory: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if !postgresIdentifierPattern.MatchString(table) {
			return nil, fmt.Errorf("tenant archive: unsafe table identifier")
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("tenant archive: no tenant-owned tables found")
	}
	return tables, nil
}

func quotePostgresIdentifier(value string) (string, error) {
	if !postgresIdentifierPattern.MatchString(value) {
		return "", fmt.Errorf("tenant archive: unsafe PostgreSQL identifier")
	}
	return `"` + value + `"`, nil
}

func (s *Store) postgresPrimaryKeyColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := s.query(ctx, `SELECT attribute.attname
		FROM pg_index index_row
		JOIN pg_class table_row ON table_row.oid = index_row.indrelid
		JOIN pg_namespace namespace_row ON namespace_row.oid = table_row.relnamespace
		JOIN unnest(index_row.indkey) WITH ORDINALITY key_row(attnum, position) ON true
		JOIN pg_attribute attribute ON attribute.attrelid = table_row.oid AND attribute.attnum = key_row.attnum
		WHERE namespace_row.nspname = current_schema() AND table_row.relname = ?
		  AND index_row.indisprimary ORDER BY key_row.position`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		if !postgresIdentifierPattern.MatchString(column) {
			return nil, fmt.Errorf("tenant archive: unsafe primary-key identifier")
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func (s *Store) exportPostgresRows(ctx context.Context, table, predicate string, args ...any) (json.RawMessage, error) {
	quotedTable, err := quotePostgresIdentifier(table)
	if err != nil {
		return nil, err
	}
	primaryKey, err := s.postgresPrimaryKeyColumns(ctx, table)
	if err != nil {
		return nil, fmt.Errorf("tenant archive %s primary key: %w", table, err)
	}
	order := "to_jsonb(t)::text"
	if len(primaryKey) != 0 {
		parts := make([]string, 0, len(primaryKey))
		for _, column := range primaryKey {
			quoted, _ := quotePostgresIdentifier(column)
			parts = append(parts, "t."+quoted)
		}
		order = strings.Join(parts, ", ")
	}
	query := `SELECT to_jsonb(t)::text FROM ` + quotedTable + ` t WHERE ` + predicate + ` ORDER BY ` + order
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tenant archive export %s: %w", table, err)
	}
	defer rows.Close()
	values := make([]json.RawMessage, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("tenant archive export %s: invalid row JSON", table)
		}
		values = append(values, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(values)
	return encoded, err
}

// RestorePostgresTenantArchive restores an absent tenant into a compatible
// schema. The dedicated restore credential must be able to set
// session_replication_role locally; API and worker credentials intentionally
// cannot. RLS remains active for the one archive tenant. Trigger suppression
// is bounded to the transaction, every tenant FK is then checked explicitly,
// and the entire tenant row set is re-hashed before commit.
func (s *Store) RestorePostgresTenantArchive(ctx context.Context, actor models.ControlPlanePrincipal, encoded []byte) (*TenantLogicalRestoreResult, error) {
	if s == nil || s.root == nil || s.dialect != DialectPostgres || !actor.Validate() {
		return nil, fmt.Errorf("restore tenant archive: invalid PostgreSQL store or authority")
	}
	archive, err := decodeTenantLogicalArchive(encoded)
	if err != nil {
		return nil, err
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("restore tenant archive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txs := &Store{db: tx, dialect: DialectPostgres, secretKeyring: s.secretKeyring}
	currentVersion, err := txs.currentPostgresSchemaVersion(ctx)
	if err != nil || currentVersion != archive.SchemaVersion {
		return nil, fmt.Errorf("restore tenant archive: schema mismatch archive=%q current=%q", archive.SchemaVersion, currentVersion)
	}
	currentTables, err := txs.postgresTenantTables(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateTenantArchiveShape(archive, currentTables); err != nil {
		return nil, err
	}
	var tenantExists int
	if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id = ?`, archive.TenantID).Scan(&tenantExists); err != nil {
		return nil, err
	}
	if tenantExists != 0 {
		return nil, fmt.Errorf("restore tenant archive: tenant already exists")
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', $1, true)`, archive.TenantID); err != nil {
		return nil, fmt.Errorf("restore tenant archive: set tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user', $1, true)`, actor.ActorID); err != nil {
		return nil, fmt.Errorf("restore tenant archive: set actor: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return nil, fmt.Errorf("restore tenant archive: dedicated restore role cannot suppress triggers: %w", err)
	}

	var restoredRows int64
	for _, table := range tenantArchiveGlobalTables {
		count, err := txs.restoreGlobalArchiveRows(ctx, table, archive.Globals[table])
		if err != nil {
			return nil, err
		}
		restoredRows += count
	}
	tenantSet, _ := json.Marshal([]json.RawMessage{archive.TenantRow})
	count, err := txs.insertArchiveRows(ctx, "tenants", tenantSet, false)
	if err != nil {
		return nil, fmt.Errorf("restore tenant archive root: %w", err)
	}
	restoredRows += count
	for _, table := range currentTables {
		count, err := txs.insertArchiveRows(ctx, table, archive.Tables[table], false)
		if err != nil {
			return nil, fmt.Errorf("restore tenant archive table %s: %w", table, err)
		}
		restoredRows += count
	}
	if err := txs.resetPostgresIdentitySequences(ctx); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role = origin`); err != nil {
		return nil, err
	}
	if err := txs.verifyPostgresTenantForeignKeys(ctx, archive.TenantID); err != nil {
		return nil, err
	}
	restored, err := txs.captureTenantLogicalArchive(ctx, archive.TenantID)
	if err != nil {
		return nil, err
	}
	if restored.DataSHA256 != archive.DataSHA256 {
		return nil, fmt.Errorf("restore tenant archive: post-restore tenant checksum mismatch")
	}
	if err := compareTenantArchiveGlobals(archive.Globals, restored.Globals); err != nil {
		return nil, err
	}
	if err := txs.appendControlPlaneAudit(ctx, archive.TenantID, actor,
		"tenant.restore.logical", "tenant", archive.TenantID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("restore tenant archive: commit: %w", err)
	}
	return &TenantLogicalRestoreResult{
		TenantID: archive.TenantID, SchemaVersion: archive.SchemaVersion,
		TableCount: len(currentTables), RestoredRows: restoredRows,
		DataSHA256: archive.DataSHA256, ArchiveSHA256: archive.ArchiveSHA256,
		ForeignKeysOK: true, IsolationCheck: true,
	}, nil
}

func decodeTenantLogicalArchive(encoded []byte) (*TenantLogicalArchive, error) {
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var archive TenantLogicalArchive
	if err := decoder.Decode(&archive); err != nil {
		return nil, fmt.Errorf("restore tenant archive: invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("restore tenant archive: trailing JSON")
		}
		return nil, fmt.Errorf("restore tenant archive: trailing data: %w", err)
	}
	if archive.FormatVersion != tenantLogicalArchiveFormat || archive.TenantID == "" ||
		archive.SchemaVersion == "" || archive.CapturedAt.IsZero() || len(archive.TenantRow) == 0 ||
		len(archive.DataSHA256) != 64 || len(archive.ArchiveSHA256) != 64 {
		return nil, fmt.Errorf("restore tenant archive: incomplete envelope")
	}
	wantArchiveDigest, err := tenantArchiveDigest(&archive, false)
	if err != nil || wantArchiveDigest != archive.ArchiveSHA256 {
		return nil, fmt.Errorf("restore tenant archive: archive checksum mismatch")
	}
	wantDataDigest, err := tenantArchiveDigest(&archive, true)
	if err != nil || wantDataDigest != archive.DataSHA256 {
		return nil, fmt.Errorf("restore tenant archive: tenant data checksum mismatch")
	}
	return &archive, nil
}

func validateTenantArchiveShape(archive *TenantLogicalArchive, currentTables []string) error {
	expectedGlobals := append([]string(nil), tenantArchiveGlobalTables...)
	sort.Strings(expectedGlobals)
	actualGlobals := make([]string, 0, len(archive.Globals))
	for table := range archive.Globals {
		actualGlobals = append(actualGlobals, table)
	}
	sort.Strings(actualGlobals)
	if strings.Join(actualGlobals, "\x00") != strings.Join(expectedGlobals, "\x00") {
		return fmt.Errorf("restore tenant archive: global table inventory mismatch")
	}
	actualTables := make([]string, 0, len(archive.Tables))
	for table, raw := range archive.Tables {
		if !postgresIdentifierPattern.MatchString(table) || !json.Valid(raw) {
			return fmt.Errorf("restore tenant archive: invalid table payload")
		}
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("restore tenant archive: invalid rows for %s", table)
		}
		for _, row := range rows {
			if tenantID, ok := row["tenant_id"].(string); !ok || tenantID != archive.TenantID {
				return fmt.Errorf("restore tenant archive: cross-tenant row in %s", table)
			}
		}
		actualTables = append(actualTables, table)
	}
	sort.Strings(actualTables)
	if strings.Join(actualTables, "\x00") != strings.Join(currentTables, "\x00") {
		return fmt.Errorf("restore tenant archive: tenant table inventory mismatch")
	}
	var tenant map[string]any
	if err := json.Unmarshal(archive.TenantRow, &tenant); err != nil || tenant["id"] != archive.TenantID {
		return fmt.Errorf("restore tenant archive: invalid tenant root")
	}
	for _, table := range expectedGlobals {
		if !json.Valid(archive.Globals[table]) {
			return fmt.Errorf("restore tenant archive: invalid global rows for %s", table)
		}
	}
	return nil
}

func compareTenantArchiveGlobals(expected, actual map[string]json.RawMessage) error {
	for _, table := range tenantArchiveGlobalTables {
		left, err := canonicalTenantArchiveGlobalRows(table, expected[table])
		if err != nil {
			return fmt.Errorf("restore tenant archive: canonicalize expected %s: %w", table, err)
		}
		right, err := canonicalTenantArchiveGlobalRows(table, actual[table])
		if err != nil {
			return fmt.Errorf("restore tenant archive: canonicalize restored %s: %w", table, err)
		}
		if left != right {
			return fmt.Errorf("restore tenant archive: dependency closure mismatch for %s", table)
		}
	}
	return nil
}

func canonicalTenantArchiveGlobalRows(table string, raw json.RawMessage) (string, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", err
	}
	canonical := make([]string, 0, len(rows))
	for _, row := range rows {
		// Plans are immutable business definitions, but their migration-time
		// bookkeeping timestamps legitimately differ between independently
		// created databases. Every authority-bearing or commercial field is
		// still compared exactly.
		if table == "plans" {
			delete(row, "created_at")
			delete(row, "updated_at")
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		canonical = append(canonical, string(encoded))
	}
	sort.Strings(canonical)
	return strings.Join(canonical, "\n"), nil
}

func (s *Store) insertArchiveRows(ctx context.Context, table string, rows json.RawMessage, ignoreConflicts bool) (int64, error) {
	quoted, err := quotePostgresIdentifier(table)
	if err != nil {
		return 0, err
	}
	var values []json.RawMessage
	if err := json.Unmarshal(rows, &values); err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, nil
	}
	query := `INSERT INTO ` + quoted + ` OVERRIDING SYSTEM VALUE
		SELECT * FROM jsonb_populate_recordset(NULL::` + quoted + `, ?::jsonb)`
	if ignoreConflicts {
		query += ` ON CONFLICT DO NOTHING`
	}
	result, err := s.exec(ctx, query, string(rows))
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return count, nil
}

func (s *Store) restoreGlobalArchiveRows(ctx context.Context, table string, rows json.RawMessage) (int64, error) {
	inserted, err := s.insertArchiveRows(ctx, table, rows, true)
	if err != nil {
		return 0, fmt.Errorf("restore tenant archive global %s: %w", table, err)
	}
	if table == "plans" {
		var mismatches int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM jsonb_populate_recordset(NULL::plans, ?::jsonb) source
			LEFT JOIN plans target ON target.id = source.id
			WHERE target.id IS NULL OR target.name <> source.name OR target.status <> source.status
			   OR target.entitlements_json <> source.entitlements_json`, string(rows)).Scan(&mismatches); err != nil {
			return 0, err
		}
		if mismatches != 0 {
			return 0, fmt.Errorf("restore tenant archive: conflicting global plan")
		}
		return inserted, nil
	}
	quoted, _ := quotePostgresIdentifier(table)
	primaryKey, err := s.postgresPrimaryKeyColumns(ctx, table)
	if err != nil || len(primaryKey) == 0 {
		return 0, fmt.Errorf("restore tenant archive: global table %s has no primary key", table)
	}
	join := make([]string, 0, len(primaryKey))
	for _, column := range primaryKey {
		quotedColumn, _ := quotePostgresIdentifier(column)
		join = append(join, `target.`+quotedColumn+` = source.`+quotedColumn)
	}
	query := `SELECT COUNT(*) FROM jsonb_populate_recordset(NULL::` + quoted + `, ?::jsonb) source
		LEFT JOIN ` + quoted + ` target ON ` + strings.Join(join, " AND ") + `
		WHERE target IS NULL OR to_jsonb(target) <> to_jsonb(source)`
	var mismatches int
	if err := s.queryRow(ctx, query, string(rows)).Scan(&mismatches); err != nil {
		return 0, err
	}
	if mismatches != 0 {
		return 0, fmt.Errorf("restore tenant archive: conflicting global %s row", table)
	}
	return inserted, nil
}

func (s *Store) resetPostgresIdentitySequences(ctx context.Context) error {
	rows, err := s.query(ctx, `SELECT table_name, column_name
		FROM information_schema.columns WHERE table_schema = current_schema()
		AND is_identity = 'YES' ORDER BY table_name, ordinal_position`)
	if err != nil {
		return err
	}
	var identities [][2]string
	for rows.Next() {
		var item [2]string
		if err := rows.Scan(&item[0], &item[1]); err != nil {
			rows.Close()
			return err
		}
		identities = append(identities, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range identities {
		table, _ := quotePostgresIdentifier(item[0])
		column, _ := quotePostgresIdentifier(item[1])
		query := `SELECT setval(pg_get_serial_sequence(?, ?), COALESCE(MAX(` + column + `), 1), MAX(` + column + `) IS NOT NULL) FROM ` + table
		if err := s.queryRow(ctx, query, item[0], item[1]).Scan(new(int64)); err != nil {
			return fmt.Errorf("restore tenant archive: reset identity %s.%s: %w", item[0], item[1], err)
		}
	}
	return nil
}

func (s *Store) verifyPostgresTenantForeignKeys(ctx context.Context, tenantID string) error {
	rows, err := s.query(ctx, `SELECT source.relname, target.relname,
		array_to_string(ARRAY(
			SELECT attribute.attname FROM unnest(constraint_row.conkey) WITH ORDINALITY key_row(attnum, position)
			JOIN pg_attribute attribute ON attribute.attrelid = constraint_row.conrelid AND attribute.attnum = key_row.attnum
			ORDER BY key_row.position
		), ','),
		array_to_string(ARRAY(
			SELECT attribute.attname FROM unnest(constraint_row.confkey) WITH ORDINALITY key_row(attnum, position)
			JOIN pg_attribute attribute ON attribute.attrelid = constraint_row.confrelid AND attribute.attnum = key_row.attnum
			ORDER BY key_row.position
		), ','), constraint_row.conname
		FROM pg_constraint constraint_row
		JOIN pg_class source ON source.oid = constraint_row.conrelid
		JOIN pg_namespace namespace_row ON namespace_row.oid = source.relnamespace
		JOIN pg_class target ON target.oid = constraint_row.confrelid
		WHERE namespace_row.nspname = current_schema() AND constraint_row.contype = 'f'
		AND EXISTS (SELECT 1 FROM pg_attribute tenant_column WHERE tenant_column.attrelid = source.oid
			AND tenant_column.attname = 'tenant_id' AND NOT tenant_column.attisdropped)
		ORDER BY source.relname, constraint_row.conname`)
	if err != nil {
		return err
	}
	type foreignKey struct{ source, target, sourceColumns, targetColumns, name string }
	var constraints []foreignKey
	for rows.Next() {
		var item foreignKey
		if err := rows.Scan(&item.source, &item.target, &item.sourceColumns, &item.targetColumns, &item.name); err != nil {
			rows.Close()
			return err
		}
		constraints = append(constraints, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, constraint := range constraints {
		source, err := quotePostgresIdentifier(constraint.source)
		if err != nil {
			return err
		}
		target, err := quotePostgresIdentifier(constraint.target)
		if err != nil {
			return err
		}
		sourceColumns, targetColumns := strings.Split(constraint.sourceColumns, ","), strings.Split(constraint.targetColumns, ",")
		if len(sourceColumns) != len(targetColumns) || len(sourceColumns) == 0 {
			return fmt.Errorf("restore tenant archive: invalid FK metadata")
		}
		nonNull, join := make([]string, 0, len(sourceColumns)), make([]string, 0, len(sourceColumns))
		for index := range sourceColumns {
			sourceColumn, err := quotePostgresIdentifier(sourceColumns[index])
			if err != nil {
				return err
			}
			targetColumn, err := quotePostgresIdentifier(targetColumns[index])
			if err != nil {
				return err
			}
			nonNull = append(nonNull, `source_row.`+sourceColumn+` IS NOT NULL`)
			join = append(join, `target_row.`+targetColumn+` = source_row.`+sourceColumn)
		}
		query := `SELECT COUNT(*) FROM ` + source + ` source_row WHERE source_row.tenant_id = ?
			AND ` + strings.Join(nonNull, " AND ") + ` AND NOT EXISTS (
				SELECT 1 FROM ` + target + ` target_row WHERE ` + strings.Join(join, " AND ") + `)`
		var invalid int64
		if err := s.queryRow(ctx, query, tenantID).Scan(&invalid); err != nil {
			return fmt.Errorf("restore tenant archive: verify FK %s: %w", constraint.name, err)
		}
		if invalid != 0 {
			return fmt.Errorf("restore tenant archive: FK %s has %d orphan rows", constraint.name, invalid)
		}
	}
	return nil
}
