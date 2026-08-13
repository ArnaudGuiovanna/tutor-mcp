// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/memory"
)

var _ memory.NarrativeStore = (*Store)(nil)

const sqliteCanonicalNarrativeKeyMigration = `
CREATE TABLE narrative_objects_v2 (
    tenant_id TEXT NOT NULL,
    enrollment_id TEXT NOT NULL,
    learner_id TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('memory','memory_pending','session','concept','archive')),
    domain_id TEXT NOT NULL DEFAULT '',
    object_key TEXT NOT NULL DEFAULT '',
    ciphertext TEXT NOT NULL,
    key_id TEXT NOT NULL,
    aad_version INTEGER NOT NULL DEFAULT 1 CHECK (aad_version IN (1,2)),
    version INTEGER NOT NULL CHECK (version > 0),
    checksum TEXT NOT NULL CHECK (LENGTH(checksum) = 64),
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, scope, domain_id, object_key),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
INSERT INTO narrative_objects_v2
    (tenant_id, enrollment_id, learner_id, scope, domain_id, object_key,
     ciphertext, key_id, aad_version, version, checksum, size_bytes, created_at, updated_at)
SELECT tenant_id, enrollment_id, learner_id, scope, domain_id, object_key,
       ciphertext, key_id, 1, version, checksum, size_bytes, created_at, updated_at
FROM narrative_objects;
CREATE TABLE narrative_mutations_v2 (
    tenant_id TEXT NOT NULL,
    enrollment_id TEXT NOT NULL,
    learner_id TEXT NOT NULL REFERENCES learners(id) ON DELETE CASCADE,
    mutation_id TEXT NOT NULL,
    scope TEXT NOT NULL,
    domain_id TEXT NOT NULL DEFAULT '',
    object_key TEXT NOT NULL DEFAULT '',
    mutation_checksum TEXT NOT NULL CHECK (LENGTH(mutation_checksum) = 64),
    result_version INTEGER NOT NULL CHECK (result_version > 0),
    created_at DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, enrollment_id, mutation_id),
    FOREIGN KEY (tenant_id, enrollment_id) REFERENCES enrollments(tenant_id, id)
);
INSERT INTO narrative_mutations_v2
    (tenant_id, enrollment_id, learner_id, mutation_id, scope, domain_id,
     object_key, mutation_checksum, result_version, created_at)
SELECT tenant_id, enrollment_id, learner_id, mutation_id, scope, domain_id,
       object_key, mutation_checksum, result_version, created_at
FROM narrative_mutations;
DROP TABLE narrative_mutations;
DROP TABLE narrative_objects;
ALTER TABLE narrative_objects_v2 RENAME TO narrative_objects;
ALTER TABLE narrative_mutations_v2 RENAME TO narrative_mutations;
CREATE INDEX idx_narrative_objects_list
    ON narrative_objects(tenant_id, enrollment_id, scope, domain_id, object_key DESC);
CREATE INDEX idx_narrative_objects_retention ON narrative_objects(updated_at, tenant_id, enrollment_id);
CREATE INDEX idx_narrative_mutations_created ON narrative_mutations(created_at, tenant_id);
`

const postgresCanonicalNarrativeKeyMigration = `
ALTER TABLE narrative_objects ADD COLUMN aad_version INTEGER NOT NULL DEFAULT 1 CHECK (aad_version IN (1,2));
ALTER TABLE narrative_objects DROP CONSTRAINT narrative_objects_pkey;
ALTER TABLE narrative_objects ADD PRIMARY KEY (tenant_id, enrollment_id, scope, domain_id, object_key);
ALTER TABLE narrative_mutations DROP CONSTRAINT narrative_mutations_pkey;
ALTER TABLE narrative_mutations ADD PRIMARY KEY (tenant_id, enrollment_id, mutation_id);
DROP INDEX IF EXISTS idx_narrative_objects_list;
DROP INDEX IF EXISTS idx_narrative_objects_retention;
DROP INDEX IF EXISTS idx_narrative_mutations_created;
CREATE INDEX idx_narrative_objects_list
    ON narrative_objects(tenant_id, enrollment_id, scope, domain_id, object_key DESC);
CREATE INDEX idx_narrative_objects_retention ON narrative_objects(updated_at, tenant_id, enrollment_id);
CREATE INDEX idx_narrative_mutations_created ON narrative_mutations(created_at, tenant_id);
`

func narrativeObjectAAD(key memory.NarrativeKey) []byte {
	// Length prefixes make the binding unambiguous even when labels contain a
	// separator. The fixed purpose prefix prevents ciphertext transplantation
	// between webhook and narrative columns while using the same managed key.
	return []byte(fmt.Sprintf(
		"tutor-mcp\x00narrative_objects\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s",
		len(key.LearnerID), key.LearnerID,
		len(key.Scope), key.Scope,
		len(key.DomainID), key.DomainID,
		len(key.Key), key.Key,
	))
}

func narrativeObjectAADv2(key memory.NarrativeKey) []byte {
	return []byte(fmt.Sprintf(
		"tutor-mcp\x00narrative_objects_v2\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s\x00%d:%s",
		len(key.TenantID), key.TenantID,
		len(key.EnrollmentID), key.EnrollmentID,
		len(key.LearnerID), key.LearnerID,
		len(key.Scope), key.Scope,
		len(key.DomainID), key.DomainID,
		len(key.Key), key.Key,
	))
}

func (s *Store) canonicalNarrativeKey(ctx context.Context, key memory.NarrativeKey, provision bool) (memory.NarrativeKey, error) {
	if key.TenantID != "" {
		var count int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM enrollments
			WHERE tenant_id = ? AND id = ? AND learner_id = ?`, key.TenantID,
			key.EnrollmentID, key.LearnerID).Scan(&count); err != nil || count != 1 {
			return memory.NarrativeKey{}, fmt.Errorf("memory: canonical enrollment does not own learner")
		}
		return key, nil
	}
	learningScope, err := s.queryLearningScope(ctx, key.LearnerID, key.DomainID, "")
	if errors.Is(err, sql.ErrNoRows) {
		if provision {
			learningScope, err = s.resolveLearningScope(ctx, key.LearnerID, key.DomainID, "")
		} else {
			// A legacy write for an unknown domain is deliberately attached to
			// the explicit recovery enrollment while retaining the original
			// domain label. Subsequent reads must resolve that same enrollment
			// without provisioning state as a side effect.
			learningScope, err = s.queryLearningScope(ctx, key.LearnerID, "", "")
		}
	}
	if err != nil {
		return memory.NarrativeKey{}, err
	}
	key.TenantID, key.EnrollmentID = learningScope.TenantID, learningScope.EnrollmentID
	return key, nil
}

func narrativeChecksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *Store) GetNarrative(ctx context.Context, key memory.NarrativeKey) (*memory.NarrativeObject, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if s.secretKeyring == nil {
		return nil, fmt.Errorf("memory: narrative encryption keyring is not configured")
	}
	canonicalKey, err := s.canonicalNarrativeKey(ctx, key, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, memory.ErrNarrativeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("memory: resolve narrative enrollment: %w", err)
	}
	var ciphertext, keyID, checksum string
	var version, sizeBytes int64
	var aadVersion int
	var updatedAt flexTime
	err = s.queryRow(ctx,
		`SELECT ciphertext, key_id, aad_version, version, checksum, size_bytes, updated_at
		 FROM narrative_objects
		 WHERE tenant_id = ? AND enrollment_id = ? AND learner_id = ?
		 AND scope = ? AND domain_id = ? AND object_key = ?`, canonicalKey.TenantID,
		canonicalKey.EnrollmentID, canonicalKey.LearnerID, string(canonicalKey.Scope),
		canonicalKey.DomainID, canonicalKey.Key,
	).Scan(&ciphertext, &keyID, &aadVersion, &version, &checksum, &sizeBytes, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, memory.ErrNarrativeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("memory: get narrative metadata: %w", err)
	}
	if keyID == "" || integrationSecretKeyID(ciphertext) != keyID {
		return nil, fmt.Errorf("%w: key metadata mismatch", memory.ErrNarrativeCorrupt)
	}
	aad := narrativeObjectAAD(canonicalKey)
	if aadVersion == 2 {
		aad = narrativeObjectAADv2(canonicalKey)
	}
	plaintext, err := s.secretKeyring.decryptWithAAD(ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed", memory.ErrNarrativeCorrupt)
	}
	if int64(len(plaintext)) != sizeBytes || narrativeChecksum(plaintext) != checksum {
		return nil, fmt.Errorf("%w: checksum or size mismatch", memory.ErrNarrativeCorrupt)
	}
	return &memory.NarrativeObject{
		NarrativeKey: canonicalKey,
		Content:      plaintext,
		Version:      version,
		Checksum:     checksum,
		SizeBytes:    sizeBytes,
		UpdatedAt:    updatedAt.Time.UTC(),
	}, nil
}

func (s *Store) CompareAndSwapNarrative(
	ctx context.Context,
	key memory.NarrativeKey,
	expectedVersion int64,
	content string,
	mutationID string,
	mutationFingerprint string,
	limits memory.Limits,
) (*memory.NarrativeObject, bool, error) {
	if err := key.Validate(); err != nil {
		return nil, false, err
	}
	if expectedVersion < 0 {
		return nil, false, fmt.Errorf("memory: expected version cannot be negative")
	}
	if s.secretKeyring == nil {
		return nil, false, fmt.Errorf("memory: narrative encryption keyring is not configured")
	}
	if int64(len(content)) > limits.MaxFileBytes {
		return nil, false, fmt.Errorf("%w: file exceeds %d bytes", memory.ErrQuotaExceeded, limits.MaxFileBytes)
	}
	mutationID = strings.TrimSpace(mutationID)
	if len(mutationID) > 128 {
		return nil, false, fmt.Errorf("memory: mutation ID exceeds 128 bytes")
	}
	if mutationID != "" && (len(mutationFingerprint) != 64 || strings.Trim(mutationFingerprint, "0123456789abcdef") != "") {
		return nil, false, fmt.Errorf("memory: mutation fingerprint must be lowercase SHA-256")
	}
	checksum := narrativeChecksum(content)
	now := time.Now().UTC()
	var result *memory.NarrativeObject
	replayed := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		lockQuery := `SELECT id FROM learners WHERE id = ?`
		if txs.dialect == DialectPostgres {
			lockQuery += ` FOR UPDATE`
		}
		var lockedLearner string
		if err := txs.queryRow(ctx, lockQuery, key.LearnerID).Scan(&lockedLearner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("memory: learner does not exist")
			}
			return fmt.Errorf("memory: lock learner quota: %w", err)
		}
		// The memory package still accepts the legacy learner/domain key while
		// the expand phase is live. Resolve it to the canonical tenant and
		// enrollment before every write; this also provisions an explicit
		// quarantine enrollment for a newly restored legacy learner.
		canonicalKey, err := txs.canonicalNarrativeKey(ctx, key, true)
		if err != nil {
			return fmt.Errorf("memory: resolve narrative enrollment: %w", err)
		}
		ciphertext, err := txs.secretKeyring.encryptWithAAD(content, narrativeObjectAADv2(canonicalKey))
		if err != nil {
			return fmt.Errorf("memory: encrypt narrative: %w", err)
		}
		keyID := integrationSecretKeyID(ciphertext)
		if keyID == "" {
			return fmt.Errorf("memory: encrypted narrative has no key version")
		}

		if mutationID != "" {
			var scope, domainID, objectKey, recordedFingerprint string
			var recordedVersion int64
			err := txs.queryRow(ctx,
				`SELECT scope, domain_id, object_key, mutation_checksum, result_version
				 FROM narrative_mutations WHERE tenant_id = ? AND enrollment_id = ? AND mutation_id = ?`,
				canonicalKey.TenantID, canonicalKey.EnrollmentID, mutationID,
			).Scan(&scope, &domainID, &objectKey, &recordedFingerprint, &recordedVersion)
			switch {
			case err == nil:
				if scope != string(key.Scope) || domainID != key.DomainID || objectKey != key.Key || recordedFingerprint != mutationFingerprint {
					return memory.ErrNarrativeMutationConflict
				}
				result = &memory.NarrativeObject{
					NarrativeKey: canonicalKey, Version: recordedVersion, UpdatedAt: now,
				}
				replayed = true
				return nil
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("memory: load narrative mutation: %w", err)
			}
		}

		var currentVersion, currentSize int64
		err = txs.queryRow(ctx,
			`SELECT version, size_bytes FROM narrative_objects
			 WHERE tenant_id = ? AND enrollment_id = ? AND scope = ? AND domain_id = ? AND object_key = ?`,
			canonicalKey.TenantID, canonicalKey.EnrollmentID, string(key.Scope), key.DomainID, key.Key,
		).Scan(&currentVersion, &currentSize)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("memory: load narrative version: %w", err)
		}
		if (!exists && expectedVersion != 0) || (exists && currentVersion != expectedVersion) {
			return memory.ErrNarrativeVersionConflict
		}

		var objectCount int
		var totalBytes int64
		if err := txs.queryRow(ctx,
			`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
			 FROM narrative_objects WHERE learner_id = ?`, key.LearnerID,
		).Scan(&objectCount, &totalBytes); err != nil {
			return fmt.Errorf("memory: load narrative quota: %w", err)
		}
		prospectiveCount := objectCount
		if !exists {
			prospectiveCount++
		}
		if prospectiveCount > limits.MaxFilesPerLearner {
			return fmt.Errorf("%w: learner object count exceeds %d", memory.ErrQuotaExceeded, limits.MaxFilesPerLearner)
		}
		if prospectiveBytes := totalBytes - currentSize + int64(len(content)); prospectiveBytes > limits.MaxLearnerBytes {
			return fmt.Errorf("%w: learner memory exceeds %d bytes", memory.ErrQuotaExceeded, limits.MaxLearnerBytes)
		}

		newVersion := int64(1)
		if exists {
			newVersion = currentVersion + 1
			updated, err := txs.exec(ctx,
				`UPDATE narrative_objects
				 SET ciphertext = ?, key_id = ?, aad_version = 2, version = ?, checksum = ?, size_bytes = ?, updated_at = ?
				 WHERE tenant_id = ? AND enrollment_id = ? AND scope = ? AND domain_id = ? AND object_key = ? AND version = ?`,
				ciphertext, keyID, newVersion, checksum, len(content), now,
				canonicalKey.TenantID, canonicalKey.EnrollmentID, string(key.Scope), key.DomainID, key.Key, currentVersion,
			)
			if err != nil {
				return fmt.Errorf("memory: update narrative object: %w", err)
			}
			if affected, _ := updated.RowsAffected(); affected != 1 {
				return memory.ErrNarrativeVersionConflict
			}
		} else {
			if _, err := txs.exec(ctx,
				`INSERT INTO narrative_objects
				    (tenant_id, enrollment_id, learner_id, scope, domain_id, object_key, ciphertext, key_id, aad_version,
				     version, checksum, size_bytes, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 2, 1, ?, ?, ?, ?)`,
				canonicalKey.TenantID, canonicalKey.EnrollmentID,
				key.LearnerID, string(key.Scope), key.DomainID, key.Key,
				ciphertext, keyID, checksum, len(content), now, now,
			); err != nil {
				return fmt.Errorf("memory: create narrative object: %w", err)
			}
		}
		if mutationID != "" {
			if _, err := txs.exec(ctx,
				`INSERT INTO narrative_mutations
				    (tenant_id, enrollment_id, learner_id, mutation_id, scope, domain_id, object_key,
				     mutation_checksum, result_version, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				canonicalKey.TenantID, canonicalKey.EnrollmentID,
				key.LearnerID, mutationID, string(key.Scope), key.DomainID, key.Key,
				mutationFingerprint, newVersion, now,
			); err != nil {
				return fmt.Errorf("memory: record narrative mutation: %w", err)
			}
		}
		result = &memory.NarrativeObject{
			NarrativeKey: canonicalKey, Content: content, Version: newVersion,
			Checksum: checksum, SizeBytes: int64(len(content)), UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

func (s *Store) ListNarratives(ctx context.Context, learnerID string, scope memory.Scope, domainID string, limit int) ([]memory.NarrativeMetadata, error) {
	if strings.TrimSpace(learnerID) == "" {
		return nil, fmt.Errorf("memory: learner_id is required")
	}
	if limit < 1 || limit > 100_000 {
		return nil, fmt.Errorf("memory: narrative list limit must be between 1 and 100000")
	}
	probe := memory.NarrativeKey{LearnerID: learnerID, Scope: scope, DomainID: domainID, Key: "probe"}
	if scope == memory.ScopeMemory || scope == memory.ScopeMemoryPending {
		probe.Key = ""
	} else if scope == memory.ScopeSession {
		probe.Key = "1970-01-01T00-00-00Z"
	}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx,
		`SELECT object_key, version, checksum, size_bytes, updated_at
		 FROM narrative_objects
		 WHERE learner_id = ? AND scope = ? AND domain_id = ?
		 ORDER BY object_key DESC LIMIT ?`,
		learnerID, string(scope), domainID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: list narratives: %w", err)
	}
	defer rows.Close()
	items := make([]memory.NarrativeMetadata, 0)
	for rows.Next() {
		item := memory.NarrativeMetadata{NarrativeKey: memory.NarrativeKey{LearnerID: learnerID, Scope: scope, DomainID: domainID}}
		var updatedAt flexTime
		if err := rows.Scan(&item.Key, &item.Version, &item.Checksum, &item.SizeBytes, &updatedAt); err != nil {
			return nil, fmt.Errorf("memory: scan narrative metadata: %w", err)
		}
		item.UpdatedAt = updatedAt.Time.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate narrative metadata: %w", err)
	}
	return items, nil
}

func (s *Store) NarrativeStats(ctx context.Context, learnerID string) (memory.NarrativeStats, error) {
	stats := memory.NarrativeStats{ScopeCounts: make(map[memory.Scope]int)}
	rows, err := s.query(ctx,
		`SELECT scope, COUNT(*), COALESCE(SUM(size_bytes), 0)
		 FROM narrative_objects WHERE learner_id = ? GROUP BY scope`, learnerID,
	)
	if err != nil {
		return stats, fmt.Errorf("memory: narrative stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var count int
		var size int64
		if err := rows.Scan(&scope, &count, &size); err != nil {
			return stats, fmt.Errorf("memory: scan narrative stats: %w", err)
		}
		stats.ScopeCounts[memory.Scope(scope)] = count
		stats.ObjectCount += count
		stats.TotalBytes += size
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("memory: iterate narrative stats: %w", err)
	}
	return stats, nil
}

func (s *Store) DeleteNarrativesBefore(ctx context.Context, cutoff time.Time, apply bool) (int64, int64, int64, error) {
	if cutoff.IsZero() {
		return 0, 0, 0, fmt.Errorf("memory: retention cutoff is required")
	}
	var eligible, held int64
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM narrative_objects
		 WHERE updated_at < ? AND `+retentionHoldClause("narrative_objects", false), cutoff.UTC(),
	).Scan(&eligible); err != nil {
		return 0, 0, 0, fmt.Errorf("memory: count retained narratives: %w", err)
	}
	if err := s.queryRow(ctx,
		`SELECT COUNT(*) FROM narrative_objects
		 WHERE updated_at < ? AND `+retentionHoldClause("narrative_objects", true), cutoff.UTC(),
	).Scan(&held); err != nil {
		return 0, 0, 0, fmt.Errorf("memory: count held narratives: %w", err)
	}
	if !apply || eligible == 0 {
		return eligible, 0, held, nil
	}
	var applied int64
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if _, err := txs.exec(ctx,
			`DELETE FROM narrative_mutations WHERE created_at < ? AND `+retentionHoldClause("narrative_mutations", false), cutoff.UTC(),
		); err != nil {
			return fmt.Errorf("memory: delete narrative mutations: %w", err)
		}
		result, err := txs.exec(ctx,
			`DELETE FROM narrative_objects WHERE updated_at < ? AND `+retentionHoldClause("narrative_objects", false), cutoff.UTC(),
		)
		if err != nil {
			return fmt.Errorf("memory: delete narrative objects: %w", err)
		}
		applied, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return eligible, 0, held, err
	}
	return eligible, applied, held, nil
}

// RotateNarrativeSecrets authenticates every object and re-encrypts old-key
// envelopes with the current key without changing semantic versions or
// updated_at. All ciphertexts are prepared before the transaction, so a bad
// row or missing retained key leaves the database unchanged.
func (s *Store) RotateNarrativeSecrets(ctx context.Context) (int64, error) {
	if s.secretKeyring == nil {
		return 0, fmt.Errorf("memory: narrative encryption keyring is not configured")
	}
	rows, err := s.query(ctx,
		`SELECT tenant_id, enrollment_id, learner_id, scope, domain_id, object_key,
		        ciphertext, key_id, aad_version, version, checksum, size_bytes
		 FROM narrative_objects
		 ORDER BY tenant_id, enrollment_id, scope, domain_id, object_key`,
	)
	if err != nil {
		return 0, fmt.Errorf("memory: list narrative envelopes: %w", err)
	}
	type rotation struct {
		key                          memory.NarrativeKey
		oldCiphertext, newCiphertext string
		version                      int64
	}
	var rotations []rotation
	for rows.Next() {
		var key memory.NarrativeKey
		var scope, ciphertext, keyID, checksum string
		var version, sizeBytes int64
		var aadVersion int
		if err := rows.Scan(
			&key.TenantID, &key.EnrollmentID, &key.LearnerID, &scope,
			&key.DomainID, &key.Key, &ciphertext, &keyID, &aadVersion,
			&version, &checksum, &sizeBytes,
		); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("memory: scan narrative envelope: %w", err)
		}
		key.Scope = memory.Scope(scope)
		if err := key.Validate(); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("%w: invalid key metadata", memory.ErrNarrativeCorrupt)
		}
		if keyID == "" || integrationSecretKeyID(ciphertext) != keyID {
			_ = rows.Close()
			return 0, fmt.Errorf("%w: key metadata mismatch", memory.ErrNarrativeCorrupt)
		}
		aad := narrativeObjectAAD(key)
		if aadVersion == 2 {
			aad = narrativeObjectAADv2(key)
		}
		plaintext, err := s.secretKeyring.decryptWithAAD(ciphertext, aad)
		if err != nil || narrativeChecksum(plaintext) != checksum || int64(len(plaintext)) != sizeBytes {
			_ = rows.Close()
			return 0, fmt.Errorf("%w: authentication or checksum failed", memory.ErrNarrativeCorrupt)
		}
		if keyID == s.secretKeyring.currentID && aadVersion == 2 {
			continue
		}
		rotated, err := s.secretKeyring.encryptWithAAD(plaintext, narrativeObjectAADv2(key))
		if err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("memory: encrypt rotated narrative: %w", err)
		}
		rotations = append(rotations, rotation{key: key, oldCiphertext: ciphertext, newCiphertext: rotated, version: version})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("memory: iterate narrative envelopes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("memory: close narrative envelopes: %w", err)
	}
	if len(rotations) == 0 {
		return 0, nil
	}
	err = s.inTx(ctx, nil, func(txs *Store) error {
		for _, item := range rotations {
			result, err := txs.exec(ctx,
				`UPDATE narrative_objects SET ciphertext = ?, key_id = ?, aad_version = 2
				 WHERE tenant_id = ? AND enrollment_id = ? AND learner_id = ?
				   AND scope = ? AND domain_id = ? AND object_key = ?
				   AND version = ? AND ciphertext = ?`,
				item.newCiphertext, s.secretKeyring.currentID,
				item.key.TenantID, item.key.EnrollmentID,
				item.key.LearnerID, string(item.key.Scope), item.key.DomainID, item.key.Key,
				item.version, item.oldCiphertext,
			)
			if err != nil {
				return fmt.Errorf("memory: rotate narrative envelope: %w", err)
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return memory.ErrNarrativeVersionConflict
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int64(len(rotations)), nil
}
