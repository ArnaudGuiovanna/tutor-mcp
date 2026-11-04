// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Additive on both supported databases; existing delivered jobs remain
// recoverable. Intermediate prose is always encrypted with job-specific AAD.
const consolidationMaintenanceMigration = `
ALTER TABLE pending_consolidations ADD COLUMN maintenance_token TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_consolidations ADD COLUMN maintenance_lease_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pending_consolidations ADD COLUMN maintenance_manifest TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_consolidations ADD COLUMN maintenance_state TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_consolidations ADD COLUMN maintenance_last_token TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_consolidations ADD COLUMN maintenance_last_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE pending_consolidations ADD COLUMN maintenance_last_completed INTEGER NOT NULL DEFAULT 0;
`

type maintenanceProgress struct {
	CursorKey        string    `json:"cursor_key"`
	CursorEnrollment string    `json:"cursor_enrollment"`
	Consumed         int       `json:"consumed"`
	StartedAt        time.Time `json:"started_at"`
	Summary          string    `json:"summary"`
	ArchiveVersion   int64     `json:"archive_version"`
	ArchiveChecksum  string    `json:"archive_checksum"`
}

type maintenanceObject struct {
	Key      memory.NarrativeKey `json:"key"`
	Version  int64               `json:"version"`
	Checksum string              `json:"checksum"`
}

type maintenanceManifest struct {
	Sources  []maintenanceObject `json:"sources"`
	Targets  []maintenanceObject `json:"targets"`
	Archive  maintenanceObject   `json:"archive"`
	Next     maintenanceProgress `json:"next"`
	EndKey   string              `json:"end_key"`
	StartKey string              `json:"start_key"`
	HasMore  bool                `json:"has_more"`
}

func maintenanceAAD(scope models.TenantScope, id int64) []byte {
	return []byte(fmt.Sprintf("tutor-mcp\x00consolidation-maintenance-v1\x00%d:%s\x00%d:%s\x00%d", len(scope.TenantID), scope.TenantID, len(scope.LearnerID), scope.LearnerID, id))
}

func maintenanceBounds(kind, key string) (time.Time, time.Time, error) {
	var start time.Time
	var err error
	switch kind {
	case "monthly":
		start, err = time.Parse("2006-01", key)
		if err == nil {
			return start, start.AddDate(0, 1, 0), nil
		}
	case "quarterly":
		if len(key) == 7 && key[4:6] == "-Q" {
			year, yearErr := strconv.Atoi(key[:4])
			quarter, quarterErr := strconv.Atoi(key[6:])
			if yearErr == nil && quarterErr == nil && year >= 1 && quarter >= 1 && quarter <= 4 {
				start = time.Date(year, time.Month((quarter-1)*3+1), 1, 0, 0, 0, 0, time.UTC)
				return start, start.AddDate(0, 3, 0), nil
			}
		}
	case "annual":
		start, err = time.Parse("2006", key)
		if err == nil {
			return start, start.AddDate(1, 0, 0), nil
		}
	}
	return time.Time{}, time.Time{}, fmt.Errorf("invalid consolidation period")
}

func (s *Store) lockMaintenanceLearner(ctx context.Context, scope models.TenantScope) error {
	if scope.LearnerID == "" {
		return fmt.Errorf("memory maintenance requires a learner")
	}
	query := `SELECT id FROM learners WHERE id = ? AND tenant_id = ?`
	if s.dialect == DialectPostgres {
		query += ` FOR UPDATE`
	}
	var id string
	return s.queryRow(ctx, query, scope.LearnerID, scope.TenantID).Scan(&id)
}

func (s *Store) readMaintenanceProgress(scope models.TenantScope, id int64, ciphertext string, now time.Time) (maintenanceProgress, error) {
	p := maintenanceProgress{StartedAt: now.UTC()}
	if ciphertext == "" {
		return p, nil
	}
	plain, err := s.secretKeyring.decryptWithAAD(ciphertext, maintenanceAAD(scope, id))
	if err != nil {
		return p, fmt.Errorf("decrypt maintenance checkpoint: %w", err)
	}
	if len(plain) > models.MemoryMaintenanceMaxBatchBytes {
		return p, models.ErrMemoryMaintenanceBudget
	}
	if err := json.Unmarshal([]byte(plain), &p); err != nil {
		return p, fmt.Errorf("invalid maintenance checkpoint")
	}
	return p, nil
}

// A changed/deleted already-consumed source invalidates the accumulated
// summary. Restart from current sources instead of silently completing an
// archive with missing or stale history.
func (s *Store) maintenancePrefixValid(ctx context.Context, scope models.TenantScope, startKey string, p maintenanceProgress) (bool, error) {
	if p.CursorKey == "" {
		return true, nil
	}
	var count, changed int
	err := s.queryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN updated_at > ? THEN 1 ELSE 0 END),0)
		FROM narrative_objects WHERE tenant_id=? AND learner_id=? AND scope='session' AND object_key>=?
		AND (object_key<? OR (object_key=? AND enrollment_id<=?))`,
		p.StartedAt, scope.TenantID, scope.LearnerID, startKey, p.CursorKey, p.CursorKey, p.CursorEnrollment).Scan(&count, &changed)
	return count == p.Consumed && changed == 0, err
}

func maintenanceObjectID(key memory.NarrativeKey) string {
	return string(key.Scope) + "\x00" + key.DomainID + "\x00" + key.Key
}

func (s *Store) maintenanceRead(ctx context.Context, key memory.NarrativeKey) (maintenanceObject, models.MemoryMaintenanceItem, error) {
	item := models.MemoryMaintenanceItem{Scope: string(key.Scope), Key: key.Key, DomainID: key.DomainID}
	canonical, err := s.canonicalNarrativeKey(ctx, key, true)
	if err != nil {
		return maintenanceObject{}, item, err
	}
	meta := maintenanceObject{Key: canonical}
	var size int64
	err = s.queryRow(ctx, `SELECT size_bytes FROM narrative_objects WHERE tenant_id=? AND enrollment_id=? AND scope=? AND domain_id=? AND object_key=?`,
		canonical.TenantID, canonical.EnrollmentID, string(canonical.Scope), canonical.DomainID, canonical.Key).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return meta, item, nil
	}
	if err != nil {
		return meta, item, err
	}
	if size > models.MemoryMaintenanceMaxObjectBytes {
		return meta, item, models.ErrMemoryMaintenanceBudget
	}
	object, err := s.GetNarrative(ctx, canonical)
	if err != nil {
		return meta, item, err
	}
	if len(object.Content) > models.MemoryMaintenanceMaxObjectBytes {
		return meta, item, models.ErrMemoryMaintenanceBudget
	}
	meta.Version, meta.Checksum = object.Version, object.Checksum
	item.Content = object.Content
	return meta, item, nil
}

// ClaimMemoryMaintenance owns at most one bounded batch for one learner. It
// never calls a model or reads all narratives into memory.
func (s *Store) ClaimMemoryMaintenance(ctx context.Context, scope models.TenantScope, requestKey string, now time.Time) (*models.MemoryMaintenanceBatch, error) {
	result := &models.MemoryMaintenanceBatch{Status: "idle"}
	if len(requestKey) < 1 || len(requestKey) > 128 || strings.IndexFunc(requestKey, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r))
	}) >= 0 {
		return nil, fmt.Errorf("a safe idempotency_key is required")
	}
	if s.secretKeyring == nil {
		return nil, fmt.Errorf("maintenance narrative encryption unavailable")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) (resultErr error) {
		txs := scoped.(*Store)
		if err := txs.lockMaintenanceLearner(ctx, scope); err != nil {
			return err
		}
		const claimNamespace = "memory_maintenance_claim"
		fingerprint := narrativeChecksum("bounded-memory-maintenance-claim-v1")
		cached, execute, err := txs.ClaimIdempotencyKey(ctx, scope.LearnerID, claimNamespace, requestKey, fingerprint, now)
		if err != nil {
			return err
		}
		if !execute {
			var receipt models.MemoryMaintenanceBatch
			if json.Unmarshal([]byte(cached), &receipt) != nil {
				return fmt.Errorf("invalid maintenance claim receipt")
			}
			result, err = txs.replayMaintenanceClaim(ctx, scope, &receipt, now)
			return err
		}
		defer func() {
			if resultErr != nil {
				return
			}
			// Replays store only a reference. Raw prose and usable expired
			// leases must never be replayed by the generic tool-result cache.
			receipt := *result
			receipt.Sessions = nil
			receipt.Snapshots = nil
			receipt.ArchiveSummary = ""
			encoded, err := json.Marshal(receipt)
			if err != nil {
				resultErr = err
				return
			}
			resultErr = txs.CompleteIdempotencyKey(ctx, scope.LearnerID, claimNamespace, requestKey, fingerprint, string(encoded), now)
		}()
		var active int
		if err := txs.queryRow(ctx, `SELECT COUNT(*) FROM pending_consolidations WHERE learner_id=? AND tenant_id=? AND status='delivered' AND maintenance_token<>'' AND maintenance_lease_ms>?`, scope.LearnerID, scope.TenantID, now.UnixMilli()).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			result.Status = "busy"
			return nil
		}
		var id int64
		var kind, key, state string
		err = txs.queryRow(ctx, `SELECT id,period_type,period_key,maintenance_state FROM pending_consolidations
			WHERE learner_id=? AND tenant_id=? AND (status='pending' OR (status='delivered' AND
			((maintenance_token<>'' AND maintenance_lease_ms<=?) OR (maintenance_token='' AND delivered_at<?))))
			ORDER BY detected_at,id LIMIT 1`, scope.LearnerID, scope.TenantID, now.UnixMilli(), now.Add(-30*time.Minute)).Scan(&id, &kind, &key, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		start, end, err := maintenanceBounds(kind, key)
		if err != nil {
			return err
		}
		const layout = "2006-01-02T15-04-05Z"
		progress, err := txs.readMaintenanceProgress(scope, id, state, now)
		if err != nil {
			return err
		}
		valid, err := txs.maintenancePrefixValid(ctx, scope, start.Format(layout), progress)
		if err != nil {
			return err
		}
		if !valid {
			progress = maintenanceProgress{StartedAt: now.UTC()}
		}
		archive, archiveItem, err := txs.maintenanceRead(ctx, memory.NarrativeKey{LearnerID: scope.LearnerID, Scope: memory.ScopeArchive, Key: key})
		if err != nil {
			return err
		}
		// Preserve an existing archive and restart the cumulative projection
		// if it was edited between batches. A lease only protects one batch.
		if progress.ArchiveVersion != archive.Version || progress.ArchiveChecksum != archive.Checksum {
			progress = maintenanceProgress{StartedAt: now.UTC()}
		}
		if progress.Consumed == 0 {
			progress.Summary = archiveItem.Content
			progress.ArchiveVersion, progress.ArchiveChecksum = archive.Version, archive.Checksum
		}
		manifest := maintenanceManifest{StartKey: start.Format(layout), EndKey: end.Format(layout), Next: progress}
		// The extra metadata row detects continuation; no plaintext is fetched
		// for it and no source is dropped at the page boundary.
		rows, err := txs.query(ctx, `SELECT enrollment_id,object_key FROM narrative_objects
			WHERE tenant_id=? AND learner_id=? AND scope='session' AND object_key>=? AND object_key<?
			AND (object_key>? OR (object_key=? AND enrollment_id>?)) ORDER BY object_key,enrollment_id LIMIT ?`,
			scope.TenantID, scope.LearnerID, manifest.StartKey, manifest.EndKey, progress.CursorKey, progress.CursorKey, progress.CursorEnrollment, models.MemoryMaintenanceMaxSessions+1)
		if err != nil {
			return err
		}
		var keys []memory.NarrativeKey
		for rows.Next() {
			k := memory.NarrativeKey{TenantID: scope.TenantID, LearnerID: scope.LearnerID, Scope: memory.ScopeSession}
			if err := rows.Scan(&k.EnrollmentID, &k.Key); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		manifest.HasMore = len(keys) > models.MemoryMaintenanceMaxSessions
		if manifest.HasMore {
			keys = keys[:models.MemoryMaintenanceMaxSessions]
		}
		moreAfterPage := manifest.HasMore
		initialCount := len(keys)
		// Shrink the prefix when combined sources/notes exceed the batch
		// budget. Never skip a source or advance its cursor without a commit.
		for {
			manifest.Sources = nil
			manifest.Targets = nil
			manifest.Next = progress
			manifest.HasMore = moreAfterPage || len(keys) < initialCount
			result.Sessions = nil
			result.Snapshots = nil
			tooManyConcepts := false
			concepts := map[string]memory.NarrativeKey{}
			for _, key := range keys {
				meta, item, err := txs.maintenanceRead(ctx, key)
				if err != nil {
					return err
				}
				manifest.Sources = append(manifest.Sources, meta)
				result.Sessions = append(result.Sessions, item)
				payload, err := memory.ParseSessionPayload(time.Time{}, item.Content)
				if err != nil {
					return err
				}
				domain, err := memory.SessionDomainID(item.Content)
				if err != nil {
					return err
				}
				result.Sessions[len(result.Sessions)-1].DomainID = domain
				for _, slug := range memory.ConceptsFromSessions([]memory.SessionPayload{payload}) {
					key := memory.NarrativeKey{LearnerID: scope.LearnerID, Scope: memory.ScopeConcept, DomainID: domain, Key: slug}
					concepts[maintenanceObjectID(key)] = key
					if len(concepts) > models.MemoryMaintenanceMaxConcepts {
						tooManyConcepts = true
						break
					}
				}
				manifest.Next.CursorKey, manifest.Next.CursorEnrollment = key.Key, key.EnrollmentID
				manifest.Next.Consumed++
			}
			if tooManyConcepts {
				if len(keys) <= 1 {
					return models.ErrMemoryMaintenanceBudget
				}
				keys = keys[:len(keys)/2]
				continue
			}
			targets := []memory.NarrativeKey{{LearnerID: scope.LearnerID, Scope: memory.ScopeMemory}, {LearnerID: scope.LearnerID, Scope: memory.ScopeMemoryPending}}
			var ordered []string
			for name := range concepts {
				ordered = append(ordered, name)
			}
			sort.Strings(ordered)
			for _, name := range ordered {
				targets = append(targets, concepts[name])
			}
			for _, key := range targets {
				meta, item, err := txs.maintenanceRead(ctx, key)
				if err != nil {
					return err
				}
				manifest.Targets = append(manifest.Targets, meta)
				result.Snapshots = append(result.Snapshots, item)
			}
			manifest.Archive = archive
			result.Status, result.JobID, result.PeriodType, result.PeriodKey = "claimed", id, kind, key
			result.ClaimToken = rand.Text()
			expires := now.Add(models.MemoryMaintenanceLease)
			result.LeaseExpiresAt = &expires
			result.HasMore, result.ArchiveSummary = manifest.HasMore, progress.Summary
			encoded, err := json.Marshal(result)
			if err != nil {
				return err
			}
			if len(encoded) > models.MemoryMaintenanceMaxBatchBytes {
				if len(keys) <= 1 {
					return models.ErrMemoryMaintenanceBudget
				}
				keys = keys[:len(keys)/2]
				continue
			}
			break
		}
		// A checkpoint contains summary prose; the claim manifest contains only
		// source/target versions and a cursor, never that prose.
		manifest.Next.Summary = ""
		encoded, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		progressJSON, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		state, err = txs.secretKeyring.encryptWithAAD(string(progressJSON), maintenanceAAD(scope, id))
		if err != nil {
			return err
		}
		_, err = txs.exec(ctx, `UPDATE pending_consolidations SET status='delivered',delivered_at=?,maintenance_token=?,maintenance_lease_ms=?,maintenance_manifest=?,maintenance_state=? WHERE id=? AND tenant_id=? AND learner_id=?`, now.UTC(), result.ClaimToken, result.LeaseExpiresAt.UnixMilli(), string(encoded), state, id, scope.TenantID, scope.LearnerID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) replayMaintenanceClaim(ctx context.Context, scope models.TenantScope, receipt *models.MemoryMaintenanceBatch, now time.Time) (*models.MemoryMaintenanceBatch, error) {
	if receipt.Status != "claimed" {
		return receipt, nil
	}
	expired := &models.MemoryMaintenanceBatch{Status: "expired", JobID: receipt.JobID}
	var token, status, manifestJSON, state string
	var lease int64
	err := s.queryRow(ctx, `SELECT status,maintenance_token,maintenance_lease_ms,maintenance_manifest,maintenance_state FROM pending_consolidations WHERE id=? AND learner_id=? AND tenant_id=?`, receipt.JobID, scope.LearnerID, scope.TenantID).Scan(&status, &token, &lease, &manifestJSON, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return expired, nil
	}
	if err != nil {
		return nil, err
	}
	if token != receipt.ClaimToken || status != "delivered" || lease <= now.UnixMilli() {
		return expired, nil
	}
	var manifest maintenanceManifest
	if len(manifestJSON) > models.MemoryMaintenanceMaxBatchBytes || json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
		return nil, fmt.Errorf("invalid maintenance manifest")
	}
	if len(manifest.Sources) > models.MemoryMaintenanceMaxSessions || len(manifest.Targets) > models.MemoryMaintenanceMaxConcepts+2 {
		return nil, models.ErrMemoryMaintenanceBudget
	}
	if err := s.checkMaintenanceObject(ctx, manifest.Archive); err != nil {
		return nil, err
	}
	for _, meta := range manifest.Sources {
		if err := s.checkMaintenanceObject(ctx, meta); err != nil {
			return nil, err
		}
		_, item, err := s.maintenanceRead(ctx, meta.Key)
		if err != nil {
			return nil, err
		}
		item.DomainID, err = memory.SessionDomainID(item.Content)
		if err != nil {
			return nil, err
		}
		receipt.Sessions = append(receipt.Sessions, item)
	}
	for _, meta := range manifest.Targets {
		if err := s.checkMaintenanceObject(ctx, meta); err != nil {
			return nil, err
		}
		_, item, err := s.maintenanceRead(ctx, meta.Key)
		if err != nil {
			return nil, err
		}
		receipt.Snapshots = append(receipt.Snapshots, item)
	}
	progress, err := s.readMaintenanceProgress(scope, receipt.JobID, state, now)
	if err != nil {
		return nil, err
	}
	receipt.ArchiveSummary = progress.Summary
	return receipt, nil
}

func (s *Store) checkMaintenanceObject(ctx context.Context, expected maintenanceObject) error {
	actual, err := s.GetNarrative(ctx, expected.Key)
	if errors.Is(err, memory.ErrNarrativeNotFound) && expected.Version == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if actual.Version != expected.Version || actual.Checksum != expected.Checksum {
		return models.ErrMemoryMaintenanceConflict
	}
	return nil
}

// CommitMemoryMaintenance applies the entire required bundle and the queue
// checkpoint in one tenant transaction. No archive or completed marker is
// visible after an interrupted/failed partial write.
func (s *Store) CommitMemoryMaintenance(ctx context.Context, scope models.TenantScope, claimToken, summary string, updates []models.MemoryMaintenanceItem, now time.Time) (models.MemoryMaintenanceResult, error) {
	result := models.MemoryMaintenanceResult{}
	if len(claimToken) < 20 || len(claimToken) > 128 || strings.TrimSpace(summary) == "" {
		return result, fmt.Errorf("claim_token and archive_summary are required")
	}
	if len(summary) > models.MemoryMaintenanceMaxObjectBytes || len(updates) > models.MemoryMaintenanceMaxConcepts+2 {
		return result, models.ErrMemoryMaintenanceBudget
	}
	encoded, err := json.Marshal(struct {
		Summary string
		Updates []models.MemoryMaintenanceItem
	}{summary, updates})
	if err != nil {
		return result, err
	}
	if len(encoded) > models.MemoryMaintenanceMaxBatchBytes {
		return result, models.ErrMemoryMaintenanceBudget
	}
	digest := narrativeChecksum(string(encoded))
	if now.IsZero() {
		now = time.Now().UTC()
	}
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		return result, err
	}
	err = s.WithTenantTx(ctx, scope, func(ctx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.lockMaintenanceLearner(ctx, scope); err != nil {
			return err
		}
		var id, lease int64
		var token, status, manifestJSON, lastToken, lastDigest string
		var lastCompleted int
		err := txs.queryRow(ctx, `SELECT id,status,maintenance_token,maintenance_lease_ms,maintenance_manifest,maintenance_last_token,maintenance_last_digest,maintenance_last_completed
			FROM pending_consolidations WHERE learner_id=? AND tenant_id=? AND (maintenance_token=? OR maintenance_last_token=?) LIMIT 1`, scope.LearnerID, scope.TenantID, claimToken, claimToken).Scan(&id, &status, &token, &lease, &manifestJSON, &lastToken, &lastDigest, &lastCompleted)
		if errors.Is(err, sql.ErrNoRows) {
			return models.ErrMemoryMaintenanceLease
		}
		if err != nil {
			return err
		}
		if lastToken == claimToken {
			if lastDigest != digest {
				return memory.ErrNarrativeMutationConflict
			}
			result = models.MemoryMaintenanceResult{OK: true, Completed: lastCompleted == 1, Replayed: true}
			return nil
		}
		if token != claimToken || status != "delivered" || lease <= now.UnixMilli() {
			return models.ErrMemoryMaintenanceLease
		}
		var manifest maintenanceManifest
		if len(manifestJSON) > models.MemoryMaintenanceMaxBatchBytes || json.Unmarshal([]byte(manifestJSON), &manifest) != nil {
			return fmt.Errorf("invalid maintenance manifest")
		}
		if len(manifest.Sources) > models.MemoryMaintenanceMaxSessions || len(manifest.Targets) > models.MemoryMaintenanceMaxConcepts+2 {
			return models.ErrMemoryMaintenanceBudget
		}
		if len(updates) != len(manifest.Targets) {
			return fmt.Errorf("every memory and concept snapshot must have exactly one update")
		}
		values := make(map[string]string, len(updates))
		for _, update := range updates {
			if len(update.Content) > models.MemoryMaintenanceMaxObjectBytes {
				return models.ErrMemoryMaintenanceBudget
			}
			key := maintenanceObjectID(memory.NarrativeKey{Scope: memory.Scope(update.Scope), DomainID: update.DomainID, Key: update.Key})
			if _, exists := values[key]; exists {
				return fmt.Errorf("duplicate maintenance update")
			}
			values[key] = update.Content
		}
		valid, err := txs.maintenancePrefixValid(ctx, scope, manifest.StartKey, manifest.Next)
		if err != nil {
			return err
		}
		if !valid {
			return models.ErrMemoryMaintenanceConflict
		}
		for _, meta := range append(append([]maintenanceObject{}, manifest.Sources...), manifest.Targets...) {
			if err := txs.checkMaintenanceObject(ctx, meta); err != nil {
				return err
			}
		}
		if err := txs.checkMaintenanceObject(ctx, manifest.Archive); err != nil {
			return err
		}
		for _, meta := range manifest.Targets {
			if _, exists := values[maintenanceObjectID(meta.Key)]; !exists {
				return fmt.Errorf("missing required maintenance update")
			}
		}
		// A source added after claiming the final page becomes another batch;
		// completing early must not silently omit it.
		var remaining int
		err = txs.queryRow(ctx, `SELECT COUNT(*) FROM narrative_objects WHERE tenant_id=? AND learner_id=? AND scope='session' AND object_key>=? AND object_key<? AND (object_key>? OR (object_key=? AND enrollment_id>?))`, scope.TenantID, scope.LearnerID, manifest.StartKey, manifest.EndKey, manifest.Next.CursorKey, manifest.Next.CursorKey, manifest.Next.CursorEnrollment).Scan(&remaining)
		if err != nil {
			return err
		}
		completed := remaining == 0
		for _, meta := range manifest.Targets {
			_, _, err := txs.CompareAndSwapNarrative(ctx, meta.Key, meta.Version, values[maintenanceObjectID(meta.Key)], "", "", limits)
			if err != nil {
				return err
			}
		}
		if completed {
			if _, _, err := txs.CompareAndSwapNarrative(ctx, manifest.Archive.Key, manifest.Archive.Version, summary, "", "", limits); err != nil {
				return err
			}
		}
		manifest.Next.Summary = summary
		plain, err := json.Marshal(manifest.Next)
		if err != nil {
			return err
		}
		ciphertext, err := txs.secretKeyring.encryptWithAAD(string(plain), maintenanceAAD(scope, id))
		if err != nil {
			return err
		}
		newStatus := "pending"
		var completedAt any
		done := 0
		if completed {
			newStatus = "completed"
			completedAt = now.UTC()
			done = 1
			ciphertext = ""
		}
		_, err = txs.exec(ctx, `UPDATE pending_consolidations SET status=?,completed_at=?,maintenance_state=?,maintenance_manifest='',maintenance_token='',maintenance_lease_ms=0,maintenance_last_token=?,maintenance_last_digest=?,maintenance_last_completed=? WHERE id=? AND learner_id=? AND tenant_id=?`, newStatus, completedAt, ciphertext, claimToken, digest, done, id, scope.LearnerID, scope.TenantID)
		if err == nil {
			result = models.MemoryMaintenanceResult{OK: true, Completed: completed}
		}
		return err
	})
	return result, err
}
