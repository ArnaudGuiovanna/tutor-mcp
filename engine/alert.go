// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"math"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

func ComputeAlerts(states []*models.ConceptState, recentInteractions []*models.Interaction, sessionStart time.Time) []models.Alert {
	return ComputeAlertsAt(states, recentInteractions, sessionStart, time.Now())
}

// ComputeAlertsAt is the clock-injected variant of ComputeAlerts: it derives all
// elapsed-time computations (FORGETTING retention decay, OVERLOAD session length)
// from the supplied now rather than the wall clock, making the logic deterministic
// and testable. ComputeAlerts is a thin wrapper that passes time.Now().
func ComputeAlertsAt(states []*models.ConceptState, recentInteractions []*models.Interaction, sessionStart time.Time, now time.Time) []models.Alert {
	var alerts []models.Alert

	// criticalForgetting tracks concepts where FORGETTING fired at UrgencyCritical.
	// These concepts suppress same-concept MASTERY_READY, ZPD_DRIFT and PLATEAU
	// below: retrieval recovery is the dominant action while retention is below
	// algorithms.RetentionAlertCriticalThreshold. FORGETTING at warning urgency
	// is less severe, so non-retrieval nudges can still be surfaced.
	criticalForgetting := make(map[string]bool)

	for _, cs := range states {
		if cs.CardState == "new" {
			continue
		}

		// FORGETTING: FSRS retention below the named alert warning threshold.
		elapsed := cs.ElapsedDays
		if cs.LastReview != nil {
			elapsed = int(now.Sub(*cs.LastReview).Hours() / 24)
		}
		retention := algorithms.Retrievability(elapsed, cs.Stability)
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

		// MASTERY_READY: BKT >= 0.85
		// Arbitration: if FORGETTING already fired at UrgencyCritical for this
		// concept, suppress MASTERY_READY to avoid emitting contradictory nudges
		// on the same tick.
		if cs.PMastery >= algorithms.MasteryBKT() && !criticalForgetting[cs.Concept] {
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

	// Predictive ZPD via IRT: probability of success below 55% signals incoming drift
	// before 3 failures accumulate. Only for concepts with review history (Reps > 0).
	zpdConcepts := make(map[string]bool)
	for _, a := range alerts {
		if a.Type == models.AlertZPDDrift {
			zpdConcepts[a.Concept] = true
		}
	}
	for _, cs := range states {
		if cs.CardState == "new" || cs.Reps == 0 || zpdConcepts[cs.Concept] || criticalForgetting[cs.Concept] {
			continue
		}
		irtDiff := algorithms.FSRSDifficultyToIRT(cs.Difficulty)
		pCorrect := algorithms.IRTProbability(cs.Theta, irtDiff, 1.0)
		if pCorrect < 0.55 {
			alerts = append(alerts, models.Alert{
				Type:              models.AlertZPDDrift,
				Concept:           cs.Concept,
				Urgency:           models.UrgencyInfo,
				ErrorRate:         1.0 - pCorrect,
				RecommendedAction: fmt.Sprintf("IRT: success probability at %.0f%% - reduce difficulty", pCorrect*100),
			})
		}
	}

	// PLATEAU: PFA probability stagnation (sigmoid saturates at extremes).
	// recentInteractions arrives newest-first (DB ORDER BY created_at DESC).
	// Iterate in reverse so PFAState evolves chronologically — PFADetectPlateau
	// examines scores[len-minCount:] which must be the *most recent* states.
	conceptInteractions := groupByConcept(recentInteractions)
	for concept, interactions := range conceptInteractions {
		if criticalForgetting[concept] {
			continue
		}
		if len(interactions) >= 4 {
			var scores []float64
			state := algorithms.PFAState{}
			for idx := len(interactions) - 1; idx >= 0; idx-- {
				state = algorithms.PFAUpdate(state, interactions[idx].Success)
				scores = append(scores, algorithms.PFAProbability(state))
			}
			if algorithms.PFADetectPlateau(scores, 4) {
				alerts = append(alerts, models.Alert{
					Type:              models.AlertPlateau,
					Concept:           concept,
					Urgency:           models.UrgencyWarning,
					SessionsStalled:   len(interactions),
					RecommendedAction: "change format - real-world case to debug",
				})
			}
		}
	}

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

func estimateReviewMinutes(cs *models.ConceptState) int {
	if cs.Lapses > 2 {
		return 12
	}
	return 8
}

func groupByConcept(interactions []*models.Interaction) map[string][]*models.Interaction {
	m := make(map[string][]*models.Interaction)
	for _, i := range interactions {
		m[i.Concept] = append(m[i.Concept], i)
	}
	return m
}

// MetacognitiveAlertOptions holds optional data for metacognitive alerts.
type MetacognitiveAlertOptions struct {
	ConceptStates   []*models.ConceptState
	TransferRecords []*models.TransferRecord
}

type MetacognitiveAlertOption func(*MetacognitiveAlertOptions)

func WithTransferData(states []*models.ConceptState, transfers []*models.TransferRecord) MetacognitiveAlertOption {
	return func(o *MetacognitiveAlertOptions) {
		o.ConceptStates = states
		o.TransferRecords = transfers
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

	// DEPENDENCY_INCREASING: autonomy_score declining over 3 consecutive sessions
	if len(autonomyScores) >= 3 {
		declining := true
		for i := 0; i < 2; i++ {
			if autonomyScores[i] >= autonomyScores[i+1] {
				declining = false
				break
			}
		}
		if declining {
			alerts = append(alerts, models.Alert{
				Type:              models.AlertDependencyIncreasing,
				Urgency:           models.UrgencyWarning,
				RecommendedAction: "miroir metacognitif active",
			})
		}
	}

	// CALIBRATION_DIVERGING: |calibration_bias| > 1.5
	if math.Abs(calibrationBias) > 1.5 {
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
