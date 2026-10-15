package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// This projection deliberately does not change session identity, encryption AAD,
// ciphertext, object versions or timestamps. Zero is unknown, including for a
// session whose authenticated frontmatter really has no domain.
const sessionNarrativeIndexMigration = `
ALTER TABLE narrative_objects ADD COLUMN session_domain_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE narrative_objects ADD COLUMN session_sort_key TEXT NOT NULL DEFAULT '';
ALTER TABLE narrative_objects ADD COLUMN session_indexed_version BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_narrative_sessions_domain
 ON narrative_objects(tenant_id, learner_id, session_domain_digest, session_sort_key DESC, enrollment_id, object_key)
 WHERE scope = 'session';
CREATE INDEX idx_narrative_sessions_recent
 ON narrative_objects(tenant_id, learner_id, session_sort_key DESC, enrollment_id, object_key) WHERE scope = 'session';
CREATE INDEX idx_narrative_sessions_pending
 ON narrative_objects(tenant_id, learner_id, enrollment_id, domain_id, object_key)
 WHERE scope = 'session' AND session_indexed_version <> version;
CREATE INDEX idx_narrative_sessions_pending_tenant
 ON narrative_objects(tenant_id, enrollment_id, domain_id, object_key)
 WHERE scope = 'session' AND session_indexed_version <> version;
`

var _ memory.SessionNarrativeIndex = (*Store)(nil)

func (s *Store) withSessionIndexScope(ctx context.Context, scope models.TenantScope, fn func(context.Context, *Store) error) error {
	return s.withSessionIndexTransaction(ctx, scope, false, fn)
}

func (s *Store) withSessionIndexReadScope(ctx context.Context, scope models.TenantScope, fn func(context.Context, *Store) error) error {
	return s.withSessionIndexTransaction(ctx, scope, true, fn)
}

func (s *Store) withSessionIndexTransaction(ctx context.Context, scope models.TenantScope, readOnly bool, fn func(context.Context, *Store) error) error {
	if err := scope.Validate(); err != nil {
		return memory.ErrNarrativeTenantScope
	}
	// A scoped store without the originating context cannot route the global
	// narrative backend through this transaction. Refuse instead of opening a
	// second pool/transaction or trusting the memory context bridge alone.
	if s.root == nil {
		bound, ok := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext)
		if !ok || bound.tx != s.db || bound.scope != scope || s.tenantScope == nil || *s.tenantScope != scope {
			return memory.ErrNarrativeTenantScope
		}
	}
	transaction := s.WithTenantTx
	if readOnly {
		transaction = s.WithTenantReadTx
	}
	return transaction(ctx, scope, func(txCtx context.Context, scoped storeport.Store) error {
		return fn(txCtx, scoped.(*Store))
	})
}

// ListRecentSessionNarratives includes the learner's recovery enrollments as
// well as native enrollments. Session frontmatter, not enrollment resolution,
// determines the domain. A missing/stale index never means an empty history.
func (s *Store) ListRecentSessionNarratives(ctx context.Context, scope models.TenantScope, learnerID, domainID string, limit int) ([]memory.NarrativeMetadata, error) {
	if learnerID == "" || (scope.LearnerID != "" && scope.LearnerID != learnerID) || limit < 1 || limit > 3 {
		return nil, memory.ErrNarrativeTenantScope
	}
	var result []memory.NarrativeMetadata
	err := s.withSessionIndexReadScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		var learner string
		if err := txs.queryRow(txCtx, `SELECT id FROM learners WHERE tenant_id=? AND id=?`, scope.TenantID, learnerID).Scan(&learner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return memory.ErrNarrativeTenantScope
			}
			return fmt.Errorf("memory: validate indexed learner: %w", err)
		}
		// One statement gives the completeness guard and candidates the same
		// PostgreSQL READ COMMITTED snapshot, including old-version writers.
		query := `WITH pending AS (SELECT CASE WHEN EXISTS (SELECT 1 FROM narrative_objects
			WHERE tenant_id=? AND learner_id=? AND scope='session'
			AND session_indexed_version<>version) THEN 1 ELSE 0 END AS incomplete),
			recent AS (SELECT tenant_id,enrollment_id,learner_id,domain_id,object_key,version,checksum,size_bytes,updated_at,session_sort_key
			FROM narrative_objects WHERE tenant_id=? AND learner_id=? AND scope='session'`
		args := []any{scope.TenantID, learnerID, scope.TenantID, learnerID}
		if domainID != "" {
			query += ` AND session_domain_digest=?`
			args = append(args, memory.SessionDomainDigest(domainID))
		}
		query += ` ORDER BY session_sort_key DESC,enrollment_id ASC,object_key ASC LIMIT ?)
			SELECT pending.incomplete,COALESCE(recent.tenant_id,''),COALESCE(recent.enrollment_id,''),COALESCE(recent.learner_id,''),
			COALESCE(recent.domain_id,''),COALESCE(recent.object_key,''),COALESCE(recent.version,0),COALESCE(recent.checksum,''),
			COALESCE(recent.size_bytes,0),recent.updated_at FROM pending LEFT JOIN recent ON 1=1
			ORDER BY recent.session_sort_key DESC,recent.enrollment_id ASC,recent.object_key ASC`
		args = append(args, limit)
		rows, err := txs.query(txCtx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		result = make([]memory.NarrativeMetadata, 0, limit)
		for rows.Next() {
			var item memory.NarrativeMetadata
			var updated flexTime
			var pending int
			item.Scope = memory.ScopeSession
			if err := rows.Scan(&pending, &item.TenantID, &item.EnrollmentID, &item.LearnerID, &item.DomainID, &item.Key, &item.Version, &item.Checksum, &item.SizeBytes, &updated); err != nil {
				return err
			}
			if pending != 0 {
				return memory.ErrSessionIndexIncomplete
			}
			if item.TenantID == "" {
				continue
			}
			if err := item.NarrativeKey.Validate(); err != nil {
				return memory.ErrNarrativeCorrupt
			}
			item.UpdatedAt = updated.Time.UTC()
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

type SessionIndexBackfillReport struct {
	Scanned    int64 `json:"scanned"`
	Indexed    int64 `json:"indexed"`
	Unresolved int64 `json:"unresolved"`
	// Intermediate pages and interrupted counts use -1. Counting only after
	// the final page keeps a successful repair linear in pending objects.
	Remaining      int64 `json:"remaining"`
	RemainingKnown bool  `json:"remaining_known"`
}

type sessionIndexEnvelope struct {
	ciphertext, keyID, checksum string
	aadVersion                  int
	version, size               int64
}

// BackfillSessionNarrativeIndex repairs one page. The opaque canonical cursor
// advances even past corrupt objects; only successfully authenticated CASes
// checkpoint progress. Restarting from an empty cursor retries unresolved rows.
// Call outside business transactions: every read/write is independently scoped,
// and no connection or SQL lock is held during decryption/parsing.
func (s *Store) BackfillSessionNarrativeIndex(ctx context.Context, scope models.TenantScope, after string, limit int) (SessionIndexBackfillReport, string, error) {
	report := SessionIndexBackfillReport{Remaining: -1}
	if s.root == nil || limit < 1 || limit > 100 || s.secretKeyring == nil {
		return report, "", fmt.Errorf("memory: invalid session index maintenance configuration")
	}
	if _, exists := ctx.Value(tenantTransactionContextKey{}).(*tenantTransactionContext); exists {
		return report, "", fmt.Errorf("memory: index repair requires an independent context")
	}
	cursor := [3]string{}
	if after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || json.Unmarshal(raw, &cursor) != nil {
			return report, "", fmt.Errorf("memory: invalid session index cursor")
		}
	}
	var keys []memory.NarrativeKey
	err := s.withSessionIndexReadScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		query, args := sessionIndexPageQuery(scope, cursor, limit)
		rows, err := txs.query(txCtx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			key := memory.NarrativeKey{TenantID: scope.TenantID, Scope: memory.ScopeSession}
			if err := rows.Scan(&key.EnrollmentID, &key.LearnerID, &key.DomainID, &key.Key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	if err != nil {
		return report, "", err
	}
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		last := keys[len(keys)-1]
		raw, _ := json.Marshal([3]string{last.EnrollmentID, last.DomainID, last.Key})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		return report, "", err
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return report, "", err
		}
		report.Scanned++
		envelope, err := s.readSessionIndexEnvelope(ctx, scope, key, limits.MaxFileBytes)
		if err != nil {
			if errors.Is(err, memory.ErrNarrativeCorrupt) {
				report.Unresolved++
				continue
			}
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return report, "", err
		}
		aad := narrativeObjectAAD(key)
		if envelope.aadVersion == 2 {
			aad = narrativeObjectAADv2(key)
		}
		plaintext, err := s.secretKeyring.decryptWithAAD(envelope.ciphertext, aad)
		if err != nil || int64(len(plaintext)) != envelope.size || narrativeChecksum(plaintext) != envelope.checksum {
			report.Unresolved++
			continue
		}
		domainID, err := memory.SessionDomainID(plaintext)
		if err != nil {
			report.Unresolved++
			continue
		}
		changed, err := s.writeSessionIndexEnvelope(ctx, scope, key, envelope, domainID)
		if err != nil {
			return report, "", err
		}
		if changed {
			report.Indexed++
		} else {
			report.Unresolved++
		}
	}
	if next == "" {
		err = s.withSessionIndexReadScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
			query := `SELECT COUNT(*) FROM narrative_objects WHERE tenant_id=? AND scope='session' AND session_indexed_version<>version`
			args := []any{scope.TenantID}
			if scope.LearnerID != "" {
				query += ` AND learner_id=?`
				args = append(args, scope.LearnerID)
			}
			return txs.queryRow(txCtx, query, args...).Scan(&report.Remaining)
		})
		if err == nil {
			report.RemainingKnown = true
		} else {
			report.Remaining = -1
		}
	}
	return report, next, err
}

func (s *Store) readSessionIndexEnvelope(ctx context.Context, scope models.TenantScope, key memory.NarrativeKey, maxBytes int64) (sessionIndexEnvelope, error) {
	var envelope sessionIndexEnvelope
	if key.Validate() != nil {
		return envelope, memory.ErrNarrativeCorrupt
	}
	err := s.withSessionIndexReadScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		// Bound the value materialized into Go even if metadata/ciphertext is
		// corrupt. One envelope at a time bounds memory independently of page.
		return txs.queryRow(txCtx, `SELECT CASE WHEN size_bytes<=? AND LENGTH(ciphertext)<=? THEN ciphertext ELSE '' END,
			key_id,aad_version,version,checksum,size_bytes FROM narrative_objects
			WHERE tenant_id=? AND enrollment_id=? AND learner_id=? AND scope='session' AND domain_id=? AND object_key=?
			AND session_indexed_version<>version`, maxBytes, maxBytes*2+1024,
			key.TenantID, key.EnrollmentID, key.LearnerID, key.DomainID, key.Key).
			Scan(&envelope.ciphertext, &envelope.keyID, &envelope.aadVersion, &envelope.version, &envelope.checksum, &envelope.size)
	})
	if err != nil {
		return envelope, err
	}
	if envelope.ciphertext == "" || envelope.size < 0 || envelope.keyID == "" || integrationSecretKeyID(envelope.ciphertext) != envelope.keyID || (envelope.aadVersion != 1 && envelope.aadVersion != 2) {
		return envelope, memory.ErrNarrativeCorrupt
	}
	return envelope, nil
}

func (s *Store) writeSessionIndexEnvelope(ctx context.Context, scope models.TenantScope, key memory.NarrativeKey, envelope sessionIndexEnvelope, domainID string) (bool, error) {
	changed := false
	sortKey, err := memory.SessionSortKey(key.Key)
	if err != nil {
		return false, err
	}
	err = s.withSessionIndexScope(ctx, scope, func(txCtx context.Context, txs *Store) error {
		result, err := txs.exec(txCtx, `UPDATE narrative_objects SET session_domain_digest=?,session_sort_key=?,session_indexed_version=version
   WHERE tenant_id=? AND enrollment_id=? AND learner_id=? AND scope='session' AND domain_id=? AND object_key=?
   AND version=? AND checksum=? AND ciphertext=? AND key_id=? AND aad_version=? AND size_bytes=?`,
			memory.SessionDomainDigest(domainID), sortKey, key.TenantID, key.EnrollmentID, key.LearnerID, key.DomainID, key.Key, envelope.version, envelope.checksum, envelope.ciphertext, envelope.keyID, envelope.aadVersion, envelope.size)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		changed = n == 1
		return err
	})
	return changed, err
}

// Shared by execution and EXPLAIN regressions, so the tested cursor/index shape
// cannot silently drift from the maintenance query.
func sessionIndexPageQuery(scope models.TenantScope, cursor [3]string, limit int) (string, []any) {
	query := `SELECT enrollment_id,learner_id,domain_id,object_key FROM narrative_objects
  WHERE tenant_id=? AND scope='session' AND session_indexed_version<>version`
	args := []any{scope.TenantID}
	if scope.LearnerID != "" {
		query += ` AND learner_id=?`
		args = append(args, scope.LearnerID)
	}
	query += ` AND (enrollment_id,domain_id,object_key)>(?,?,?) ORDER BY enrollment_id,domain_id,object_key LIMIT ?`
	args = append(args, cursor[0], cursor[1], cursor[2], limit+1)
	return query, args
}
