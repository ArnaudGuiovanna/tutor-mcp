// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var errLearningEventExists = errors.New("learning event key already exists")

func (s *Store) RecordLearningEvent(ctx context.Context, actor models.Principal, request models.LearningEventRequest) (*models.LearningEvent, bool, error) {
	if actor.LearnerID == "" || !actor.Authorize(models.PermissionLearningSelf, models.AuthorizationResource{TenantID: actor.TenantID, OwnerUserID: actor.UserID}) ||
		!models.OAuthScopeAllows(strings.Join(actor.Scopes, " "), models.OAuthScopeLearnerWrite) {
		return nil, false, storeport.ErrInvalidPrincipal
	}
	for _, id := range []string{request.DomainID, request.ConceptID, request.EventKey} {
		if strings.TrimSpace(id) != id || id == "" || len(id) > 128 || strings.ContainsRune(id, 0) {
			return nil, false, fmt.Errorf("invalid learning event identifier")
		}
	}
	if len(request.AttemptID) > 255 || strings.HasPrefix(request.EventKey, "response:") || (request.Kind != "feedback" && request.Kind != "instruction") {
		return nil, false, fmt.Errorf("invalid learning event kind or key")
	}
	var event *models.LearningEvent
	replayed := false
	err := s.WithTenantTx(ctx, actor.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		txs := scoped.(*Store)
		if err := txs.ValidatePrincipal(txCtx, actor); err != nil {
			return err
		}
		if err := txs.lockLearningEventStream(txCtx, actor.LearnerID, request.DomainID); err != nil {
			return err
		}
		version, err := txs.lockCurriculumForEvidence(txCtx, actor.LearnerID, request.DomainID, request.ConceptID)
		if err != nil {
			return err
		}
		var archived int
		if err := txs.queryRow(txCtx, `SELECT archived FROM domains WHERE tenant_id = ? AND learner_id = ? AND id = ?`, actor.TenantID, actor.LearnerID, request.DomainID).Scan(&archived); err != nil {
			return err
		}
		if archived != 0 {
			return fmt.Errorf("domain is archived")
		}
		prior, err := txs.learningEventByKey(txCtx, actor.TenantID, actor.LearnerID, request.EventKey)
		if err == nil {
			if prior.DomainID != request.DomainID || prior.ConceptID != request.ConceptID || prior.AttemptID != request.AttemptID || prior.Kind != request.Kind {
				return fmt.Errorf("learning event key reused with different content")
			}
			event, replayed = prior, true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if request.AttemptID != "" {
			attempt, err := txs.GetAssessmentAttemptForUpdate(txCtx, actor.LearnerID, request.AttemptID)
			if err != nil {
				return err
			}
			if attempt.DomainID != request.DomainID || attempt.ConceptID != request.ConceptID || attempt.CurriculumInvalidatedVersion != 0 || attempt.Status == models.AssessmentAttemptCancelled {
				return ErrAssessmentStateConflict
			}
			if request.Kind == "feedback" && attempt.SubmittedAt == nil {
				return fmt.Errorf("feedback requires a committed response")
			}
		}
		scope, err := txs.resolveLearningScope(txCtx, actor.LearnerID, request.DomainID, request.ConceptID)
		if err != nil {
			return err
		}
		event = &models.LearningEvent{ID: "event_" + rand.Text(), EventKey: request.EventKey, DomainID: request.DomainID, ConceptID: request.ConceptID, AttemptID: request.AttemptID, Kind: request.Kind, Source: "host_reported", CurriculumVersion: version, OccurredAt: time.Now().UTC().Truncate(time.Microsecond)}
		if err := txs.insertLearningEvent(txCtx, scope.TenantID, actor.LearnerID, scope.EnrollmentID, event); err != nil {
			if !errors.Is(err, errLearningEventExists) {
				return err
			}
			prior, err := txs.learningEventByKey(txCtx, actor.TenantID, actor.LearnerID, request.EventKey)
			if err != nil {
				return err
			}
			if prior.DomainID != request.DomainID || prior.ConceptID != request.ConceptID || prior.AttemptID != request.AttemptID || prior.Kind != request.Kind {
				return fmt.Errorf("learning event key reused with different content")
			}
			event, replayed = prior, true
		}
		return nil
	})
	return event, replayed, err
}

// Serialize exposure and response commits before taking shared curriculum or
// attempt locks. PostgreSQL's statement snapshots otherwise allow a response
// to miss a concurrently committed exposure. SQLite serializes writers.
func (s *Store) lockLearningEventStream(ctx context.Context, learnerID, domainID string) error {
	if s.dialect != DialectPostgres {
		return nil
	}
	var id string
	return s.queryRow(ctx, `SELECT id FROM domains WHERE id = ? AND learner_id = ? AND deleted_at IS NULL FOR UPDATE`, domainID, learnerID).Scan(&id)
}

func (s *Store) insertLearningEvent(ctx context.Context, tenantID, learnerID, enrollmentID string, event *models.LearningEvent) error {
	result, err := s.exec(ctx, `INSERT INTO learning_events (id,tenant_id,learner_id,domain_id,enrollment_id,concept_id,attempt_id,event_key,kind,source,curriculum_version,occurred_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (tenant_id,learner_id,event_key) DO NOTHING`, event.ID, tenantID, learnerID, event.DomainID, enrollmentID, event.ConceptID, nullString(event.AttemptID), event.EventKey, event.Kind, event.Source, event.CurriculumVersion, event.OccurredAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errLearningEventExists
	}
	return err
}

func (s *Store) learningEventByKey(ctx context.Context, tenantID, learnerID, key string) (*models.LearningEvent, error) {
	var e models.LearningEvent
	var attempt sql.NullString
	err := s.queryRow(ctx, `SELECT id,event_key,domain_id,concept_id,attempt_id,kind,source,curriculum_version,occurred_at
 FROM learning_events WHERE tenant_id = ? AND learner_id = ? AND event_key = ?`, tenantID, learnerID, key).Scan(&e.ID, &e.EventKey, &e.DomainID, &e.ConceptID, &attempt, &e.Kind, &e.Source, &e.CurriculumVersion, &e.OccurredAt)
	e.AttemptID = attempt.String
	return &e, err
}
