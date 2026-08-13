// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	RetentionBackupMaxAge = 24 * time.Hour
	RetentionJobLease     = 30 * time.Minute
	retentionJobLockID    = int64(0x7475746f7252544e)

	RetentionPhaseDatabase  = "database"
	RetentionPhaseNarrative = "narrative"
)

var (
	ErrRetentionJobManifestConflict = errors.New("retention job manifest conflict")
	ErrRetentionJobInProgress       = errors.New("retention job is already leased")
	ErrRetentionJobLeaseLost        = errors.New("retention job lease lost")
	ErrRetentionPhaseOrder          = errors.New("retention job phase order violation")
	retentionIdentifierPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
)

type RetentionJobPhase struct {
	Phase        string     `json:"phase"`
	Position     int        `json:"position"`
	Status       string     `json:"status"`
	AttemptCount int        `json:"attempt_count"`
	Eligible     int64      `json:"eligible"`
	Applied      int64      `json:"applied"`
	Held         int64      `json:"held"`
	ReportJSON   string     `json:"report_json"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

type RetentionJob struct {
	JobID           string              `json:"job_id"`
	Policy          RetentionPolicy     `json:"policy"`
	PolicyHash      string              `json:"policy_hash"`
	AsOf            time.Time           `json:"as_of"`
	BackupReference string              `json:"backup_reference"`
	BackupCreatedAt time.Time           `json:"backup_created_at"`
	Status          string              `json:"status"`
	AttemptCount    int                 `json:"attempt_count"`
	LeaseOwner      string              `json:"lease_owner,omitempty"`
	LeasedUntil     *time.Time          `json:"leased_until,omitempty"`
	CreatedBy       string              `json:"created_by"`
	CreatedAt       time.Time           `json:"created_at"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	CompletedAt     *time.Time          `json:"completed_at,omitempty"`
	LastError       string              `json:"last_error,omitempty"`
	ReportJSON      string              `json:"report_json"`
	Phases          []RetentionJobPhase `json:"phases"`
}

type RetentionLegalHold struct {
	HoldID        string     `json:"hold_id"`
	LearnerID     string     `json:"learner_id"`
	Reason        string     `json:"reason"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
	ReleasedBy    string     `json:"released_by,omitempty"`
	ReleaseReason string     `json:"release_reason,omitempty"`
}

func retentionPolicyManifest(policy RetentionPolicy) (string, string, error) {
	if err := policy.Validate(); err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(encoded)
	return string(encoded), hex.EncodeToString(hash[:]), nil
}

func validateRetentionActor(actor string) error {
	if strings.TrimSpace(actor) == "" || len(actor) > 120 {
		return fmt.Errorf("retention actor must use 1..120 bytes")
	}
	return nil
}

func (s *Store) lockRetentionCoordinator(ctx context.Context) error {
	if s.dialect != DialectPostgres {
		return nil
	}
	if _, err := s.exec(ctx, `SELECT pg_advisory_xact_lock(?)`, retentionJobLockID); err != nil {
		return fmt.Errorf("lock retention coordinator: %w", err)
	}
	return nil
}

func (s *Store) CreateOrResumeRetentionJob(
	ctx context.Context,
	jobID string,
	policy RetentionPolicy,
	asOf time.Time,
	backupReference string,
	backupCreatedAt time.Time,
	actor string,
	now time.Time,
) (*RetentionJob, error) {
	if !retentionIdentifierPattern.MatchString(jobID) {
		return nil, fmt.Errorf("retention job ID must use 1..128 safe characters")
	}
	if err := validateRetentionActor(actor); err != nil {
		return nil, err
	}
	backupReference = strings.TrimSpace(backupReference)
	if backupReference == "" || len(backupReference) > 500 {
		return nil, fmt.Errorf("retention backup reference must use 1..500 bytes")
	}
	if asOf.IsZero() || backupCreatedAt.IsZero() || now.IsZero() {
		return nil, fmt.Errorf("retention manifest timestamps are required")
	}
	asOf, backupCreatedAt, now = asOf.UTC(), backupCreatedAt.UTC(), now.UTC()
	if backupCreatedAt.After(asOf) {
		return nil, fmt.Errorf("retention backup cannot be newer than the manifest cutoff")
	}
	if asOf.Sub(backupCreatedAt) > RetentionBackupMaxAge {
		return nil, fmt.Errorf("retention backup is older than %s", RetentionBackupMaxAge)
	}
	policyJSON, policyHash, err := retentionPolicyManifest(policy)
	if err != nil {
		return nil, fmt.Errorf("retention manifest policy: %w", err)
	}
	if !policy.Enabled() {
		return nil, fmt.Errorf("retention manifest has no enabled category")
	}

	err = s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockRetentionCoordinator(ctx); err != nil {
			return err
		}
		var storedPolicyHash, storedBackupReference string
		var storedAsOf, storedBackupAt flexTime
		err := txs.queryRow(ctx,
			`SELECT policy_hash, as_of, backup_reference, backup_created_at
			 FROM retention_jobs WHERE job_id = ?`, jobID,
		).Scan(&storedPolicyHash, &storedAsOf, &storedBackupReference, &storedBackupAt)
		switch {
		case err == nil:
			if storedPolicyHash != policyHash || !storedAsOf.Time.Equal(asOf) ||
				storedBackupReference != backupReference || !storedBackupAt.Time.Equal(backupCreatedAt) {
				return ErrRetentionJobManifestConflict
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("load retention manifest: %w", err)
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO retention_jobs
			    (job_id, policy_json, policy_hash, as_of, backup_reference,
			     backup_created_at, status, attempt_count, lease_owner,
			     leased_until, created_by, created_at, last_error, report_json)
			 VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, '', NULL, ?, ?, '', '{}')`,
			jobID, policyJSON, policyHash, asOf, backupReference, backupCreatedAt, actor, now,
		); err != nil {
			return fmt.Errorf("create retention manifest: %w", err)
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO retention_job_phases
			    (job_id, phase, position, status) VALUES (?, 'database', 1, 'pending')`, jobID,
		); err != nil {
			return fmt.Errorf("create database retention phase: %w", err)
		}
		narrativeStatus := "skipped"
		var narrativeCompleted any = now
		if policy.NarrativeMemoryDays > 0 {
			narrativeStatus = "pending"
			narrativeCompleted = nil
		}
		if _, err := txs.exec(ctx,
			`INSERT INTO retention_job_phases
			    (job_id, phase, position, status, completed_at)
			 VALUES (?, 'narrative', 2, ?, ?)`,
			jobID, narrativeStatus, narrativeCompleted,
		); err != nil {
			return fmt.Errorf("create narrative retention phase: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRetentionJob(ctx, jobID)
}

func (s *Store) ClaimRetentionJob(ctx context.Context, jobID, owner string, now time.Time) (*RetentionJob, error) {
	if !retentionIdentifierPattern.MatchString(owner) {
		return nil, fmt.Errorf("retention lease owner must use 1..128 safe characters")
	}
	now = now.UTC()
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.lockRetentionCoordinator(ctx); err != nil {
			return err
		}
		var status string
		var leaseOwner string
		var leasedUntil flexTime
		if err := txs.queryRow(ctx,
			`SELECT status, lease_owner, leased_until FROM retention_jobs WHERE job_id = ?`, jobID,
		).Scan(&status, &leaseOwner, &leasedUntil); err != nil {
			return fmt.Errorf("load retention job lease: %w", err)
		}
		if status == "completed" {
			return nil
		}
		if status == "running" && leasedUntil.Valid && leasedUntil.Time.After(now) && leaseOwner != owner {
			return ErrRetentionJobInProgress
		}
		result, err := txs.exec(ctx,
			`UPDATE retention_jobs
			 SET status = 'running', attempt_count = attempt_count + 1,
			     lease_owner = ?, leased_until = ?,
			     started_at = COALESCE(started_at, ?), last_error = ''
			 WHERE job_id = ? AND status <> 'completed'`,
			owner, now.Add(RetentionJobLease), now, jobID,
		)
		if err != nil {
			return fmt.Errorf("claim retention job: %w", err)
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrRetentionJobLeaseLost
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRetentionJob(ctx, jobID)
}

func (s *Store) StartRetentionJobPhase(ctx context.Context, jobID, owner, phase string, now time.Time) (bool, error) {
	now = now.UTC()
	started := false
	err := s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.validateRetentionLease(ctx, jobID, owner, now); err != nil {
			return err
		}
		var position int
		var status string
		if err := txs.queryRow(ctx,
			`SELECT position, status FROM retention_job_phases WHERE job_id = ? AND phase = ?`, jobID, phase,
		).Scan(&position, &status); err != nil {
			return fmt.Errorf("load retention phase: %w", err)
		}
		if status == "completed" || status == "skipped" {
			return nil
		}
		var incomplete int
		if err := txs.queryRow(ctx,
			`SELECT COUNT(*) FROM retention_job_phases
			 WHERE job_id = ? AND position < ? AND status NOT IN ('completed','skipped')`,
			jobID, position,
		).Scan(&incomplete); err != nil {
			return err
		}
		if incomplete != 0 {
			return ErrRetentionPhaseOrder
		}
		if _, err := txs.exec(ctx,
			`UPDATE retention_job_phases
			 SET status = 'running', attempt_count = attempt_count + 1,
			     started_at = COALESCE(started_at, ?), completed_at = NULL, last_error = ''
			 WHERE job_id = ? AND phase = ?`, now, jobID, phase,
		); err != nil {
			return fmt.Errorf("start retention phase: %w", err)
		}
		started = true
		return nil
	})
	return started, err
}

func (s *Store) CompleteRetentionJobPhase(
	ctx context.Context, jobID, owner, phase string,
	eligible, applied, held int64, reportJSON string, now time.Time,
) error {
	if !json.Valid([]byte(reportJSON)) {
		return fmt.Errorf("retention phase report must be JSON")
	}
	now = now.UTC()
	return s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.validateRetentionLease(ctx, jobID, owner, now); err != nil {
			return err
		}
		result, err := txs.exec(ctx,
			`UPDATE retention_job_phases
			 SET status = 'completed', eligible = ?, applied = ?, held = ?,
			     report_json = ?, completed_at = ?, last_error = ''
			 WHERE job_id = ? AND phase = ? AND status = 'running'`,
			eligible, applied, held, reportJSON, now, jobID, phase,
		)
		if err != nil {
			return fmt.Errorf("complete retention phase: %w", err)
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrRetentionPhaseOrder
		}
		return nil
	})
}

// ApplyRetentionDatabaseJobPhase commits the relational mutations and their
// durable proof in one transaction. A crash therefore exposes either neither
// side or both; there is no "rows deleted, checkpoint still running" window.
func (s *Store) ApplyRetentionDatabaseJobPhase(
	ctx context.Context,
	jobID, owner string,
	policy RetentionPolicy,
	now time.Time,
) (*RetentionReport, error) {
	_, policyHash, err := retentionPolicyManifest(policy)
	if err != nil {
		return nil, err
	}
	var report *RetentionReport
	err = s.inTx(ctx, txOptionsForDialect(s.dialect), func(txs *Store) error {
		if err := txs.validateRetentionLease(ctx, jobID, owner, now.UTC()); err != nil {
			return err
		}
		var storedHash string
		var asOf flexTime
		if err := txs.queryRow(ctx,
			`SELECT policy_hash, as_of FROM retention_jobs WHERE job_id = ?`, jobID,
		).Scan(&storedHash, &asOf); err != nil {
			return err
		}
		if storedHash != policyHash {
			return ErrRetentionJobManifestConflict
		}
		var phaseStatus string
		if err := txs.queryRow(ctx,
			`SELECT status FROM retention_job_phases WHERE job_id = ? AND phase = 'database'`, jobID,
		).Scan(&phaseStatus); err != nil {
			return err
		}
		if phaseStatus != "running" {
			return ErrRetentionPhaseOrder
		}
		report = &RetentionReport{DryRun: false, AsOf: asOf.Time.UTC(), Policy: policy}
		if err := txs.runDataRetention(ctx, policy, report.AsOf, true, report); err != nil {
			return err
		}
		reportJSON, err := json.Marshal(report)
		if err != nil {
			return err
		}
		eligible, applied, held := RetentionReportTotals(report)
		result, err := txs.exec(ctx,
			`UPDATE retention_job_phases
			 SET status = 'completed', eligible = ?, applied = ?, held = ?,
			     report_json = ?, completed_at = ?, last_error = ''
			 WHERE job_id = ? AND phase = 'database' AND status = 'running'`,
			eligible, applied, held, string(reportJSON), now.UTC(), jobID,
		)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrRetentionPhaseOrder
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("apply retention database phase: %w", err)
	}
	return report, nil
}

func (s *Store) FailRetentionJobPhase(ctx context.Context, jobID, owner, phase string, cause error, now time.Time) error {
	message := "retention phase failed"
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 500 {
		message = message[:500]
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.validateRetentionLease(ctx, jobID, owner, now.UTC()); err != nil {
			return err
		}
		if _, err := txs.exec(ctx,
			`UPDATE retention_job_phases SET status = 'failed', last_error = ?
			 WHERE job_id = ? AND phase = ?`, message, jobID, phase,
		); err != nil {
			return err
		}
		_, err := txs.exec(ctx,
			`UPDATE retention_jobs
			 SET status = 'failed', lease_owner = '', leased_until = NULL, last_error = ?
			 WHERE job_id = ?`, message, jobID,
		)
		return err
	})
}

func (s *Store) CompleteRetentionJob(ctx context.Context, jobID, owner, reportJSON string, now time.Time) error {
	if !json.Valid([]byte(reportJSON)) {
		return fmt.Errorf("retention job report must be JSON")
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		if err := txs.validateRetentionLease(ctx, jobID, owner, now.UTC()); err != nil {
			return err
		}
		var incomplete int
		if err := txs.queryRow(ctx,
			`SELECT COUNT(*) FROM retention_job_phases
			 WHERE job_id = ? AND status NOT IN ('completed','skipped')`, jobID,
		).Scan(&incomplete); err != nil {
			return err
		}
		if incomplete != 0 {
			return ErrRetentionPhaseOrder
		}
		result, err := txs.exec(ctx,
			`UPDATE retention_jobs
			 SET status = 'completed', completed_at = ?, lease_owner = '',
			     leased_until = NULL, last_error = '', report_json = ?
			 WHERE job_id = ? AND status = 'running'`, now.UTC(), reportJSON, jobID,
		)
		if err != nil {
			return err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return ErrRetentionJobLeaseLost
		}
		return nil
	})
}

func (s *Store) validateRetentionLease(ctx context.Context, jobID, owner string, now time.Time) error {
	var one int
	err := s.queryRow(ctx,
		`SELECT 1 FROM retention_jobs
		 WHERE job_id = ? AND status = 'running' AND lease_owner = ?
		   AND leased_until IS NOT NULL AND leased_until > ?`,
		jobID, owner, now,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRetentionJobLeaseLost
	}
	return err
}

func (s *Store) GetRetentionJob(ctx context.Context, jobID string) (*RetentionJob, error) {
	job := &RetentionJob{JobID: jobID}
	var policyJSON string
	var asOf, backupAt, createdAt, leasedUntil, startedAt, completedAt flexTime
	err := s.queryRow(ctx,
		`SELECT policy_json, policy_hash, as_of, backup_reference,
		        backup_created_at, status, attempt_count, lease_owner,
		        leased_until, created_by, created_at, started_at,
		        completed_at, last_error, report_json
		 FROM retention_jobs WHERE job_id = ?`, jobID,
	).Scan(
		&policyJSON, &job.PolicyHash, &asOf, &job.BackupReference,
		&backupAt, &job.Status, &job.AttemptCount, &job.LeaseOwner,
		&leasedUntil, &job.CreatedBy, &createdAt, &startedAt,
		&completedAt, &job.LastError, &job.ReportJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("get retention job: %w", err)
	}
	if err := json.Unmarshal([]byte(policyJSON), &job.Policy); err != nil {
		return nil, fmt.Errorf("decode retention job policy: %w", err)
	}
	job.AsOf, job.BackupCreatedAt, job.CreatedAt = asOf.Time.UTC(), backupAt.Time.UTC(), createdAt.Time.UTC()
	assignOptionalTime(&job.LeasedUntil, leasedUntil)
	assignOptionalTime(&job.StartedAt, startedAt)
	assignOptionalTime(&job.CompletedAt, completedAt)
	rows, err := s.query(ctx,
		`SELECT phase, position, status, attempt_count, eligible, applied,
		        held, report_json, started_at, completed_at, last_error
		 FROM retention_job_phases WHERE job_id = ? ORDER BY position`, jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var phase RetentionJobPhase
		var phaseStarted, phaseCompleted flexTime
		if err := rows.Scan(
			&phase.Phase, &phase.Position, &phase.Status, &phase.AttemptCount,
			&phase.Eligible, &phase.Applied, &phase.Held, &phase.ReportJSON,
			&phaseStarted, &phaseCompleted, &phase.LastError,
		); err != nil {
			return nil, err
		}
		assignOptionalTime(&phase.StartedAt, phaseStarted)
		assignOptionalTime(&phase.CompletedAt, phaseCompleted)
		job.Phases = append(job.Phases, phase)
	}
	return job, rows.Err()
}

func assignOptionalTime(target **time.Time, value flexTime) {
	if value.Valid {
		result := value.Time.UTC()
		*target = &result
	}
}

func (s *Store) CreateRetentionLegalHold(ctx context.Context, hold RetentionLegalHold) error {
	if !retentionIdentifierPattern.MatchString(hold.HoldID) || strings.TrimSpace(hold.LearnerID) == "" {
		return fmt.Errorf("retention hold ID and learner are required")
	}
	if strings.TrimSpace(hold.Reason) == "" || len(hold.Reason) > 500 {
		return fmt.Errorf("retention hold reason must use 1..500 bytes")
	}
	if err := validateRetentionActor(hold.CreatedBy); err != nil {
		return err
	}
	if hold.CreatedAt.IsZero() {
		return fmt.Errorf("retention hold creation time is required")
	}
	return s.inTx(ctx, nil, func(txs *Store) error {
		var learnerID, reason, createdBy string
		err := txs.queryRow(ctx,
			`SELECT learner_id, reason, created_by FROM retention_legal_holds WHERE hold_id = ?`, hold.HoldID,
		).Scan(&learnerID, &reason, &createdBy)
		switch {
		case err == nil:
			if learnerID != hold.LearnerID || reason != hold.Reason || createdBy != hold.CreatedBy {
				return fmt.Errorf("retention hold ID already has different metadata")
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = txs.exec(ctx,
			`INSERT INTO retention_legal_holds
			    (hold_id, learner_id, reason, created_by, created_at,
			     released_at, released_by, release_reason)
			 VALUES (?, ?, ?, ?, ?, NULL, '', '')`,
			hold.HoldID, hold.LearnerID, hold.Reason, hold.CreatedBy, hold.CreatedAt.UTC(),
		)
		return err
	})
}

func (s *Store) ReleaseRetentionLegalHold(ctx context.Context, holdID, actor, reason string, now time.Time) (bool, error) {
	if !retentionIdentifierPattern.MatchString(holdID) {
		return false, fmt.Errorf("retention hold ID is invalid")
	}
	if err := validateRetentionActor(actor); err != nil {
		return false, err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return false, fmt.Errorf("retention hold release reason must use 1..500 bytes")
	}
	result, err := s.exec(ctx,
		`UPDATE retention_legal_holds
		 SET released_at = ?, released_by = ?, release_reason = ?
		 WHERE hold_id = ? AND released_at IS NULL`, now.UTC(), actor, reason, holdID,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) ListRetentionLegalHolds(ctx context.Context, activeOnly bool, limit int) ([]RetentionLegalHold, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("retention hold limit must be between 1 and 1000")
	}
	where := ""
	if activeOnly {
		where = " WHERE released_at IS NULL"
	}
	rows, err := s.query(ctx,
		`SELECT hold_id, learner_id, reason, created_by, created_at,
		        released_at, released_by, release_reason
		 FROM retention_legal_holds`+where+` ORDER BY created_at DESC, hold_id LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var holds []RetentionLegalHold
	for rows.Next() {
		var hold RetentionLegalHold
		var createdAt, releasedAt flexTime
		if err := rows.Scan(
			&hold.HoldID, &hold.LearnerID, &hold.Reason, &hold.CreatedBy,
			&createdAt, &releasedAt, &hold.ReleasedBy, &hold.ReleaseReason,
		); err != nil {
			return nil, err
		}
		hold.CreatedAt = createdAt.Time.UTC()
		assignOptionalTime(&hold.ReleasedAt, releasedAt)
		holds = append(holds, hold)
	}
	return holds, rows.Err()
}

func (s *Store) ActiveRetentionLegalHoldLearners(ctx context.Context) (map[string]bool, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT learner_id FROM retention_legal_holds WHERE released_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var learnerID string
		if err := rows.Scan(&learnerID); err != nil {
			return nil, err
		}
		result[learnerID] = true
	}
	return result, rows.Err()
}
