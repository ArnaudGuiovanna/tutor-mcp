// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"
	"tutor-mcp/models"
	"tutor-mcp/store"
)

// interactionInput carries the minimal fields needed to persist an
// interaction and drive the BKT/FSRS update chain. Fields not
// relevant to a given call site (e.g. HintsRequested for submit_answer)
// should be left at their zero values.
type interactionInput struct {
	Concept                  string
	ActivityType             string
	Success                  bool
	ResponseTimeSeconds      float64
	Confidence               float64 // 0 if not provided
	ErrorType                string  // "" if not applicable
	Notes                    string
	HintsRequested           int
	SelfInitiated            bool
	CalibrationID            string
	MisconceptionType        string
	MisconceptionDetail      string
	DomainID                 string // persisted on the interaction row (issue #24)
	SessionID                string // durable session boundary; blank only for legacy internal callers
	AssessmentAttemptID      string
	DecisionID               string // copied from the frozen attempt, never a caller claim
	CurriculumVersion        int
	EvaluatorID              string
	EvaluationMethod         models.EvaluationMethod
	EvaluationProvenanceJSON string
	AssessmentScore          float64
	RubricJSON               string
	RubricScoreJSON          string
	Rubric                   any
	RubricScore              any
	RubricWarnings           []string
	RubricScoreWarnings      []string
	SemanticObservation      map[string]any
	InterpretationBrief      string
	TransferDimension        string
	TransferScore            *float64
	TransferSessionID        string
}

// applyInteraction persists the interaction and updates the learner's
// cognitive state (BKT, FSRS) for the concept. Returns the
// resulting ConceptState (post-update) and an error if any persistence
// step failed.
//
// Both model updates read a snapshot of the prior state and write the merged
// result once. Legacy theta is preserved for storage compatibility; generated
// tasks have no calibrated item parameters, and FSRS concept difficulty is not
// an IRT item difficulty.
//
// PFA is not part of the learner-model update or alert policy.
func applyInteraction(
	ctx context.Context,
	deps *Deps,
	learnerID string,
	input interactionInput,
	now time.Time,
) (*models.ConceptState, map[string]any, error) {
	if !isCognitiveEvidenceActivity(input.ActivityType) {
		return nil, nil, fmt.Errorf(
			"applyInteraction: activity_type %q is not learner evidence and cannot update BKT/FSRS",
			input.ActivityType,
		)
	}
	transferDimension, err := normalizeTransferEvidence(input.ActivityType, input.TransferDimension, input.TransferScore, input.TransferSessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("applyInteraction: %w", err)
	}
	input.TransferDimension = transferDimension

	// R008 / security-todo F-5.1: group the read-modify-write cycle
	// (concept_states + interaction + pedagogical_snapshot) under a
	// serializable transaction. Without this, concurrent record_interaction
	// calls on the same (learner, concept) both read a stale snapshot and
	// one silently overwrites the other on commit. WithTx maps to
	// BEGIN IMMEDIATE on SQLite (modernc.org/sqlite) so writers block at
	// tx start, not at first write.
	var resultCS *models.ConceptState
	var resultMeta map[string]any
	err = deps.Store.WithTx(ctx, func(s store.Store) error {
		// The store's for-update reads lock domain before assessment/model rows,
		// serializing semantic revision with this entire observation.
		// Lock and consume the assessment state in the same transaction as the
		// interaction/model update. A duplicate evaluator call therefore cannot
		// double-count one learner response, and any later write failure rolls the
		// evaluation back to submitted.
		if input.AssessmentAttemptID != "" {
			attempt, err := s.GetAssessmentAttemptForUpdate(ctx, learnerID, input.AssessmentAttemptID)
			if err != nil {
				return fmt.Errorf("load assessment attempt: %w", err)
			}
			if attempt.Status != models.AssessmentAttemptSubmitted || attempt.CurriculumInvalidatedVersion != 0 ||
				attempt.DomainID != input.DomainID || attempt.ConceptID != input.Concept ||
				attempt.ActivityType != input.ActivityType ||
				(attempt.SessionID != "" && attempt.SessionID != input.SessionID) {
				return fmt.Errorf("assessment attempt state or scope conflict")
			}
			if err := s.CompleteAssessmentEvaluation(
				ctx, learnerID, input.AssessmentAttemptID, input.RubricScoreJSON,
				input.EvaluatorID, input.EvaluationMethod, input.EvaluationProvenanceJSON,
				input.AssessmentScore, input.Success, now,
			); err != nil {
				return fmt.Errorf("complete assessment evaluation: %w", err)
			}
			input.DecisionID = attempt.DecisionID
			input.CurriculumVersion = attempt.CurriculumVersion
		}

		// Load or bootstrap concept state. GetOrCreateConceptStateForUpdate
		// materializes the row before taking the FOR UPDATE lock so concurrent
		// first-touch interactions on a brand-new (learner, concept) serialize
		// on Postgres instead of both bootstrapping and one overwriting the
		// other (lost update). On SQLite the materialize is a cheap no-op.
		cs, err := s.GetOrCreateConceptStateForUpdateInDomain(ctx, learnerID, input.DomainID, input.Concept)
		if err != nil {
			return fmt.Errorf("load concept state: %w", err)
		}
		observation := structuredObservation(input)

		// ── Snapshot read-only prior state ──────────────────────────────
		// All downstream algorithm steps read from this snapshot, never
		// from `cs` directly, to keep the BKT and FSRS updates
		// commutative. See doc comment above and issue #53.
		priorPMastery := cs.PMastery
		priorPLearn := cs.PLearn
		priorPForget := cs.PForget
		priorPSlip := cs.PSlip
		priorPGuess := cs.PGuess
		priorStability := cs.Stability
		priorDifficulty := cs.Difficulty
		priorElapsedDays := cs.ElapsedDays
		priorScheduledDays := cs.ScheduledDays
		priorReps := cs.Reps
		priorLapses := cs.Lapses
		priorCardState := cs.CardState
		priorTheta := cs.Theta
		var priorLastReview time.Time
		if cs.LastReview != nil {
			priorLastReview = *cs.LastReview
		}
		priorNextReview := cs.NextReview

		// ── BKT update — individualized over recent learner/concept
		// evidence, while preserving the existing error_type ramps for the
		// empty-history path. Computed up front (it only reads from the
		// prior snapshot + prior interactions) so the effective parameters
		// can be persisted and replayed deterministically.
		bktState := algorithms.BKTState{
			PMastery: priorPMastery,
			PLearn:   priorPLearn,
			PForget:  priorPForget,
			PSlip:    priorPSlip,
			PGuess:   priorPGuess,
		}
		recentForBKT, err := s.GetRecentInteractionsInDomain(ctx, learnerID, input.DomainID, input.Concept, 20)
		if err != nil {
			deps.Logger.Warn("applyInteraction: individualized BKT profile unavailable", "err", err, "learner", learnerID, "concept", input.Concept)
			recentForBKT = nil
		}
		bktProfile := buildIndividualBKTProfile(recentForBKT, priorStability)
		bktMode := engine.BKTUpdateModeForActivity(models.ActivityType(input.ActivityType))
		var bktResult algorithms.IndividualBKTUpdateResult
		if bktMode == models.BKTLearningOpportunity {
			bktResult = algorithms.BKTUpdateIndividualized(bktState, bktProfile, input.Success, input.ErrorType)
		} else {
			bktResult = algorithms.BKTObserveIndividualized(bktState, bktProfile, input.Success, input.ErrorType)
		}
		bktState = bktResult.State
		slipUsed := bktResult.Params.PSlip
		guessUsed := bktResult.Params.PGuess

		observation = mergeObservation(observation, map[string]any{
			"bkt_update_mode":            bktMode,
			"bkt_transition_applied":     bktResult.TransitionApplied,
			"bkt_posterior":              bktResult.PosteriorMastery,
			"bkt_transition_delta":       bktState.PMastery - bktResult.PosteriorMastery,
			"bkt_forget":                 bktResult.Params.PForget,
			"bkt_individualized_profile": individualBKTProfileSnapshot(bktProfile),
			"bkt_individualized_params":  individualBKTParamsSnapshot(bktResult.Params),
		})

		// Build and persist the interaction row.
		interaction := &models.Interaction{
			LearnerID:           learnerID,
			SessionID:           input.SessionID,
			AssessmentAttemptID: input.AssessmentAttemptID,
			Concept:             input.Concept,
			ActivityType:        input.ActivityType,
			Success:             input.Success,
			ResponseTime:        int(input.ResponseTimeSeconds),
			Confidence:          input.Confidence,
			ErrorType:           input.ErrorType,
			Notes:               input.Notes,
			HintsRequested:      input.HintsRequested,
			SelfInitiated:       input.SelfInitiated,
			CalibrationID:       input.CalibrationID,
			DomainID:            input.DomainID,
			BKTSlip:             &slipUsed,
			BKTGuess:            &guessUsed,
			RubricJSON:          input.RubricJSON,
			RubricScoreJSON:     input.RubricScoreJSON,
			CreatedAt:           now,
		}

		// Misconception fields — only stored on failures.
		if !input.Success && input.MisconceptionType != "" {
			interaction.MisconceptionType = input.MisconceptionType
			interaction.MisconceptionDetail = input.MisconceptionDetail
		}

		// Proactive review flag — derived from the prior schedule.
		if priorNextReview != nil && priorNextReview.After(now) && priorCardState != "new" {
			interaction.IsProactiveReview = true
		}

		if err := s.CreateInteraction(ctx, interaction); err != nil {
			return fmt.Errorf("create interaction: %w", err)
		}
		if input.TransferScore != nil {
			transferSessionID := input.SessionID
			if transferSessionID == "" {
				transferSessionID = input.TransferSessionID
			}
			record := &models.TransferRecord{
				LearnerID:           learnerID,
				DomainID:            input.DomainID,
				AssessmentAttemptID: input.AssessmentAttemptID,
				ConceptID:           input.Concept,
				ContextType:         input.TransferDimension,
				Score:               *input.TransferScore,
				SessionID:           transferSessionID,
			}
			if err := s.CreateTransferRecord(ctx, record); err != nil {
				return fmt.Errorf("create transfer evidence: %w", err)
			}
			observation = mergeObservation(observation, map[string]any{
				"transfer_evidence": map[string]any{
					"dimension": input.TransferDimension,
					"score":     *input.TransferScore,
					"recorded":  true,
				},
			})
		}

		// ── FSRS update — reads from prior snapshot. ───────────────────
		rating := algorithms.Good
		if !input.Success {
			rating = algorithms.Again
		} else if input.Confidence >= 0.9 {
			rating = algorithms.Easy
		} else if input.Confidence < 0.5 {
			rating = algorithms.Hard
		}

		fsrsCard := algorithms.FSRSCard{
			Stability:     priorStability,
			Difficulty:    priorDifficulty,
			ElapsedDays:   priorElapsedDays,
			ScheduledDays: priorScheduledDays,
			Reps:          priorReps,
			Lapses:        priorLapses,
			State:         algorithms.CardState(priorCardState),
			LastReview:    priorLastReview,
		}
		fsrsCard = algorithms.ReviewCard(fsrsCard, rating, now)

		// Preserve the legacy estimate. Updating it from FSRS would treat a
		// learner/concept memory parameter as the difficulty of this task.
		// Resume IRT only with an explicit, independently calibrated item model.
		newTheta := priorTheta

		// ── Single write-back of the merged result. ────────────────────
		cs.PMastery = bktState.PMastery
		cs.Stability = fsrsCard.Stability
		cs.Difficulty = fsrsCard.Difficulty
		cs.ElapsedDays = fsrsCard.ElapsedDays
		cs.ScheduledDays = fsrsCard.ScheduledDays
		cs.Reps = fsrsCard.Reps
		cs.Lapses = fsrsCard.Lapses
		cs.CardState = string(fsrsCard.State)
		cs.LastReview = &now
		nextReview := now.Add(time.Duration(fsrsCard.ScheduledDays) * 24 * time.Hour)
		cs.NextReview = &nextReview
		cs.Theta = newTheta

		// Persist updated concept state.
		if err := s.UpsertConceptState(ctx, cs); err != nil {
			return fmt.Errorf("upsert concept state: %w", err)
		}
		if err := s.CreatePedagogicalSnapshot(ctx, &models.PedagogicalSnapshot{
			InteractionID:       interaction.ID,
			LearnerID:           learnerID,
			DomainID:            input.DomainID,
			Concept:             input.Concept,
			ActivityType:        input.ActivityType,
			BeforeJSON:          mustSnapshotJSON(conceptStateSnapshot(priorPMastery, priorPLearn, priorPForget, priorPSlip, priorPGuess, priorStability, priorDifficulty, priorElapsedDays, priorScheduledDays, priorReps, priorLapses, priorCardState, priorTheta, priorLastReview, priorNextReview)),
			ObservationJSON:     mustSnapshotJSON(observationSnapshot(input, bktResult.Params.PLearn, slipUsed, guessUsed, observation)),
			AfterJSON:           mustSnapshotJSON(conceptStateAfterSnapshot(cs)),
			DecisionJSON:        mustSnapshotJSON(decisionSnapshot(input, interaction.IsProactiveReview)),
			InterpretationBrief: input.InterpretationBrief,
			CreatedAt:           now,
		}); err != nil {
			return fmt.Errorf("create pedagogical snapshot: %w", err)
		}

		resultCS = cs
		resultMeta = observation
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("applyInteraction: %w", err)
	}

	return resultCS, resultMeta, nil
}

func isCognitiveEvidenceActivity(activityType string) bool {
	switch models.ActivityType(activityType) {
	case models.ActivityRecall,
		models.ActivityDiagnosticAssessment,
		models.ActivityNewConcept,
		models.ActivityMasteryChallenge,
		models.ActivityDebuggingCase,
		models.ActivityPractice,
		models.ActivityDebugMisconception,
		models.ActivityFeynmanPrompt,
		models.ActivityTransferProbe:
		return true
	default:
		return false
	}
}

func structuredObservation(input interactionInput) map[string]any {
	observation := map[string]any{
		"evidence_verification": "unverified_observation",
	}
	if input.Rubric != nil {
		observation["rubric"] = input.Rubric
	}
	if input.RubricScore != nil {
		observation["rubric_score"] = input.RubricScore
	}
	if input.AssessmentAttemptID != "" {
		observation["evidence_verification"] = "assessment_linked_untrusted_evaluation"
		observation["assessment"] = map[string]any{
			"attempt_id":        input.AssessmentAttemptID,
			"score":             input.AssessmentScore,
			"derived_success":   input.Success,
			"evaluation_method": input.EvaluationMethod,
			"evaluator_id":      input.EvaluatorID,
			"trusted":           false,
		}
	}
	if len(input.RubricWarnings) > 0 {
		observation["rubric_schema_warnings"] = input.RubricWarnings
	}
	if len(input.RubricScoreWarnings) > 0 {
		observation["rubric_score_schema_warnings"] = input.RubricScoreWarnings
	}
	if input.SemanticObservation != nil {
		observation["semantic_observation"] = input.SemanticObservation
	}
	return observation
}

func conceptStateSnapshot(
	pMastery, pLearn, pForget, pSlip, pGuess, stability, difficulty float64,
	elapsedDays, scheduledDays, reps, lapses int,
	cardState string,
	theta float64,
	lastReview time.Time,
	nextReview *time.Time,
) map[string]any {
	out := map[string]any{
		"p_mastery":      pMastery,
		"p_learn":        pLearn,
		"p_forget":       pForget,
		"p_slip":         pSlip,
		"p_guess":        pGuess,
		"stability":      stability,
		"difficulty":     difficulty,
		"elapsed_days":   elapsedDays,
		"scheduled_days": scheduledDays,
		"reps":           reps,
		"lapses":         lapses,
		"card_state":     cardState,
		"theta":          theta,
	}
	if !lastReview.IsZero() {
		out["last_review"] = lastReview
	}
	if nextReview != nil {
		out["next_review"] = *nextReview
	}
	return out
}

func conceptStateAfterSnapshot(cs *models.ConceptState) map[string]any {
	var lastReview time.Time
	if cs.LastReview != nil {
		lastReview = *cs.LastReview
	}
	return conceptStateSnapshot(
		cs.PMastery, cs.PLearn, cs.PForget, cs.PSlip, cs.PGuess,
		cs.Stability, cs.Difficulty, cs.ElapsedDays, cs.ScheduledDays,
		cs.Reps, cs.Lapses, cs.CardState, cs.Theta, lastReview, cs.NextReview,
	)
}

func observationSnapshot(input interactionInput, learnUsed, slipUsed, guessUsed float64, observation map[string]any) map[string]any {
	out := map[string]any{
		"success":               input.Success,
		"response_time_seconds": input.ResponseTimeSeconds,
		"confidence":            input.Confidence,
		"error_type":            input.ErrorType,
		"hints_requested":       input.HintsRequested,
		"self_initiated":        input.SelfInitiated,
		"calibration_id":        input.CalibrationID,
		"bkt_learn":             learnUsed,
		"bkt_slip":              slipUsed,
		"bkt_guess":             guessUsed,
	}
	if !input.Success && input.MisconceptionType != "" {
		out["misconception_type"] = input.MisconceptionType
		out["misconception_detail"] = input.MisconceptionDetail
	}
	for k, v := range observation {
		out[k] = v
	}
	return out
}

func decisionSnapshot(input interactionInput, proactiveReview bool) map[string]any {
	return map[string]any{
		"domain_id":           input.DomainID,
		"activity_type":       input.ActivityType,
		"concept":             input.Concept,
		"is_proactive_review": proactiveReview,
		"source":              "record_interaction",
		"decision_id":         input.DecisionID,
		"curriculum_version":  input.CurriculumVersion,
		"policy_version":      models.PedagogicalPolicyVersion,
	}
}

func mustSnapshotJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
