// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package store is the persistence port. engine/tools/auth depend on these
// interfaces; db.Store implements them for both SQLite and PostgreSQL.
// See docs/scaling/SCALING-SPEC-v2.md §2.
package store

import (
	"context"
	"errors"
	"time"

	"tutor-mcp/models"
)

// ErrOAuthClientLimitReached is returned by CreateOAuthClientWithSecretCapped
// when the per-deployment client cap is hit.
var ErrOAuthClientLimitReached = errors.New("oauth client limit reached")

// ErrInvalidAccountToken deliberately collapses unknown, expired, consumed,
// and wrong-purpose email capabilities into one public failure.
var ErrInvalidAccountToken = errors.New("invalid account token")

// ErrInvalidAuthCode deliberately collapses unknown, expired, client-bound and
// already-consumed authorization codes into OAuth's invalid_grant response.
var ErrInvalidAuthCode = errors.New("invalid authorization code")

// ErrInvalidRefreshToken deliberately covers unknown, expired, client-mismatched,
// and already-rotated refresh tokens. OAuth callers must not reveal which case
// occurred, but can distinguish the expected invalid_grant path from a storage
// failure.
var ErrInvalidRefreshToken = errors.New("invalid refresh token")

// ErrInvalidOAuthScope covers unsupported authorization scopes and refresh
// requests that attempt to widen the credential's persisted grant.
var ErrInvalidOAuthScope = errors.New("invalid oauth scope")

// ErrNotFound is the persistence-port sentinel for an expected missing row.
// Store implementations should preserve their native cause as well so callers
// can distinguish absence from an unavailable backend without depending on a
// particular SQL driver.
var ErrNotFound = errors.New("store item not found")

type notFoundError struct {
	cause error
}

func (e *notFoundError) Error() string {
	if e.cause == nil {
		return ErrNotFound.Error()
	}
	return e.cause.Error()
}

// Unwrap exposes both the stable port sentinel and the implementation cause.
// In particular, the SQL store continues to satisfy existing errors.Is checks
// for sql.ErrNoRows while application code can use ErrNotFound.
func (e *notFoundError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrNotFound}
	}
	return []error{ErrNotFound, e.cause}
}

// WrapNotFound marks a backend-specific absence with the stable Store
// contract. It is intentionally limited to confirmed absence; timeouts,
// connection failures, and cancelled queries must pass through unchanged.
func WrapNotFound(cause error) error {
	if errors.Is(cause, ErrNotFound) {
		return cause
	}
	return &notFoundError{cause: cause}
}

// ErrRefreshTokenReuse marks a replay of an already-used or revoked refresh
// token. OAuth responses deliberately collapse it to invalid_grant, while the
// store keeps it distinct so the caller can audit suspected credential theft.
// The whole token family is revoked before this error is returned.
var ErrRefreshTokenReuse = errors.New("refresh token reuse detected")

// ErrCurriculumVersionConflict is returned when a caller tries to publish a
// curriculum revision from a version that is no longer current. Callers must
// reload the latest immutable snapshot and intentionally rebase their change;
// silently overwriting the winner would lose curriculum history.
var ErrCurriculumVersionConflict = errors.New("curriculum version conflict")

// ErrDomainPhaseConflict prevents two orchestration decisions computed from
// the same phase snapshot from blindly overwriting one another. Callers must
// reload the domain and recompute the next activity from current evidence.
var ErrDomainPhaseConflict = errors.New("domain phase conflict")

// ErrIdempotencyKeyConflict prevents a caller from reusing the same scoped key
// for different arguments. Treating it as a replay would return a response for
// an operation the caller did not request.
var ErrIdempotencyKeyConflict = errors.New("idempotency key reused with different arguments")

// ErrIdempotencyInProgress represents an already-owned mutation whose final
// outcome is not yet cached. The reservation is deliberately not stolen: an
// ambiguous crash must not duplicate learner-state writes.
var ErrIdempotencyInProgress = errors.New("idempotent operation in progress")

// ErrIdempotencyResponseExpired represents a mutation that completed
// successfully but whose cached response was later removed by an explicit
// retention policy. Callers must never interpret it as permission to execute
// the mutation again.
var ErrIdempotencyResponseExpired = errors.New("mutation already completed; cached response expired")

// ErrAvailabilityVersionConflict prevents two clients from silently
// overwriting learner-controlled notification or accessibility preferences.
var ErrAvailabilityVersionConflict = errors.New("availability version conflict")

// RateLimitBackend is the optional shared, fleet-wide store the auth
// RateLimiter delegates token accounting to (opt-in via RATELIMIT_BACKEND).
// Declared here in the neutral persistence-port package so package auth
// (consumer) and package db (Postgres implementation) can both reference it
// without an import cycle. nil backend = the in-memory per-process default.
type RateLimitBackend interface {
	// Allow atomically refills and consumes one token for key under a
	// (rate tokens/sec, burst) token bucket, returning whether allowed.
	Allow(ctx context.Context, key string, rate float64, burst int, now time.Time) (bool, error)
}

// LoginFailureBackend is the optional shared, fleet-wide store the auth
// LoginFailureTracker delegates per-account lockout to (opt-in via
// RATELIMIT_BACKEND). Declared in this neutral port package so auth and db can
// both reference it without an import cycle. nil = in-memory default.
type LoginFailureBackend interface {
	Record(ctx context.Context, key string, now time.Time) (int, error) // returns failures in window
	CountInWindow(ctx context.Context, key string, window time.Duration, now time.Time) (int, error)
	Reset(ctx context.Context, key string) error
}

// ---------------------------------------------------------------------------
// Sub-interfaces — segregated by domain concern
// ---------------------------------------------------------------------------

// LearnerStore manages learner accounts and profile data.
type LearnerStore interface {
	CreateLearner(ctx context.Context, email, passwordHash, objective, webhookURL string) (*models.Learner, error)
	CreateUnverifiedLearner(ctx context.Context, email, passwordHash, objective, webhookURL string) (*models.Learner, error)
	GetLearnerByID(ctx context.Context, id string) (*models.Learner, error)
	GetLearnerByEmail(ctx context.Context, email string) (*models.Learner, error)
	UpdateLastActive(ctx context.Context, id string) error
	GetActiveLearners(ctx context.Context) ([]*models.Learner, error)
	// UpdateLearnerProfile atomically persists declared profile preferences and,
	// when objective is non-nil, the canonical learner objective.
	UpdateLearnerProfile(ctx context.Context, learnerID, profileJSON string, objective *string) error
}

// AuthStore manages OAuth2/refresh-token and auth-code state.
type AuthStore interface {
	CreateRefreshToken(ctx context.Context, learnerID, clientID, resource string) (*models.RefreshToken, error)
	CreateRefreshTokenWithScope(ctx context.Context, learnerID, clientID, resource, scope string) (*models.RefreshToken, error)
	GetRefreshToken(ctx context.Context, token string) (*models.RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, token string) error
	RotateRefreshToken(ctx context.Context, token, clientID, resource string) (*models.RefreshToken, error)
	RotateRefreshTokenWithScope(ctx context.Context, token, clientID, resource, requestedScope string) (*models.RefreshToken, error)
	CreateAuthCodeWithBinding(ctx context.Context, code, learnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource string, expiresAt time.Time) error
	CreateAuthCodeWithBindingAndScope(ctx context.Context, code, learnerID, codeChallenge, codeChallengeMethod, clientID, redirectURI, resource, scope string, expiresAt time.Time) error
	GetAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error)
	ConsumeAuthCode(ctx context.Context, code, clientID string) (*models.AuthCode, error)
	ExchangeAuthCodeForRefreshToken(ctx context.Context, code, clientID string) (*models.AuthCode, *models.RefreshToken, error)
	CreateOAuthClient(ctx context.Context, clientID, clientName, redirectURIs string) error
	CreateOAuthClientWithSecret(ctx context.Context, clientID, clientName, redirectURIs, secretHash string) error
	CreateOAuthClientWithSecretCapped(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int) error
	CreateOAuthClientWithSecretCappedTTL(ctx context.Context, clientID, clientName, redirectURIs, secretHash string, maxClients int, expiresAt time.Time) error
	CleanupExpiredOAuthClients(ctx context.Context) (int64, error)
	CreateAccountToken(ctx context.Context, token *models.AccountToken) error
	GetAccountToken(ctx context.Context, tokenHash, purpose string) (*models.AccountToken, error)
	ActivateLearnerAndCreateAuthCode(ctx context.Context, tokenHash, passwordHash, code string, codeExpiresAt time.Time, persistClientApproval bool) (*models.AccountToken, error)
	ResetPasswordWithToken(ctx context.Context, tokenHash, passwordHash string) (string, error)
	CleanupExpiredAccountTokens(ctx context.Context) (int64, error)
	CountOAuthClients(ctx context.Context) (int, error)
	GetOAuthClient(ctx context.Context, clientID string) (*models.OAuthClient, error)
	IsClientApproved(ctx context.Context, learnerID, clientID, redirectURI string) (bool, error)
	IsClientApprovedForScope(ctx context.Context, learnerID, clientID, redirectURI, scope string) (bool, error)
	ApproveClient(ctx context.Context, learnerID, clientID, redirectURI string) error
	ApproveClientForScope(ctx context.Context, learnerID, clientID, redirectURI, scope string) error
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
	MarkDomainHighStakes(ctx context.Context, domainID, learnerID string) error
	UpdateDomainValueFramings(ctx context.Context, domainID, valueFramingsJSON string) error
	UpdateDomainLastValueAxis(ctx context.Context, domainID, axis string) error
	UpdateDomainGraph(ctx context.Context, domainID string, graph models.KnowledgeSpace) error
	EnsureCurriculumBaseline(ctx context.Context, learnerID, domainID string) (*models.CurriculumSnapshot, error)
	GetCurriculumSnapshot(ctx context.Context, learnerID, domainID string, version int) (*models.CurriculumSnapshot, error)
	ListCurriculumSnapshots(ctx context.Context, learnerID, domainID string, limit int) ([]*models.CurriculumSnapshot, error)
	CompareAndSwapCurriculum(ctx context.Context, learnerID, domainID string, expectedVersion int, snapshot *models.CurriculumSnapshot) error
	ArchiveDomain(ctx context.Context, domainID, learnerID string) error
	UnarchiveDomain(ctx context.Context, domainID, learnerID string) error
	ActiveDomainConceptSet(ctx context.Context, learnerID string) (map[string]bool, error)
	DeleteDomain(ctx context.Context, domainID, learnerID string) error
	UpdateDomainPhase(ctx context.Context, domainID string, phase models.Phase, phaseEntryEntropy float64, now time.Time) error
	CompareAndSwapDomainPhase(ctx context.Context, domainID string, expectedPhase, phase models.Phase, phaseEntryEntropy float64, now time.Time) error
	MergeDomainGoalRelevance(ctx context.Context, domainID string, relevance map[string]float64) (*models.GoalRelevance, error)
	GetDomainGoalRelevance(ctx context.Context, domainID string) (*models.GoalRelevance, error)
}

// ConceptStateStore manages cognitive state. Production learning paths use the
// domain-scoped methods: concept labels are not globally unique for a learner.
// The unscoped methods remain for legacy data and aggregate/operator views.
type ConceptStateStore interface {
	InsertConceptStateIfNotExists(ctx context.Context, cs *models.ConceptState) error
	GetConceptState(ctx context.Context, learnerID, concept string) (*models.ConceptState, error)
	GetConceptStateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error)
	// GetConceptStateForUpdate is identical to GetConceptState under SQLite and
	// acquires a row-level lock under PostgreSQL.
	GetConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error)
	GetConceptStateForUpdateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error)
	// GetOrCreateConceptStateForUpdate returns the row-locked concept state,
	// materializing a default row first when none exists. The materialize-then-
	// lock order is required for first-touch concurrency safety on Postgres
	// (a bare SELECT ... FOR UPDATE locks nothing when the row is absent).
	// Must run inside WithTx.
	GetOrCreateConceptStateForUpdate(ctx context.Context, learnerID, concept string) (*models.ConceptState, error)
	GetOrCreateConceptStateForUpdateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error)
	UpsertConceptState(ctx context.Context, cs *models.ConceptState) error
	GetConceptStatesByLearner(ctx context.Context, learnerID string) ([]*models.ConceptState, error)
	GetConceptStatesByDomain(ctx context.Context, learnerID, domainID string) ([]*models.ConceptState, error)
	GetConceptsDueForReview(ctx context.Context, learnerID string) ([]string, error)
	GetConceptsDueForReviewByDomain(ctx context.Context, learnerID, domainID string) ([]string, error)
}

// InteractionStore records and queries learner interaction history.
type InteractionStore interface {
	CreateInteraction(ctx context.Context, i *models.Interaction) error
	GetRecentInteractions(ctx context.Context, learnerID, concept string, limit int) ([]*models.Interaction, error)
	GetRecentInteractionsInDomain(ctx context.Context, learnerID, domainID, concept string, limit int) ([]*models.Interaction, error)
	GetRecentInteractionsByLearner(ctx context.Context, learnerID string, limit int) ([]*models.Interaction, error)
	GetRecentInteractionsByDomain(ctx context.Context, learnerID, domainID string, limit int) ([]*models.Interaction, error)
	GetSessionInteractions(ctx context.Context, learnerID string) ([]*models.Interaction, error)
	GetSessionInteractionsInDomain(ctx context.Context, learnerID, domainID string) ([]*models.Interaction, error)
	GetInteractionsBySession(ctx context.Context, learnerID, sessionID string) ([]*models.Interaction, error)
	GetInteractionsBySessionInDomain(ctx context.Context, learnerID, sessionID, domainID string) ([]*models.Interaction, error)
	GetInteractionsSince(ctx context.Context, learnerID string, since time.Time) ([]*models.Interaction, error)
	GetInteractionsSinceInDomain(ctx context.Context, learnerID, domainID string, since time.Time) ([]*models.Interaction, error)
	GetSessionStart(ctx context.Context, learnerID string) (time.Time, error)
	GetActionHistoryForConcept(ctx context.Context, learnerID, concept string, recentLimit int) (models.ActionHistoryCounts, error)
	GetActionHistoryForConceptInDomain(ctx context.Context, learnerID, domainID, concept string, recentLimit int) (models.ActionHistoryCounts, error)
	CountInteractionsSince(ctx context.Context, learnerID string, since time.Time, domainConcepts []string) (int, error)
	// GetQualifiedDiagnosticConceptsSinceInDomain returns the distinct concept
	// IDs observed through hint-free, submitted and evaluated diagnostics.
	GetQualifiedDiagnosticConceptsSinceInDomain(ctx context.Context, learnerID, domainID string, since time.Time) ([]string, error)
	GetRecentConceptsByDomain(ctx context.Context, learnerID string, domainConcepts []string, limit int) ([]string, error)
	GetRecentConceptsInDomain(ctx context.Context, learnerID, domainID string, limit int) ([]string, error)
}

// AssessmentStore persists the pre-declared task/rubric and its subsequent
// response/evaluation as an explicit state machine. TrustedEvaluation is
// assigned by a server-side evaluator boundary, not by MCP callers.
type AssessmentStore interface {
	CreateAssessmentAttempt(ctx context.Context, attempt *models.AssessmentAttempt) error
	GetAssessmentAttempt(ctx context.Context, learnerID, attemptID string) (*models.AssessmentAttempt, error)
	GetAssessmentAttemptForUpdate(ctx context.Context, learnerID, attemptID string) (*models.AssessmentAttempt, error)
	SubmitAssessmentAttempt(ctx context.Context, learnerID, attemptID, responseText, responseHash string, now time.Time) error
	CompleteAssessmentEvaluation(ctx context.Context, learnerID, attemptID, rubricScoreJSON, evaluatorID string, method models.EvaluationMethod, provenanceJSON string, score float64, passed bool, now time.Time) error
	CancelAssessmentAttempt(ctx context.Context, learnerID, attemptID string, now time.Time) error
	// GetEvaluatedAssessmentAttemptsInDomain returns both passing and failing
	// attempts. TrustedEvaluation is effective trust: the persistence boundary
	// clears it for unsupported methods and, in high-stakes domains, for every
	// method except human_review.
	GetEvaluatedAssessmentAttemptsInDomain(ctx context.Context, learnerID, domainID, conceptID string, limit int) ([]*models.AssessmentAttempt, error)
	// GetEvaluatedAssessmentAttemptsBatchInDomain applies limitPerConcept to
	// every requested concept while loading the domain evidence in one query.
	GetEvaluatedAssessmentAttemptsBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string, limitPerConcept int) (map[string][]*models.AssessmentAttempt, error)
	GetTrustedPassedAssessmentAttemptsInDomain(ctx context.Context, learnerID, domainID, conceptID string, limit int) ([]*models.AssessmentAttempt, error)
	HasHumanReviewedEvaluationInDomain(ctx context.Context, learnerID, domainID string) (bool, error)
}

// LearningSessionStore manages explicit, durable learning-session boundaries.
// At most one session may be open for a learner. OpenLearningSession is
// idempotent: concurrent callers receive the same active session.
type LearningSessionStore interface {
	OpenLearningSession(ctx context.Context, learnerID, domainID, requestedID string, now time.Time) (*models.LearningSession, error)
	GetLearningSession(ctx context.Context, learnerID, sessionID string) (*models.LearningSession, error)
	GetActiveLearningSession(ctx context.Context, learnerID string) (*models.LearningSession, error)
	TouchLearningSession(ctx context.Context, learnerID, sessionID string, now time.Time) (*models.LearningSession, error)
	CloseLearningSession(ctx context.Context, learnerID, sessionID string, now time.Time) (*models.LearningSession, error)
}

// IdempotencyStore protects retryable MCP mutations from duplicate side
// effects. Keys are scoped by learner and tool; requestHash binds a key to one
// canonical argument payload. Completed responses are replayed verbatim until
// an opt-in retention policy expires only the cached payload; the reservation
// remains completed and can never be acquired for execution again.
type IdempotencyStore interface {
	ClaimIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash string, now time.Time) (cachedResponse string, execute bool, err error)
	CompleteIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash, responseText string, now time.Time) error
	AbortIdempotencyKey(ctx context.Context, learnerID, toolName, key, requestHash string) error
}

// ActivityStatsStore provides aggregate activity and motivation statistics.
type ActivityStatsStore interface {
	GetDailyStreak(ctx context.Context, learnerID string) (int, error)
	GetTodayInteractionCount(ctx context.Context, learnerID string) (int, error)
	GetTodaySuccessRate(ctx context.Context, learnerID string) (float64, int, error)
	GetActivityStreak(ctx context.Context, learnerID string) (int, error)
	GetRecentLearnerEvents(ctx context.Context, learnerID string, since time.Time) ([]models.RawLearnerEvent, error)
	ConceptMasteryDelta(ctx context.Context, learnerID string, domainConcepts []string, since time.Time, limit int) ([]models.ConceptDelta, error)
	ConceptMasteryDeltaInDomain(ctx context.Context, learnerID, domainID string, domainConcepts []string, since time.Time, limit int) ([]models.ConceptDelta, error)
	MilestonesInWindow(ctx context.Context, learnerID string, domainConcepts []string, since time.Time) ([]string, error)
	MilestonesInWindowInDomain(ctx context.Context, learnerID, domainID string, domainConcepts []string, since time.Time) ([]string, error)
	CountInteractionsByConcept(ctx context.Context, learnerID, concept string) (int, error)
	CountInteractionsByConceptInDomain(ctx context.Context, learnerID, domainID, concept string) (int, error)
	CountSessionsOnConcept(ctx context.Context, learnerID, concept string) (int, error)
	CountSessionsOnConceptInDomain(ctx context.Context, learnerID, domainID, concept string) (int, error)
	CountLearnerSessionStreak(ctx context.Context, learnerID string) (int, error)
	SelfInitiatedRatio(ctx context.Context, learnerID, concept string) (float64, error)
	SelfInitiatedRatioInDomain(ctx context.Context, learnerID, domainID, concept string) (float64, error)
	LastFailureOnConcept(ctx context.Context, learnerID, concept string, window time.Duration) (*models.Interaction, error)
	LastFailureOnConceptInDomain(ctx context.Context, learnerID, domainID, concept string, window time.Duration) (*models.Interaction, error)
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
	GetCalibrationBiasInDomain(ctx context.Context, learnerID, domainID string, limit int) (float64, error)
	GetCalibrationBiasHistory(ctx context.Context, learnerID string, limit int) ([]float64, error)
	GetCalibrationBiasHistoryInDomain(ctx context.Context, learnerID, domainID string, limit int) ([]float64, error)
	CreateTransferRecord(ctx context.Context, r *models.TransferRecord) error
	GetTransferScores(ctx context.Context, learnerID, conceptID string) ([]*models.TransferRecord, error)
	GetTransferScoresInDomain(ctx context.Context, learnerID, domainID, conceptID string) ([]*models.TransferRecord, error)
	GetTransferScoresBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string) (map[string][]*models.TransferRecord, error)
	GetTransferRecordsByLearner(ctx context.Context, learnerID string) ([]*models.TransferRecord, error)
	GetTransferRecordsByDomain(ctx context.Context, learnerID, domainID string) ([]*models.TransferRecord, error)
	GetHintStatsForMastered(ctx context.Context, learnerID string, threshold float64) (hints int, total int, err error)
	UpdateAffectAutonomyScore(ctx context.Context, learnerID, sessionID string, score float64) error
	CountProactiveReviews(ctx context.Context, learnerID string, since time.Time) (proactive int, total int, err error)
}

// MisconceptionStore queries and aggregates learner misconception data.
type MisconceptionStore interface {
	GetMisconceptionGroups(ctx context.Context, learnerID string, conceptFilter map[string]bool) ([]models.MisconceptionGroup, error)
	GetMisconceptionGroupsInDomain(ctx context.Context, learnerID, domainID string, conceptFilter map[string]bool) ([]models.MisconceptionGroup, error)
	GetDistinctMisconceptionTypes(ctx context.Context, learnerID, concept string) ([]string, error)
	GetDistinctMisconceptionTypesInDomain(ctx context.Context, learnerID, domainID, concept string) ([]string, error)
	GetActiveMisconceptions(ctx context.Context, learnerID, concept string) ([]models.MisconceptionGroup, error)
	GetActiveMisconceptionsInDomain(ctx context.Context, learnerID, domainID, concept string) ([]models.MisconceptionGroup, error)
	GetActiveMisconceptionsBatch(ctx context.Context, learnerID string, concepts []string) (map[string]bool, error)
	GetActiveMisconceptionsBatchInDomain(ctx context.Context, learnerID, domainID string, concepts []string) (map[string]bool, error)
	GetFirstActiveMisconception(ctx context.Context, learnerID, concept string) (*models.MisconceptionGroup, error)
	GetFirstActiveMisconceptionInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.MisconceptionGroup, error)
}

// IntentionStore manages implementation intentions and learning-negotiation overrides.
type IntentionStore interface {
	InsertImplementationIntentionForSession(ctx context.Context, learnerID, domainID, sessionID, trigger, action string, scheduledFor time.Time) (int64, error)
	HasRecentImplementationIntention(ctx context.Context, learnerID, domainID string, since time.Time) (bool, error)
	GetRecentImplementationIntentions(ctx context.Context, learnerID string, since time.Time, limit int) ([]*models.ImplementationIntention, error)
	GetImplementationIntention(ctx context.Context, learnerID string, id int64) (*models.ImplementationIntention, error)
	UpdateImplementationIntentionStatus(ctx context.Context, learnerID string, id int64, status string, now time.Time) (*models.ImplementationIntention, error)
	InsertLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID, payload string, expiresAt, now time.Time) (int64, error)
	ConsumeLearningNegotiationOverridePayload(ctx context.Context, learnerID, domainID string, now time.Time) (*models.LearningNegotiationOverridePayloadResult, error)
}

// AlertStore manages scheduled learner alerts.
type AlertStore interface {
	CreateScheduledAlert(ctx context.Context, learnerID, alertType, concept string, scheduledAt time.Time) error
	WasAlertSentToday(ctx context.Context, learnerID, alertType string) (bool, error)
}

// AvailabilityStore manages learner availability windows.
type AvailabilityStore interface {
	GetAvailability(ctx context.Context, learnerID string) (*models.Availability, error)
	UpsertAvailability(ctx context.Context, a *models.Availability) error
	UpdateAvailability(ctx context.Context, a *models.Availability, expectedVersion int) (*models.Availability, error)
	ReserveNotificationDelivery(ctx context.Context, learnerID, alertType, domainID string, intrusive bool, at time.Time) (int64, bool, error)
	CompleteNotificationDelivery(ctx context.Context, reservationID int64, learnerID string) error
	ReleaseNotificationDelivery(ctx context.Context, reservationID int64, learnerID string) error
}

// WebhookQueueStore manages the outbound webhook message queue.
type WebhookQueueStore interface {
	EnqueueWebhookMessage(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority int) (int64, error)
	EnqueueWebhookMessageWithMaxAttempts(ctx context.Context, learnerID, kind, content string, scheduledFor, expiresAt time.Time, priority, maxAttempts int) (int64, error)
	EnqueueWebhookMessageOncePerDay(ctx context.Context, learnerID, kind, alertType, content string, scheduledFor, expiresAt time.Time, priority int) (int64, bool, error)
	CreateWebhookPushLog(ctx context.Context, learnerID string, queueID int64, brief *models.WebhookBrief, pushedAt time.Time) (int64, error)
	GetLatestOpenWebhookPush(ctx context.Context, learnerID, domainID string, since time.Time) (*models.WebhookPushLog, error)
	MarkWebhookPushSessionOpened(ctx context.Context, learnerID string, openedAt, since time.Time) error
	MarkWebhookPushConceptAddressed(ctx context.Context, learnerID, domainID, concept string, addressedAt, since time.Time) error
	ClaimNextPendingWebhook(ctx context.Context, learnerID, kind string, now time.Time, window time.Duration) (*models.WebhookQueueItem, error)
	IsWebhookClaimActive(ctx context.Context, id int64, learnerID string) (bool, error)
	MarkWebhookSent(ctx context.Context, id int64, learnerID string, now time.Time) error
	MarkWebhookFailed(ctx context.Context, id int64, learnerID string) error
	RecordWebhookFailure(ctx context.Context, id int64, learnerID, reason string, now time.Time) (deadLettered bool, err error)
	ReleaseWebhookClaim(ctx context.Context, id int64, learnerID string) error
	RequeueStaleWebhookClaims(ctx context.Context, cutoff, now time.Time) (int64, error)
	ExpirePastWebhookMessages(ctx context.Context, now time.Time) (int64, error)
	GetPendingWebhookMessages(ctx context.Context, learnerID string) ([]*models.WebhookQueueItem, error)
	GetDeadLetterWebhookMessages(ctx context.Context, learnerID string, limit int) ([]*models.WebhookQueueItem, error)
}

// ConsolidationStore manages periodic consolidation records.
type ConsolidationStore interface {
	UpsertPendingConsolidation(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error
	GetLearnerIDsForConsolidation(ctx context.Context) ([]string, error)
	GetPendingConsolidations(ctx context.Context, learnerID string) ([]*models.PendingConsolidation, error)
	ClaimPendingConsolidations(ctx context.Context, learnerID string, now time.Time) ([]*models.PendingConsolidation, error)
	ReleaseConsolidationClaims(ctx context.Context, learnerID string, ids []int64) error
	GetConsolidation(ctx context.Context, learnerID, periodType, periodKey string) (*models.PendingConsolidation, error)
	MarkConsolidationCompleted(ctx context.Context, learnerID, periodType, periodKey string, now time.Time) error
	RequeueStaleDeliveredConsolidations(ctx context.Context, cutoff time.Time) (int64, error)
}

// SnapshotStore manages pedagogical snapshots.
type SnapshotStore interface {
	CreatePedagogicalSnapshot(ctx context.Context, snapshot *models.PedagogicalSnapshot) error
	GetPedagogicalSnapshots(ctx context.Context, learnerID, domainID, concept string, limit int) ([]*models.PedagogicalSnapshot, error)
}

// SchedulerStore manages distributed scheduler job-run leases (Phase 4).
type SchedulerStore interface {
	ClaimJobRun(ctx context.Context, name, windowKey string) (bool, error)
	PurgeJobRunsBefore(ctx context.Context, cutoff time.Time) (int64, error)
	CleanupRateLimitState(ctx context.Context, bucketCutoff, failureCutoff time.Time) (int64, int64, error)
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
	AssessmentStore
	LearningSessionStore
	IdempotencyStore
	ActivityStatsStore
	MetacognitionStore
	MisconceptionStore
	IntentionStore
	AlertStore
	AvailabilityStore
	WebhookQueueStore
	ConsolidationStore
	SnapshotStore
	SchedulerStore

	// Lifecycle methods — implemented on *db.Store in the next task.
	WithTx(ctx context.Context, fn func(s Store) error) error
	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}
