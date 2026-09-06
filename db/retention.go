// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"fmt"
	"time"
)

// RetentionPolicy is intentionally disabled by default: a zero value preserves
// every row. Operators must opt in to each independently-auditable category.
// Core learning evidence (interactions, affect, calibration, transfer and
// concept state) is deliberately outside this policy.
type RetentionPolicy struct {
	WebhookTerminalDays        int `json:"webhook_terminal_days"`
	WebhookLiveDays            int `json:"webhook_live_days"`
	AssessmentPlaintextDays    int `json:"assessment_plaintext_days"`
	AssessmentAbandonedDays    int `json:"assessment_abandoned_days"`
	IdempotencyResponseDays    int `json:"idempotency_response_days"`
	PedagogicalSnapshotDays    int `json:"pedagogical_snapshot_days"`
	OperationalEventLogDays    int `json:"operational_event_log_days"`
	CompletedConsolidationDays int `json:"completed_consolidation_days"`
	NarrativeMemoryDays        int `json:"narrative_memory_days"`
}

const maxRetentionDays = 365000 // 1000 years; rejects accidental integer garbage.

// Validate rejects negative or implausibly large durations. Zero always means
// "disabled / preserve", never "delete immediately".
func (p RetentionPolicy) Validate() error {
	values := []struct {
		name string
		days int
	}{
		{"webhook_terminal_days", p.WebhookTerminalDays},
		{"webhook_live_days", p.WebhookLiveDays},
		{"assessment_plaintext_days", p.AssessmentPlaintextDays},
		{"assessment_abandoned_days", p.AssessmentAbandonedDays},
		{"idempotency_response_days", p.IdempotencyResponseDays},
		{"pedagogical_snapshot_days", p.PedagogicalSnapshotDays},
		{"operational_event_log_days", p.OperationalEventLogDays},
		{"completed_consolidation_days", p.CompletedConsolidationDays},
		{"narrative_memory_days", p.NarrativeMemoryDays},
	}
	for _, value := range values {
		if value.days < 0 {
			return fmt.Errorf("%s must be >= 0 (zero preserves data)", value.name)
		}
		if value.days > maxRetentionDays {
			return fmt.Errorf("%s exceeds the maximum of %d days", value.name, maxRetentionDays)
		}
	}
	return nil
}

// Enabled reports whether at least one data category has an explicit TTL.
func (p RetentionPolicy) Enabled() bool {
	return p.WebhookTerminalDays > 0 ||
		p.WebhookLiveDays > 0 ||
		p.AssessmentPlaintextDays > 0 ||
		p.AssessmentAbandonedDays > 0 ||
		p.IdempotencyResponseDays > 0 ||
		p.PedagogicalSnapshotDays > 0 ||
		p.OperationalEventLogDays > 0 ||
		p.CompletedConsolidationDays > 0 ||
		p.NarrativeMemoryDays > 0
}

// RetentionMetric separates what matched the policy from what was actually
// mutated. In dry-run mode Applied is always zero.
type RetentionMetric struct {
	Eligible int64 `json:"eligible"`
	Applied  int64 `json:"applied"`
	Held     int64 `json:"held"`
}

// RetentionReport is stable JSON output for the maintenance command and
// monitoring integrations. AssessmentPlaintextBlocked counts old rows with
// plaintext preserved because at least one corresponding content hash is
// absent; destroying their only remaining representation would be unsafe.
type RetentionReport struct {
	DryRun                       bool            `json:"dry_run"`
	AsOf                         time.Time       `json:"as_of"`
	Policy                       RetentionPolicy `json:"policy"`
	WebhookTerminalRows          RetentionMetric `json:"webhook_terminal_rows"`
	WebhookLiveRowsTerminalized  RetentionMetric `json:"webhook_live_rows_terminalized"`
	WebhookPushLinksDetached     RetentionMetric `json:"webhook_push_links_detached"`
	AssessmentTaskPlaintext      RetentionMetric `json:"assessment_task_plaintext"`
	AssessmentResponsePlaintext  RetentionMetric `json:"assessment_response_plaintext"`
	AssessmentPlaintextBlocked   int64           `json:"assessment_plaintext_blocked"`
	AssessmentAbandonedAttempts  RetentionMetric `json:"assessment_abandoned_attempts"`
	IdempotencyResponsePlaintext RetentionMetric `json:"idempotency_response_plaintext"`
	PedagogicalSnapshots         RetentionMetric `json:"pedagogical_snapshots"`
	PedagogicalDecisions         RetentionMetric `json:"pedagogical_decisions"`
	WebhookPushEvents            RetentionMetric `json:"webhook_push_events"`
	ScheduledAlertEvents         RetentionMetric `json:"scheduled_alert_events"`
	CompletedConsolidations      RetentionMetric `json:"completed_consolidations"`
	NarrativeMemoryFiles         RetentionMetric `json:"narrative_memory_files"`
}

// RunDataRetention previews or applies the configured lifecycle policy. Apply
// mode is one transaction: a failure in any category rolls every mutation
// back. Dry-run performs only SELECT COUNT queries and never opens a write
// transaction. Counts are best-effort under concurrent writes, while apply
// reports the exact RowsAffected values returned by the transaction.
func (s *Store) RunDataRetention(ctx context.Context, policy RetentionPolicy, now time.Time, apply bool) (*RetentionReport, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("data retention policy: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	report := &RetentionReport{
		DryRun: !apply,
		AsOf:   now,
		Policy: policy,
	}
	run := func(txs *Store) error {
		return txs.runDataRetention(ctx, policy, now, apply, report)
	}
	if !apply {
		if err := run(s); err != nil {
			return nil, err
		}
		return report, nil
	}
	if err := s.inTx(ctx, txOptionsForDialect(s.dialect), run); err != nil {
		return nil, fmt.Errorf("apply data retention: %w", err)
	}
	return report, nil
}

func (s *Store) runDataRetention(ctx context.Context, policy RetentionPolicy, now time.Time, apply bool, report *RetentionReport) error {
	if policy.AssessmentAbandonedDays > 0 {
		cutoff := retentionCutoff(now, policy.AssessmentAbandonedDays)
		eligible := `status IN ('prepared', 'submitted') AND created_at < ?`
		var err error
		report.AssessmentAbandonedAttempts.Eligible, report.AssessmentAbandonedAttempts.Held, err = s.retentionCountsByHold(
			ctx, "assessment_attempts", eligible, cutoff)
		if err != nil {
			return fmt.Errorf("count abandoned assessment attempts: %w", err)
		}
		if apply {
			report.AssessmentAbandonedAttempts.Applied, err = s.retentionExec(ctx,
				`UPDATE assessment_attempts
				 SET status = 'cancelled', cancelled_at = ?,
				     task_text = CASE WHEN task_content_hash <> '' THEN NULL ELSE task_text END,
				     response_text = CASE WHEN response_content_hash <> '' THEN NULL ELSE response_text END
				 WHERE `+eligible+` AND `+retentionHoldClause("assessment_attempts", false),
				now, cutoff)
			if err != nil {
				return fmt.Errorf("cancel abandoned assessment attempts: %w", err)
			}
		}
	}

	if policy.WebhookLiveDays > 0 {
		cutoff := retentionCutoff(now, policy.WebhookLiveDays)
		eligible := `status IN ('pending', 'processing') AND created_at < ?`
		var err error
		report.WebhookLiveRowsTerminalized.Eligible, report.WebhookLiveRowsTerminalized.Held, err = s.retentionCountsByHold(
			ctx, "webhook_message_queue", eligible, cutoff)
		if err != nil {
			return fmt.Errorf("count stale live webhook rows: %w", err)
		}
		if apply {
			report.WebhookLiveRowsTerminalized.Applied, err = s.retentionExec(ctx,
				`UPDATE webhook_message_queue
				 SET status = 'expired', claimed_at = NULL, next_attempt_at = NULL,
				     last_error = 'retention_expired', dead_lettered_at = NULL
				 WHERE `+eligible+` AND `+retentionHoldClause("webhook_message_queue", false),
				cutoff)
			if err != nil {
				return fmt.Errorf("terminalize stale live webhook rows: %w", err)
			}
		}
	}

	if policy.IdempotencyResponseDays > 0 {
		cutoff := retentionCutoff(now, policy.IdempotencyResponseDays)
		eligible := `status = 'completed'
			AND completed_at < ?
			AND response_text <> ''
			AND response_expired_at IS NULL`
		var err error
		report.IdempotencyResponsePlaintext.Eligible, report.IdempotencyResponsePlaintext.Held, err = s.retentionCountsByHold(
			ctx, "tool_call_idempotency", eligible, cutoff)
		if err != nil {
			return fmt.Errorf("count idempotency response plaintext: %w", err)
		}
		if apply {
			report.IdempotencyResponsePlaintext.Applied, err = s.retentionExec(ctx,
				`UPDATE tool_call_idempotency
				 SET response_text = '', response_expired_at = ?
				 WHERE `+eligible+` AND `+retentionHoldClause("tool_call_idempotency", false),
				now, cutoff)
			if err != nil {
				return fmt.Errorf("redact idempotency response plaintext: %w", err)
			}
		}
	}

	if policy.AssessmentPlaintextDays > 0 {
		cutoff := retentionCutoff(now, policy.AssessmentPlaintextDays)
		terminal := `status IN ('evaluated', 'cancelled')
			AND COALESCE(evaluated_at, cancelled_at, created_at) < ?`
		taskEligible := terminal + ` AND task_text IS NOT NULL AND task_text <> '' AND task_content_hash <> ''`
		responseEligible := terminal + ` AND response_text IS NOT NULL AND response_text <> '' AND response_content_hash <> ''`
		blocked := terminal + ` AND (
			(task_text IS NOT NULL AND task_text <> '' AND task_content_hash = '') OR
			(response_text IS NOT NULL AND response_text <> '' AND response_content_hash = '')
		)`

		var err error
		report.AssessmentTaskPlaintext.Eligible, report.AssessmentTaskPlaintext.Held, err = s.retentionCountsByHold(
			ctx, "assessment_attempts", taskEligible, cutoff)
		if err != nil {
			return fmt.Errorf("count assessment task plaintext: %w", err)
		}
		report.AssessmentResponsePlaintext.Eligible, report.AssessmentResponsePlaintext.Held, err = s.retentionCountsByHold(
			ctx, "assessment_attempts", responseEligible, cutoff)
		if err != nil {
			return fmt.Errorf("count assessment response plaintext: %w", err)
		}
		report.AssessmentPlaintextBlocked, err = s.retentionCount(ctx,
			`SELECT COUNT(*) FROM assessment_attempts WHERE `+blocked, cutoff)
		if err != nil {
			return fmt.Errorf("count unsafe assessment plaintext: %w", err)
		}
		if apply {
			report.AssessmentTaskPlaintext.Applied, err = s.retentionExec(ctx,
				`UPDATE assessment_attempts SET task_text = NULL WHERE `+taskEligible+` AND `+retentionHoldClause("assessment_attempts", false), cutoff)
			if err != nil {
				return fmt.Errorf("redact assessment task plaintext: %w", err)
			}
			report.AssessmentResponsePlaintext.Applied, err = s.retentionExec(ctx,
				`UPDATE assessment_attempts SET response_text = NULL WHERE `+responseEligible+` AND `+retentionHoldClause("assessment_attempts", false), cutoff)
			if err != nil {
				return fmt.Errorf("redact assessment response plaintext: %w", err)
			}
		}
	}

	if policy.WebhookTerminalDays > 0 {
		cutoff := retentionCutoff(now, policy.WebhookTerminalDays)
		terminal := `status IN ('sent', 'failed', 'expired')
			AND COALESCE(sent_at, dead_lettered_at, claimed_at, created_at) < ?`
		var err error
		report.WebhookTerminalRows.Eligible, report.WebhookTerminalRows.Held, err = s.retentionCountsByHold(
			ctx, "webhook_message_queue", terminal, cutoff)
		if err != nil {
			return fmt.Errorf("count terminal webhook rows: %w", err)
		}
		pushLinks := `queue_id <> 0 AND queue_id IN (
				SELECT id FROM webhook_message_queue WHERE ` + terminal + `
			 )`
		report.WebhookPushLinksDetached.Eligible, report.WebhookPushLinksDetached.Held, err = s.retentionCountsByHold(
			ctx, "webhook_push_log", pushLinks, cutoff)
		if err != nil {
			return fmt.Errorf("count webhook push links: %w", err)
		}
		if apply {
			report.WebhookPushLinksDetached.Applied, err = s.retentionExec(ctx,
				`UPDATE webhook_push_log SET queue_id = 0
				 WHERE `+pushLinks+` AND `+retentionHoldClause("webhook_push_log", false), cutoff)
			if err != nil {
				return fmt.Errorf("detach webhook push links: %w", err)
			}
			report.WebhookTerminalRows.Applied, err = s.retentionExec(ctx,
				`DELETE FROM webhook_message_queue WHERE `+terminal+` AND `+retentionHoldClause("webhook_message_queue", false), cutoff)
			if err != nil {
				return fmt.Errorf("delete terminal webhook rows: %w", err)
			}
		}
	}

	if policy.PedagogicalSnapshotDays > 0 {
		cutoff := retentionCutoff(now, policy.PedagogicalSnapshotDays)
		var err error
		report.PedagogicalSnapshots.Eligible, report.PedagogicalSnapshots.Held, err = s.retentionCountsByHold(
			ctx, "pedagogical_snapshots", `created_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("count pedagogical snapshots: %w", err)
		}
		if apply {
			report.PedagogicalSnapshots.Applied, err = s.retentionExec(ctx,
				`DELETE FROM pedagogical_snapshots WHERE created_at < ? AND `+retentionHoldClause("pedagogical_snapshots", false), cutoff)
			if err != nil {
				return fmt.Errorf("delete pedagogical snapshots: %w", err)
			}
		}
		// Decisions hold only a minimal immutable runtime contract. Once an
		// assessment references it, that binding is retained with the evidence
		// until explicit learner erasure. Unused decisions follow snapshot TTL;
		// neither retention nor redaction may detach a live assessment's contract.
		unusedDecision := `created_at < ? AND NOT EXISTS
			(SELECT 1 FROM assessment_attempts a WHERE a.decision_id = pedagogical_decisions.id)`
		report.PedagogicalDecisions.Eligible, report.PedagogicalDecisions.Held, err = s.retentionCountsByHold(
			ctx, "pedagogical_decisions", unusedDecision, cutoff)
		if err != nil {
			return fmt.Errorf("count unused pedagogical decisions: %w", err)
		}
		if apply {
			report.PedagogicalDecisions.Applied, err = s.retentionExec(ctx,
				`DELETE FROM pedagogical_decisions WHERE `+unusedDecision+` AND `+retentionHoldClause("pedagogical_decisions", false), cutoff)
			if err != nil {
				return fmt.Errorf("delete unused pedagogical decisions: %w", err)
			}
		}
	}

	if policy.OperationalEventLogDays > 0 {
		cutoff := retentionCutoff(now, policy.OperationalEventLogDays)
		var err error
		report.WebhookPushEvents.Eligible, report.WebhookPushEvents.Held, err = s.retentionCountsByHold(
			ctx, "webhook_push_log", `created_at < ? AND pushed_at < ?`, cutoff, cutoff)
		if err != nil {
			return fmt.Errorf("count webhook push events: %w", err)
		}
		report.ScheduledAlertEvents.Eligible, report.ScheduledAlertEvents.Held, err = s.retentionCountsByHold(
			ctx, "scheduled_alerts", `created_at < ? AND scheduled_at < ?`, cutoff, cutoff)
		if err != nil {
			return fmt.Errorf("count scheduled alert events: %w", err)
		}
		if apply {
			report.WebhookPushEvents.Applied, err = s.retentionExec(ctx,
				`DELETE FROM webhook_push_log
				 WHERE created_at < ? AND pushed_at < ? AND `+retentionHoldClause("webhook_push_log", false), cutoff, cutoff)
			if err != nil {
				return fmt.Errorf("delete webhook push events: %w", err)
			}
			report.ScheduledAlertEvents.Applied, err = s.retentionExec(ctx,
				`DELETE FROM scheduled_alerts
				 WHERE created_at < ? AND scheduled_at < ? AND `+retentionHoldClause("scheduled_alerts", false), cutoff, cutoff)
			if err != nil {
				return fmt.Errorf("delete scheduled alert events: %w", err)
			}
		}
	}

	if policy.CompletedConsolidationDays > 0 {
		cutoff := retentionCutoff(now, policy.CompletedConsolidationDays)
		eligible := `status = 'completed' AND completed_at IS NOT NULL AND completed_at < ?`
		var err error
		report.CompletedConsolidations.Eligible, report.CompletedConsolidations.Held, err = s.retentionCountsByHold(
			ctx, "pending_consolidations", eligible, cutoff)
		if err != nil {
			return fmt.Errorf("count completed consolidations: %w", err)
		}
		if apply {
			report.CompletedConsolidations.Applied, err = s.retentionExec(ctx,
				`DELETE FROM pending_consolidations WHERE `+eligible+` AND `+retentionHoldClause("pending_consolidations", false), cutoff)
			if err != nil {
				return fmt.Errorf("delete completed consolidations: %w", err)
			}
		}
	}

	return nil
}

func retentionCutoff(now time.Time, days int) time.Time {
	return now.AddDate(0, 0, -days).UTC()
}

func retentionHoldClause(table string, held bool) string {
	prefix := "NOT "
	if held {
		prefix = ""
	}
	return prefix + `EXISTS (
		SELECT 1 FROM retention_legal_holds AS retention_hold
		WHERE retention_hold.learner_id = ` + table + `.learner_id
		  AND retention_hold.released_at IS NULL
	)`
}

func (s *Store) retentionCountsByHold(ctx context.Context, table, eligible string, args ...any) (int64, int64, error) {
	selectable, err := s.retentionCount(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE (`+eligible+`) AND `+retentionHoldClause(table, false),
		args...,
	)
	if err != nil {
		return 0, 0, err
	}
	held, err := s.retentionCount(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE (`+eligible+`) AND `+retentionHoldClause(table, true),
		args...,
	)
	return selectable, held, err
}

// RetentionReportTotals collapses the per-category proof into phase checkpoint
// counters while retaining the detailed report JSON alongside it.
func RetentionReportTotals(report *RetentionReport) (eligible, applied, held int64) {
	if report == nil {
		return 0, 0, 0
	}
	metrics := []RetentionMetric{
		report.WebhookTerminalRows,
		report.WebhookLiveRowsTerminalized,
		report.WebhookPushLinksDetached,
		report.AssessmentTaskPlaintext,
		report.AssessmentResponsePlaintext,
		report.AssessmentAbandonedAttempts,
		report.IdempotencyResponsePlaintext,
		report.PedagogicalSnapshots,
		report.PedagogicalDecisions,
		report.WebhookPushEvents,
		report.ScheduledAlertEvents,
		report.CompletedConsolidations,
		report.NarrativeMemoryFiles,
	}
	for _, metric := range metrics {
		eligible += metric.Eligible
		applied += metric.Applied
		held += metric.Held
	}
	return eligible, applied, held
}

func (s *Store) retentionCount(ctx context.Context, query string, args ...any) (int64, error) {
	var count int64
	if err := s.queryRow(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) retentionExec(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := s.exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
