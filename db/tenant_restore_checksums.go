// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"tutor-mcp/models"
)

type tenantChecksumSpec struct{ table string }

// Every column of these tables participates, including content, relationships,
// status and timestamps. The coverage test inventories the remaining tables and
// requires an explicit reason for each deliberate table-level exclusion.
var tenantChecksumSpecs = []tenantChecksumSpec{
	{"concept_bkt_estimates"}, {"concept_collective_weights"}, {"collective_weight_applications"},
	{"learning_badges"}, {"learning_badge_evidence"}, {"interactions"}, {"concept_states"},
	{"assessment_attempts"}, {"assessment_reviews"}, {"assessment_adjudications"},
	{"learning_events"}, {"learning_sessions"}, {"pedagogical_decisions"}, {"transfer_records"},
	{"affect_states"}, {"calibration_records"}, {"domains"}, {"curriculum_versions"},
	{"formation_modules"}, {"formation_concepts"}, {"concept_prerequisites"},
	{"formation_trainers"}, {"cohort_trainers"}, {"formation_admissions"}, {"enrollment_migrations"},
	{"tenant_memberships"}, {"cohorts"}, {"enrollments"}, {"formation_versions"}, {"formations"},
	{"integration_deliveries"}, {"learner_concept_states"}, {"narrative_objects"}, {"usage_events"},
}

const tenantChecksumFormat = "content-v2"

func (s *Store) ComputeTenantChecksums(ctx context.Context, actor models.ControlPlanePrincipal, tenantID string) (string, string, error) {
	if !actor.Validate() || tenantID == "" {
		return "", "", fmt.Errorf("compute tenant checksums: invalid authority")
	}
	var tables, objects string
	err := s.withTenantRestoreSnapshot(ctx, tenantID, actor.ActorID, true, func(txs *Store) error {
		var err error
		tables, objects, err = txs.computeTenantChecksums(ctx, tenantID)
		return err
	})
	return tables, objects, err
}

// A normal PostgreSQL control transaction is READ COMMITTED, which can mix
// several moments across tables. Checksums and a verification decision instead
// share one repeatable snapshot. SQLite transactions already pin one snapshot.
func (s *Store) withTenantRestoreSnapshot(ctx context.Context, tenantID, actorID string, readOnly bool, fn func(*Store) error) error {
	if s == nil || tenantID == "" || actorID == "" {
		return fmt.Errorf("tenant restore snapshot: invalid store or authority")
	}
	if s.root == nil {
		if s.dialect == DialectPostgres {
			var isolation string
			if err := s.queryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
				return err
			}
			if isolation != "repeatable read" && isolation != "serializable" {
				return fmt.Errorf("tenant restore snapshot: existing transaction must be repeatable read or serializable")
			}
		}
		return fn(s)
	}
	level := sql.LevelSerializable
	if s.dialect == DialectPostgres {
		level = sql.LevelRepeatableRead
	}
	tx, err := s.root.BeginTx(ctx, &sql.TxOptions{Isolation: level, ReadOnly: readOnly})
	if err != nil {
		return fmt.Errorf("tenant restore snapshot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == DialectPostgres {
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_tenant', $1, true)`, tenantID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user', $1, true)`, actorID); err != nil {
			return err
		}
	}
	if err := fn(&Store{db: tx, dialect: s.dialect, secretKeyring: s.secretKeyring}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) computeTenantChecksums(ctx context.Context, tenantID string) (string, string, error) {
	tableChecksums := make(map[string]string, len(tenantChecksumSpecs))
	for _, spec := range tenantChecksumSpecs {
		checksum, err := s.tenantTableContentChecksum(ctx, spec.table, tenantID)
		if err != nil {
			return "", "", fmt.Errorf("checksum %s: %w", spec.table, err)
		}
		tableChecksums[spec.table] = checksum
	}
	objectChecksums := map[string]string{}
	rows, err := s.query(ctx, `SELECT enrollment_id, scope, domain_id, object_key, checksum
		FROM narrative_objects WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var enrollment, scope, domain, key, checksum string
		if err := rows.Scan(&enrollment, &scope, &domain, &key, &checksum); err != nil {
			return "", "", err
		}
		// JSON tuples avoid collisions between colon-bearing object keys.
		identity, err := json.Marshal([]string{enrollment, scope, domain, key})
		if err != nil {
			return "", "", err
		}
		objectChecksums[string(identity)] = checksum
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if err := rows.Close(); err != nil {
		return "", "", err
	}
	tables, err := json.Marshal(tableChecksums)
	if err != nil {
		return "", "", err
	}
	objects, err := json.Marshal(objectChecksums)
	return string(tables), string(objects), err
}

func (s *Store) tenantTableContentChecksum(ctx context.Context, table, tenantID string) (string, error) {
	quoted, err := quotePostgresIdentifier(table)
	if err != nil {
		return "", err
	}
	rows, err := s.query(ctx, `SELECT * FROM `+quoted+` WHERE tenant_id = ?`, tenantID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	order := make([]int, len(columns))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return columns[order[i]] < columns[order[j]] })
	canonicalColumns := make([]string, len(columns))
	for i, position := range order {
		canonicalColumns[i] = columns[position]
	}
	// Retain only a fixed-size hash per row, rather than every answer or object
	// payload. Sorting hashes makes row order independent of plans and collation,
	// preserves duplicate multiplicity and avoids ambiguous composite SQL keys.
	var rowHashes [][sha256.Size]byte
	values, dest := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return "", err
		}
		cells := make([]tenantChecksumValue, len(columns))
		for i, position := range order {
			cells[i], err = canonicalTenantChecksumValue(values[position])
			if err != nil {
				return "", fmt.Errorf("column %s: %w", columns[position], err)
			}
		}
		encoded, err := json.Marshal(cells)
		if err != nil {
			return "", err
		}
		rowHashes = append(rowHashes, sha256.Sum256(encoded))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	sort.Slice(rowHashes, func(i, j int) bool {
		return string(rowHashes[i][:]) < string(rowHashes[j][:])
	})
	header, err := json.Marshal([]any{tenantChecksumFormat, table, tenantID, canonicalColumns})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write(header)
	_, _ = h.Write([]byte{0})
	for _, sum := range rowHashes {
		_, _ = h.Write(sum[:])
	}
	return fmt.Sprintf("%s:%d:%s", tenantChecksumFormat, len(rowHashes), hex.EncodeToString(h.Sum(nil))), nil
}

type tenantChecksumValue struct {
	Kind  string `json:"type"`
	Value string `json:"value,omitempty"`
}

func canonicalTenantChecksumValue(value any) (tenantChecksumValue, error) {
	switch v := value.(type) {
	case nil:
		return tenantChecksumValue{Kind: "null"}, nil
	case string:
		return tenantChecksumValue{Kind: "text", Value: v}, nil
	case []byte:
		return tenantChecksumValue{Kind: "bytes", Value: base64.StdEncoding.EncodeToString(v)}, nil
	case int64:
		return tenantChecksumValue{Kind: "integer", Value: strconv.FormatInt(v, 10)}, nil
	case float64:
		if v == 0 {
			v = 0 // Numeric zero has one representation, including negative zero.
		}
		return tenantChecksumValue{Kind: "real", Value: strconv.FormatFloat(v, 'g', -1, 64)}, nil
	case bool:
		return tenantChecksumValue{Kind: "boolean", Value: strconv.FormatBool(v)}, nil
	case time.Time:
		return tenantChecksumValue{Kind: "timestamp", Value: v.UTC().Format(time.RFC3339Nano)}, nil
	default:
		return tenantChecksumValue{}, fmt.Errorf("unsupported SQL value type %T", value)
	}
}
