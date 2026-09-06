// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

func ComputeAlerts(states []*models.ConceptState, recentInteractions []*models.Interaction, sessionStart time.Time) []models.Alert {
	return ComputeAlertsAt(states, recentInteractions, sessionStart, time.Now())
}

// MasteryAlertEvidence carries the protocol envelopes needed to distinguish
// routing observations from retained/trusted evidence for one concept.
type MasteryAlertEvidence struct {
	Transfers   []*models.TransferRecord
	Assessments []*models.AssessmentAttempt
}

// ComputeAlertsAt is the clock-injected variant of ComputeAlerts: it derives all
// elapsed-time computations (FORGETTING retention decay, OVERLOAD session length)
// from the supplied now rather than the wall clock, making the logic deterministic
// and testable. ComputeAlerts is a thin wrapper that passes time.Now().
func ComputeAlertsAt(states []*models.ConceptState, recentInteractions []*models.Interaction, sessionStart time.Time, now time.Time) []models.Alert {
	return ComputeAlertsWithEvidenceAt(states, recentInteractions, nil, sessionStart, now)
}

// ComputeAlertsWithEvidenceAt is the authoritative MASTERY_READY variant.
// Callers backed by persistence must provide evaluated attempts and transfer
// records per concept; absent evidence is conservative and cannot emit
// MASTERY_READY.
func ComputeAlertsWithEvidenceAt(states []*models.ConceptState, recentInteractions []*models.Interaction, evidenceByConcept map[string]MasteryAlertEvidence, sessionStart time.Time, now time.Time) []models.Alert {
	var alerts []models.Alert

	// criticalForgetting tracks concepts where FORGETTING fired at UrgencyCritical.
	// These concepts suppress same-concept MASTERY_READY and ZPD_DRIFT
	// below: retrieval recovery is the dominant action while retention is below
	// algorithms.RetentionAlertCriticalThreshold. FORGETTING at warning urgency
	// is less severe, so non-retrieval nudges can still be surfaced.
	criticalForgetting := make(map[string]bool)

	for _, cs := range states {
		if cs.CardState == "new" {
			continue
		}

		// FORGETTING: FSRS retention below the named alert warning threshold.
		retention := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
		if retention < algorithms.RetentionAlertWarningThreshold {
			urgency := models.UrgencyWarning
			if retention < algorithms.RetentionAlertCriticalThreshold {
				urgency = models.UrgencyCritical
				criticalForgetting[cs.Concept] = true
			}
			hoursLeft := 0.0
			if retention > algorithms.RetentionAlertCriticalThreshold {
				hoursLeft = (retention - algorithms.RetentionAlertCriticalThreshold) / 0.01 * 2
			}
			alerts = append(alerts, models.Alert{
				Type:               models.AlertForgetting,
				Concept:            cs.Concept,
				Urgency:            urgency,
				Retention:          retention,
				HoursUntilCritical: hoursLeft,
				RecommendedAction:  fmt.Sprintf("immediate review - %d minutes", estimateReviewMinutes(cs)),
			})
		}

		// MASTERY_READY means ready to attempt the challenge, not already
		// mastered. Use the same retained/diverse/uncertainty policy as
		// check_mastery; a high BKT estimate alone is insufficient.
		// Arbitration: if FORGETTING already fired at UrgencyCritical for this
		// concept, suppress MASTERY_READY to avoid emitting contradictory nudges
		// on the same tick.
		evidence := evidenceByConcept[cs.Concept]
		if masteryChallengeReadyForAlert(cs, recentInteractions, evidence.Transfers, evidence.Assessments, now) && !criticalForgetting[cs.Concept] {
			alerts = append(alerts, models.Alert{
				Type:              models.AlertMasteryReady,
				Concept:           cs.Concept,
				Urgency:           models.UrgencyInfo,
				RecommendedAction: "mastery challenge disponible",
			})
		}
	}

	// ZPD_DRIFT: 3+ consecutive failures on same concept (check from most recent)
	conceptFailStreaks := make(map[string]int)
	conceptProcessed := make(map[string]bool)
	for _, i := range recentInteractions {
		if conceptProcessed[i.Concept] {
			continue
		}
		if !i.Success {
			conceptFailStreaks[i.Concept]++
		} else {
			conceptProcessed[i.Concept] = true
		}
	}
	for concept, streak := range conceptFailStreaks {
		if criticalForgetting[concept] {
			continue
		}
		if streak >= 3 {
			errorRate := float64(streak) / float64(streak+1)

			// Analyze error types for richer recommendation
			recommendedAction := "reduce difficulty"
			errorTypeCounts := make(map[string]int)
			for _, i := range recentInteractions {
				if i.Concept == concept && !i.Success && i.ErrorType != "" {
					errorTypeCounts[i.ErrorType]++
				}
			}
			if errorTypeCounts["KNOWLEDGE_GAP"] >= 3 {
				recommendedAction = "reduce difficulty - conceptual gap - revisit the fundamentals"
			} else if errorTypeCounts["LOGIC_ERROR"] >= 3 {
				recommendedAction = "reduce difficulty - recurring logic errors - reasoning exercises"
			} else if errorTypeCounts["SYNTAX_ERROR"] >= 3 {
				recommendedAction = "reduce difficulty - recurring syntax errors - practice exercises"
			}

			alerts = append(alerts, models.Alert{
				Type:              models.AlertZPDDrift,
				Concept:           concept,
				Urgency:           models.UrgencyWarning,
				ErrorRate:         errorRate,
				RecommendedAction: recommendedAction,
			})
		}
	}

	// Generated items have no calibrated IRT difficulty. Legacy theta and
	// FSRS difficulty therefore cannot predict a ZPD alert; the failure-based
	// observation above remains available without claiming an item probability.

	// Historical PLATEAU alerts remain readable, but the runtime no longer
	// infers stalled learning from saturation of a cumulative probability.

	// OVERLOAD: session > 45 min
	if !sessionStart.IsZero() && now.Sub(sessionStart) > 45*time.Minute {
		alerts = append(alerts, models.Alert{
			Type:              models.AlertOverload,
			Urgency:           models.UrgencyInfo,
			RecommendedAction: "pause recommandee",
		})
	}

	return alerts
}

func masteryChallengeReadyForAlert(cs *models.ConceptState, interactions []*models.Interaction, transfers []*models.TransferRecord, assessments []*models.AssessmentAttempt, now time.Time) bool {
	if cs == nil {
		return false
	}
	relevant := make([]*models.Interaction, 0)
	for _, interaction := range interactions {
		if interaction == nil || interaction.Concept != cs.Concept {
			continue
		}
		if cs.LearnerID != "" && interaction.LearnerID != "" && interaction.LearnerID != cs.LearnerID {
			continue
		}
		relevant = append(relevant, interaction)
	}
	status := AssessMasteryStatus(cs.LearnerID, cs.Concept, cs, relevant, transfers, assessments, now)
	evidence := MasteryEvidenceQuality(BuildEvidenceProfile(cs.LearnerID, cs.Concept, relevant, now))
	uncertainty := ComputeMasteryUncertainty(cs, relevant, MasteryEvidenceProfile{Now: now})
	transfer := BuildTrustedTransferProfileFromEvidence(cs.Concept, transfers, assessments, now)
	return ReadyForMasteryChallenge(status, evidence, uncertainty, transfer)
}

func estimateReviewMinutes(cs *models.ConceptState) int {
	if cs.Lapses > 2 {
		return 12
	}
	return 8
}

// MetacognitiveAlertOptions holds optional data for metacognitive alerts.
type MetacognitiveAlertOptions struct {
	ConceptStates      []*models.ConceptState
	TransferRecords    []*models.TransferRecord
	CalibrationSamples int
}

type MetacognitiveAlertOption func(*MetacognitiveAlertOptions)

func WithTransferData(states []*models.ConceptState, transfers []*models.TransferRecord) MetacognitiveAlertOption {
	return func(o *MetacognitiveAlertOptions) {
		o.ConceptStates = states
		o.TransferRecords = transfers
	}
}

// WithCalibrationEvidence supplies the number of completed observations used
// to compute calibrationBias. A bias is not labelled as a persistent pattern
// until the shared calibration policy has enough evidence.
func WithCalibrationEvidence(samples int) MetacognitiveAlertOption {
	return func(o *MetacognitiveAlertOptions) {
		o.CalibrationSamples = samples
	}
}

// ComputeMetacognitiveAlerts computes the 4 new metacognitive alerts.
func ComputeMetacognitiveAlerts(
	autonomyScores []float64,
	calibrationBias float64,
	recentAffects []*models.AffectState,
	interactions []*models.Interaction,
	opts ...MetacognitiveAlertOption,
) []models.Alert {
	var options MetacognitiveAlertOptions
	for _, o := range opts {
		o(&options)
	}

	var alerts []models.Alert

	// Legacy autonomy scores remain accepted for compatibility. Their
	// composite has not been validated as evidence of increasing dependency
	// and no longer produces a pedagogical alert.

	// CALIBRATION_DIVERGING: normalized bias is actionable only after enough
	// completed predictions. This shares the same policy as the OLM and mirror
	// paths so one learner cannot receive contradictory calibration labels.
	if calibrationBiasIsActionable(calibrationBias, options.CalibrationSamples) {
		direction := "sur-estimation"
		if calibrationBias < 0 {
			direction = "sous-estimation"
		}
		alerts = append(alerts, models.Alert{
			Type:              models.AlertCalibrationDiverging,
			Urgency:           models.UrgencyWarning,
			RecommendedAction: fmt.Sprintf("calibration divergente: %s persistante", direction),
		})
	}

	// AFFECT_NEGATIVE: satisfaction ≤ 2 on 2 consecutive sessions
	if len(recentAffects) >= 2 {
		if recentAffects[0].Satisfaction > 0 && recentAffects[0].Satisfaction <= 2 &&
			recentAffects[1].Satisfaction > 0 && recentAffects[1].Satisfaction <= 2 {
			alerts = append(alerts, models.Alert{
				Type:              models.AlertAffectNegative,
				Urgency:           models.UrgencyWarning,
				RecommendedAction: "adapter le tutor_mode",
			})
		}
		// Also on perceived_difficulty = 1 on 2 consecutive
		if recentAffects[0].PerceivedDifficulty == 1 && recentAffects[1].PerceivedDifficulty == 1 {
			found := false
			for _, a := range alerts {
				if a.Type == models.AlertAffectNegative {
					found = true
					break
				}
			}
			if !found {
				alerts = append(alerts, models.Alert{
					Type:              models.AlertAffectNegative,
					Urgency:           models.UrgencyWarning,
					RecommendedAction: "adapter le tutor_mode",
				})
			}
		}
	}

	// TRANSFER_BLOCKED: PMastery >= MasteryBKT() but transfer_score < 0.50 on 2+ contexts
	if options.ConceptStates != nil && options.TransferRecords != nil {
		masteryBKT := algorithms.MasteryBKT()
		mastered := make(map[string]bool)
		for _, cs := range options.ConceptStates {
			if cs.PMastery >= masteryBKT {
				mastered[cs.Concept] = true
			}
		}
		transferByConcept := make(map[string][]*models.TransferRecord)
		for _, tr := range options.TransferRecords {
			transferByConcept[tr.ConceptID] = append(transferByConcept[tr.ConceptID], tr)
		}
		for concept := range mastered {
			records := transferByConcept[concept]
			lowContexts := 0
			for _, tr := range records {
				if tr.Score < 0.50 {
					lowContexts++
				}
			}
			if lowContexts >= 2 {
				alerts = append(alerts, models.Alert{
					Type:              models.AlertTransferBlocked,
					Concept:           concept,
					Urgency:           models.UrgencyWarning,
					RecommendedAction: "feynman challenge recommande",
				})
			}
		}
	}

	return alerts
}
