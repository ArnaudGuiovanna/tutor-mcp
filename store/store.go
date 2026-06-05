// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package store is the persistence port. engine/tools/auth depend on these
// interfaces; db.Store (and a future PostgresStore) implement them.
// See docs/scaling/SCALING-SPEC-v2.md §2.
package store

import (
	"context"
	"time"

	"tutor-mcp/models"
)

// ---------------------------------------------------------------------------
// Sub-interfaces — segregated by domain concern
// ---------------------------------------------------------------------------

// LearnerStore manages learner accounts and profile data.
type LearnerStore interface {
	CreateLearner(ctx context.Context, email, passwordHash, objective, webhookURL string) (*models.Learner, error)
	GetLearnerByID(ctx context.Context, id string) (*models.Learner, error)
	GetLearnerByEmail(ctx context.Context, email string) (*models.Learner, error)
	UpdateLastActive(ctx context.Context, id string) error
	GetActiveLearners(ctx context.Context) ([]*models.Learner, error)
	UpdateLearnerProfile(ctx context.Context, learnerID, profileJSON string) error
}

// AuthStore manages OAuth2/refresh-token and auth-code state.
type AuthStore interface {
	CreateRefreshToken(ctx context.Context, learnerID, clientID string) (*models.RefreshToken, error)
	GetRefreshToken(ctx context.Context, token string) (*models.RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, token string) error
	CreateAuthCode(ctx context.Context, code, learnerID, codeChallenge, clientID string, expiresAt time.Time) error
	ConsumeAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error)
	CreateOAuthClient(ctx context.Context, clientID, clientName, redirectURIs string) error
	CreateOAuthClientWithSecret(ctx context.Context, clientID, clientName, redirectURIs, secretHash string) error
	CreateOAuthClientWithSecretCapped(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int) error
	CountOAuthClients(ctx context.Context) (int, error)
	GetOAuthClient(ctx context.Context, clientID string) (*models.OAuthClient, error)
	IsClientApproved(ctx context.Context, learnerID, clientID, redirectURI string) (bool, error)
	ApproveClient(ctx context.Context, learnerID, clientID, redirectURI string) error
	CleanupExpiredCodes(ctx context.Context) (int64, error)
	CleanupExpiredRefreshTokens(ctx context.Context) (int64, error)
}

// DomainStore manages learning domains, their graphs, phases, and metadata.
type DomainStore interface {
	CreateDomain(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace) (*models.Domain, error)
	CreateDomainWithValueFramings(ctx context.Context, learnerID, name, personalGoal string, graph models.KnowledgeSpace, valueFramingsJSON string) (*models.Domain, error)
	GetDomainByLearner(ctx context.Context, learnerID string) (*models.Domain, error)
	GetDomainByID(ctx context.Context, id string) (*models.Domain, error)
	GetDomainsByLearner(ctx context.Context, learnerID string, includeArchived bool) ([]*models.Domain, error)
	SetDomainPriority(ctx context.Context, domainID, learnerID string, rank int) error
	UpdateDomainValueFramings(ctx context.Context, domainID, valueFramingsJSON string) error
	UpdateDomainLastValueAxis(ctx context.Context, domainID, axis string) error
	UpdateDomainGraph(ctx context.Context, domainID string, graph models.KnowledgeSpace) error
	ArchiveDomain(ctx context.Context, domainID, learnerID string) error
	UnarchiveDomain(ctx context.Context, domainID, learnerID string) error
	ActiveDomainConceptSet(ctx context.Context, learnerID string) (map[string]bool, error)
	DeleteDomain(ctx context.Context, domainID, learnerID string) error
	UpdateDomainPhase(ctx context.Context, domainID string, phase models.Phase, phaseEntryEntropy float64, now time.Time) error
	MergeDomainGoalRelevance(ctx context.Context, domainID string, relevance map[string]float64) (*models.GoalRelevance, error)
	GetDomainGoalRelevance(ctx context.Context, domainID string) (*models.GoalRelevance, error)
}

// ConceptStateStore manages per-learner per-concept BKT state.
type ConceptStateStore interface {
	InsertConceptStateIfNotExists(ctx context.Context, cs *models.ConceptState) error
	GetConceptState(ctx context.Context, learnerID, concept string) (*models.ConceptState, error)
	// GetConceptStateForUpdate is identical to GetConceptState under SQLite and
	// acquires a row-level lock under PostgreSQL. Implemented next task.
	GetConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error)
	UpsertConceptState(ctx context.Context, cs *models.ConceptState) error
	GetConceptStatesByLearner(ctx context.Context, learnerID string) ([]*models.ConceptState, error)
	GetConceptsDueForReview(ctx context.Context, learnerID string) ([]string, error)
}

// InteractionStore records and queries learner interaction history.
type InteractionStore interface {
	CreateInteraction(ctx context.Context, i *models.Interaction) error
	GetRecentInteractions(ctx context.Context, learnerID, concept string, limit int) ([]*models.Interaction, error)
	GetRecentInteractionsByLearner(ctx context.Context, learnerID string, limit int) ([]*models.Interaction, error)
	GetSessionInteractions(ctx context.Context, learnerID string) ([]*models.Interaction, error)
	GetInteractionsSince(ctx context.Context, learnerID string, since time.Time) ([]*models.Interaction, error)
	GetSessionStart(ctx context.Context, learnerID string) (time.Time, error)
	GetActionHistoryForConcept(ctx context.Context, learnerID, concept string, recentLimit int) (models.ActionHistoryCounts, error)
	CountInteractionsSince(ctx context.Context, learnerID string, since time.Time, domainConcepts []string) (int, error)
	GetRecentConceptsByDomain(ctx context.Context, learnerID string, domainConcepts []string, limit int) ([]string, error)
}

// ActivityStatsStore provides aggregate activity and motivation statistics.
type ActivityStatsStore interface {
	GetDailyStreak(ctx context.Context, learnerID string) (int, error)
	GetTodayInteractionCount(ctx context.Context, learnerID string) (int, error)
	GetTodaySuccessRate(ctx context.Context, learnerID string) (float64, int, error)
	GetActivityStreak(ctx context.Context, learnerID string) (int, error)
	GetRecentLearnerEvents(ctx context.Context, learnerID string, since time.Time) ([]models.RawLearnerEvent, error)
	ConceptMasteryDelta(ctx context.Context, learnerID string, domainConcepts []string, since time.Time, limit int) ([]models.ConceptDelta, error)
	MilestonesInWindow(ctx context.Context, learnerID string, domainConcepts []string, since time.Time) ([]string, error)
	CountInteractionsByConcept(ctx context.Context, learnerID, concept string) (int, error)
	CountSessionsOnConcept(ctx context.Context, learnerID, concept string) (int, error)
	CountLearnerSessionStreak(ctx context.Context, learnerID string) (int, error)
	SelfInitiatedRatio(ctx context.Context, learnerID, concept string) (float64, error)
	LastFailureOnConcept(ctx context.Context, learnerID, concept string, window time.Duration) (*models.Interaction, error)
}

// MetacognitionStore manages affect, calibration, and transfer records.
type MetacognitionStore interface {
	UpsertAffectState(ctx context.Context, a *models.AffectState) error
	GetRecentAffectStates(ctx context.Context, learnerID string, limit int) ([]*models.AffectState, error)
	GetAffectBySession(ctx context.Context, learnerID, sessionID string) (*models.AffectState, error)
	CreateCalibrationPrediction(ctx context.Context, r *models.CalibrationRecord) error
	CompleteCalibrationRecord(ctx context.Context, predictionID, learnerID string, actual, delta float64) error
	GetCalibrationRecord(ctx context.Context, predictionID, learnerID string) (*models.CalibrationRecord, error)
	GetCalibrationBias(ctx context.Context, learnerID string, limit int) (float64, error)
	GetCalibrationBiasHistory(ctx context.Context, learnerID string, limit int) ([]float64, error)
	CreateTransferRecord(ctx context.Context, r *models.TransferRecord) error
	GetTransferScores(ctx context.Context, learnerID, conceptID string) ([]*models.TransferRecord, error)
	GetTransferRecordsByLearner(ctx context.Context, learnerID string) ([]*models.TransferRecord, error)
	GetHintStatsForMastered(ctx context.Context, learnerID string, threshold float64) (hints int, total int, err error)
	UpdateAffectAutonomyScore(ctx context.Context, learnerID, sessionID string, score float64) error
	CountProactiveReviews(ctx context.Context, learnerID string, since time.Time) (proactive int, total int, err error)
}

// MisconceptionStore queries and aggregates learner misconception data.
type MisconceptionStore interface {
	GetMisconceptionGroups(ctx context.Context, learnerID string, conceptFilter map[string]bool) ([]models.MisconceptionGroup, error)
	GetDistinctMisconceptionTypes(ctx context.Context, learnerID, concept string) ([]string, error)
	GetActiveMisconceptions(ctx context.Context, learnerID, concept string) ([]models.MisconceptionGroup, error)
	GetActiveMisconceptionsBatch(ctx context.Context, learnerID string, concepts []string) (map[string]bool, error)
	GetFirstActiveMisconception(ctx context.Context, learnerID, concept string) (*models.MisconceptionGroup, error)
}

// IntentionStore manages implementation intentions and learning-negotiation overrides.
type IntentionStore interface {
	InsertImplementationIntention(ctx context.Context, learnerID, domainID, trigger, action string, scheduledFor time.Time) (int64, error)
	HasRecentImplementationIntention(ctx context.Context, learnerID, domainID string, since time.Time) (bool, error)
	GetRecentImplementationIntentions(ctx context.Context, learnerID string, since time.Time, limit int) ([]*models.ImplementationIntention, error)
	MarkIntentionHonored(ctx context.Context, id int64, honored bool) error
	InsertLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID, payload string, expiresAt, now time.Time) (int64, error)
	ConsumeLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID string, now time.Time) (*models.LearningNegotiationOverridePayloadResult, error)
}

// AlertStore manages scheduled learner alerts.
type AlertStore interface {
	CreateScheduledAlert(ctx context.Context, learnerID, alertType, concept string, scheduledAt time.Time) error
	GetUnsentAlerts(ctx context.Context, learnerID string) ([]*models.ScheduledAlert, error)
	MarkAlertSent(ctx context.Context, id int64) error
	WasAlertSentToday(ctx context.Context, learnerID, alertType string) (bool, error)
}

// AvailabilityStore manages learner availability windows.
type AvailabilityStore interface {
	GetAvailability(ctx context.Context, learnerID string) (*models.Availability, error)
	UpsertAvailability(ctx context.Context, a *models.Availability) error
}

// WebhookQueueStore manages the outbound webhook message queue.
type WebhookQueueStore interface {
	EnqueueWebhookMessage(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority int) (int64, error)
	CreateWebhookPushLog(ctx context.Context, learnerID string, queueID int64, brief *models.WebhookBrief, pushedAt time.Time) (int64, error)
	GetLatestOpenWebhookPush(ctx context.Context, learnerID, domainID string, since time.Time) (*models.WebhookPushLog, error)
	MarkWebhookPushSessionOpened(ctx context.Context, learnerID string, openedAt, since time.Time) error
	MarkWebhookPushConceptAddressed(ctx context.Context, learnerID, domainID, concept string, addressedAt, since time.Time) error
	DequeueNextPending(ctx context.Context, learnerID, kind string, now time.Time, window time.Duration) (*models.WebhookQueueItem, error)
	MarkWebhookSent(ctx context.Context, id int64, learnerID string, now time.Time) error
	MarkWebhookFailed(ctx context.Context, id int64, learnerID string) error
	ExpirePastWebhookMessages(ctx context.Context, now time.Time) (int64, error)
	GetPendingWebhookMessages(ctx context.Context, learnerID string) ([]*models.WebhookQueueItem, error)
}

// ConsolidationStore manages periodic consolidation records.
type ConsolidationStore interface {
	UpsertPendingConsolidation(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error
	GetPendingConsolidations(ctx context.Context, learnerID string) ([]*models.PendingConsolidation, error)
	GetConsolidation(ctx context.Context, learnerID, periodType, periodKey string) (*models.PendingConsolidation, error)
	MarkConsolidationsDelivered(ctx context.Context, learnerID string, ids []int64, now time.Time) error
	MarkConsolidationCompleted(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error
	RequeueStaleDeliveredConsolidations(ctx context.Context, cutoff time.Time) (int64, error)
}

// SnapshotStore manages pedagogical snapshots.
type SnapshotStore interface {
	CreatePedagogicalSnapshot(ctx context.Context, snapshot *models.PedagogicalSnapshot) error
	GetPedagogicalSnapshots(ctx context.Context, learnerID, domainID, concept string, limit int) ([]*models.PedagogicalSnapshot, error)
}

// ---------------------------------------------------------------------------
// Aggregate interface — the full persistence port
// ---------------------------------------------------------------------------

// Store is the aggregate persistence interface. db.Store implements all
// sub-interfaces; a future PostgresStore can do the same.
type Store interface {
	LearnerStore
	AuthStore
	DomainStore
	ConceptStateStore
	InteractionStore
	ActivityStatsStore
	MetacognitionStore
	MisconceptionStore
	IntentionStore
	AlertStore
	AvailabilityStore
	WebhookQueueStore
	ConsolidationStore
	SnapshotStore

	// Lifecycle methods — implemented on *db.Store in the next task.
	WithTx(ctx context.Context, fn func(s Store) error) error
	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}
