// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	assessmentrules "tutor-mcp/assessment"
	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type RecordInteractionParams struct {
	IdempotentMutationParams
	Concept              string          `json:"concept" jsonschema:"the concept being practiced"`
	ActivityType         string          `json:"activity_type" jsonschema:"evidence-bearing activity type; with attempt_id, the type of the prepared attempt"`
	Success              bool            `json:"success" jsonschema:"whether the exercise was completed successfully"`
	ResponseTimeSeconds  float64         `json:"response_time_seconds" jsonschema:"response time in seconds"`
	Confidence           float64         `json:"confidence" jsonschema:"estimated confidence as a 0..1 float"`
	ErrorType            string          `json:"error_type,omitempty" jsonschema:"error class on failure; omit on success"`
	Notes                string          `json:"notes,omitempty" jsonschema:"optional notes about the interaction"`
	DomainID             string          `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
	SessionID            string          `json:"session_id,omitempty" jsonschema:"durable learning session ID; omit only for legacy clients, which resume or open the active session"`
	AssessmentAttemptID  string          `json:"attempt_id,omitempty" jsonschema:"submitted assessment attempt whose frozen rubric is being evaluated"`
	EvaluatorID          string          `json:"evaluator_id,omitempty" jsonschema:"identity/version of the evaluator; required with attempt_id"`
	EvaluationMethod     string          `json:"evaluation_method,omitempty" jsonschema:"host_llm for MCP-side grading; trusted external/human evaluation is assigned only by server-side boundaries"`
	EvaluationProvenance json.RawMessage `json:"evaluation_provenance,omitempty" jsonschema:"optional object describing evaluator model/version/trace identifiers"`
	HintsRequested       int             `json:"hints_requested,omitempty" jsonschema:"number of hints requested during the exchange (optional, default 0)"`
	SelfInitiated        bool            `json:"self_initiated,omitempty" jsonschema:"true if the session started without a webhook alert"`
	CalibrationID        string          `json:"calibration_id,omitempty" jsonschema:"id of the associated calibration prediction (optional)"`
	MisconceptionType    string          `json:"misconception_type,omitempty" jsonschema:"free-form label of the detected misconception (optional, ignored if success=true)"`
	MisconceptionDetail  string          `json:"misconception_detail,omitempty" jsonschema:"one-sentence description of the misconception (optional)"`
	Rubric               json.RawMessage `json:"rubric,omitempty" jsonschema:"optional rubric of an observation without attempt_id; never sent with attempt_id, whose rubric is frozen. Scoring fields such as evidence or total belong to rubric_score"`
	RubricScore          json.RawMessage `json:"rubric_score,omitempty" jsonschema:"rubric scoring result: one criteria_scores entry per frozen rubric criterion with the exact criterion id, a numeric score and concrete evidence. Required with attempt_id"`
	SemanticObservation  json.RawMessage `json:"semantic_observation,omitempty" jsonschema:"optional semantic observation object (for example reasoning_quality, success_mode, confidence_alignment)"`
	InterpretationBrief  string          `json:"interpretation_brief,omitempty" jsonschema:"optional brief hypothesis produced before the activity, stored for pedagogical audit"`
	TransferDimension    string          `json:"transfer_dimension,omitempty" jsonschema:"required for TRANSFER_PROBE: near, far, debugging, teaching, or creative"`
	TransferScore        *float64        `json:"transfer_score,omitempty" jsonschema:"required for TRANSFER_PROBE: observed transfer score from 0..1; omit for every other activity type"`
	TransferSessionID    string          `json:"transfer_session_id,omitempty" jsonschema:"optional session correlation ID for TRANSFER_PROBE"`
}

func registerRecordInteraction(server *mcp.Server, deps *Deps) {
	addTool(server, deps, &mcp.Tool{
		Name:        "record_interaction",
		Description: "Record an observed result and update the learner model. For assessment evidence, pass a submitted attempt_id: the server loads its pre-frozen rubric, derives success from criterion scores, and completes the evaluation atomically with the interaction. Calls without attempt_id remain explicit unverified routing observations and cannot establish retained, demonstrated, or transferred evidence.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordInteractionParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_interaction", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if params.Concept == "" {
			r, _ := errorResult("concept is required")
			return r, nil, nil
		}

		// String length caps (issue #31). Without these guards a misbehaving
		// caller could push multi-MB strings into Notes / MisconceptionDetail
		// and bloat the interactions table, plus orphan rows that read-side
		// filters cannot fully hide.
		stringFields := []struct {
			name  string
			value string
			max   int
		}{
			{"concept", params.Concept, maxShortLabelLen},
			{"activity_type", params.ActivityType, maxShortLabelLen},
			{"error_type", params.ErrorType, maxShortLabelLen},
			{"calibration_id", params.CalibrationID, maxShortLabelLen},
			{"misconception_type", params.MisconceptionType, maxShortLabelLen},
			{"misconception_detail", params.MisconceptionDetail, maxNoteLen},
			{"notes", params.Notes, maxNoteLen},
			{"interpretation_brief", params.InterpretationBrief, maxNoteLen},
			{"session_id", params.SessionID, maxShortLabelLen},
			{"attempt_id", params.AssessmentAttemptID, maxShortLabelLen},
			{"evaluator_id", params.EvaluatorID, maxShortLabelLen},
			{"evaluation_method", params.EvaluationMethod, maxShortLabelLen},
			{"evaluation_provenance", rawJSONText(params.EvaluationProvenance), maxLongTextLen},
			{"transfer_dimension", params.TransferDimension, maxShortLabelLen},
			{"transfer_session_id", params.TransferSessionID, maxShortLabelLen},
		}
		for _, f := range stringFields {
			if err := validateString(f.name, f.value, f.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Enum whitelist for activity_type and error_type (issue #88).
		// Without these guards the LLM has to guess from the prose schema
		// description ("RECALL_EXERCISE, NEW_CONCEPT, etc."), and typos
		// like "RECALL" leak into the audit row, escape downstream filters,
		// and silently degrade the BKT slip-by-error-type heuristic plus
		// alert.go's errorTypeCounts aggregation.
		if err := validateEnum("activity_type", params.ActivityType, allowedActivityTypes); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		transferDimension, err := normalizeTransferEvidence(
			params.ActivityType,
			params.TransferDimension,
			params.TransferScore,
			params.TransferSessionID,
		)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		// error_type is optional — empty string passes through unchecked.
		if params.ErrorType != "" {
			if err := validateEnum("error_type", params.ErrorType, allowedErrorTypes); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Numeric range validation. Without these guards the BKT/FSRS chain
		// silently absorbs garbage scores (confidence>1, negative response
		// time, hint counts in the thousands) and corrupts the learner's
		// cognitive estimate. See issue #25.
		if err := validateUnitInterval("confidence", params.Confidence); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateNonNegativeDuration("response_time_seconds", params.ResponseTimeSeconds, 24*3600); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateNonNegativeCount("hints_requested", params.HintsRequested, 50); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		rubric, rubricWarnings, err := normalizeRubricJSON(rawJSONText(params.Rubric))
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		var rubricScore map[string]any
		var rubricScoreWarnings []string
		if params.AssessmentAttemptID == "" {
			rubricScore, rubricScoreWarnings, err = normalizeRubricScoreJSON(rawJSONText(params.RubricScore), rubric)
			if err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		rubricJSON := ""
		if rubric != nil {
			rubricJSON = mustSnapshotJSON(rubric)
		}
		rubricScoreJSON := ""
		if rubricScore != nil {
			rubricScoreJSON = mustSnapshotJSON(rubricScore)
		}
		semanticObservation, err := normalizeSemanticObservationJSON(rawJSONText(params.SemanticObservation))
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		interpretationBrief := normalizeInterpretationBrief(params.InterpretationBrief, params.Notes)

		// Resolve the active domain (honoring the optional domain_id) and
		// validate the concept against its concept list. Without this guard
		// the BKT/FSRS chain silently inserts orphan concept_states for
		// hallucinated or stale concept names — see issue #23.
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("record_interaction: domain not found by id", append(diagnosticErrorAttrs(err), "learner", learnerID, "domain_id", params.DomainID)...)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("record_interaction: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult(deps)
			return r, nil, nil
		}
		if err := validateConceptInDomain(domain, params.Concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		now := time.Now().UTC()
		requestedSessionID := params.SessionID
		if params.TransferSessionID != "" {
			if requestedSessionID != "" && requestedSessionID != params.TransferSessionID {
				r, _ := errorResult("session_id and transfer_session_id must match")
				return r, nil, nil
			}
			requestedSessionID = params.TransferSessionID
		}
		learningSession, err := resolveOpenLearningSession(ctx, deps, learnerID, domain.ID, requestedSessionID, now)
		if err != nil {
			return learningSessionResolutionErrorResult(deps, err), nil, nil
		}

		assessmentScore := 0.0
		assessmentPassed := params.Success
		evaluationProvenanceJSON := ""
		if params.AssessmentAttemptID != "" {
			if rawJSONText(params.Rubric) != "" {
				r, _ := errorResult("rubric must be frozen by prepare_assessment_attempt and omitted from record_interaction")
				return r, nil, nil
			}
			if params.EvaluatorID == "" || params.EvaluationMethod != string(models.EvaluationMethodHostLLM) {
				r, _ := errorResult("attempt evaluation requires evaluator_id and evaluation_method=host_llm; trusted methods are accepted only through a server-side evaluator boundary")
				return r, nil, nil
			}
			assessment, getErr := deps.Store.GetAssessmentAttempt(ctx, learnerID, params.AssessmentAttemptID)
			if getErr != nil {
				r, _ := errorResult("assessment attempt not found")
				return r, nil, nil
			}
			if assessment.CurriculumInvalidatedVersion != 0 {
				r, _ := errorResult("assessment curriculum definition changed: prepare a new task against the current curriculum")
				return r, nil, nil
			}
			if assessment.Status != models.AssessmentAttemptSubmitted {
				r, _ := errorResult("assessment attempt must be submitted before evaluation")
				return r, nil, nil
			}
			if assessment.DomainID != domain.ID || assessment.ConceptID != params.Concept || assessment.ActivityType != params.ActivityType {
				r, _ := errorResult("assessment attempt does not match domain, concept, or activity_type")
				return r, nil, nil
			}
			if assessment.SessionID != "" && assessment.SessionID != learningSession.ID {
				r, _ := errorResult("assessment attempt belongs to another learning session")
				return r, nil, nil
			}
			var boundRubric *models.AssessmentRubric
			if assessment.DecisionID != "" {
				parsed, parseErr := normalizeAssessmentRubric(assessment.RubricJSON)
				if parseErr != nil {
					r, _ := errorResult("stored bound assessment rubric is invalid")
					return r, nil, nil
				}
				boundRubric = &parsed
				rubric = assessmentRubricMap(parsed)
				rubricWarnings = nil
			} else {
				rubric, rubricWarnings, err = normalizeRubricJSON(assessment.RubricJSON)
				if err != nil || rubric == nil {
					r, _ := errorResult("stored assessment rubric is invalid")
					return r, nil, nil
				}
			}
			if boundRubric != nil {
				var result assessmentrules.Result
				result, err = assessmentrules.EvaluateJSON(*boundRubric, rawJSONText(params.RubricScore))
				err = publicRubricError(err)
				assessmentScore, assessmentPassed = result.Total, result.Passed
				rubricScore, rubricScoreWarnings = result.Score, nil
			} else {
				rubricScore, rubricScoreWarnings, err = normalizeRubricScoreJSON(rawJSONText(params.RubricScore), rubric)
				if err == nil && rubricScore == nil {
					err = fmt.Errorf("rubric_score is required with attempt_id")
				}
				if err == nil {
					assessmentScore, assessmentPassed, err = deriveAssessmentOutcome(rubric, rubricScore)
				}
			}
			if err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
			if params.Success != assessmentPassed {
				r, _ := errorResult("success contradicts the outcome derived from the frozen rubric")
				return r, nil, nil
			}
			if params.TransferScore != nil {
				maxTotal := assessmentMaxTotal(assessment.RubricJSON, rubric)
				if err := checkLinkedTransferScore(*params.TransferScore, assessmentScore, maxTotal); err != nil {
					r, _ := errorResult(err.Error())
					return r, nil, nil
				}
			}
			if rawJSONText(params.EvaluationProvenance) != "" {
				provenance, provenanceErr := normalizeJSONObject("evaluation_provenance", rawJSONText(params.EvaluationProvenance))
				if provenanceErr != nil {
					r, _ := errorResult(provenanceErr.Error())
					return r, nil, nil
				}
				evaluationProvenanceJSON = mustSnapshotJSON(provenance)
			}
			rubricJSON = assessment.RubricJSON
			rubricScoreJSON = mustSnapshotJSON(rubricScore)
		} else if params.EvaluatorID != "" || params.EvaluationMethod != "" || rawJSONText(params.EvaluationProvenance) != "" {
			r, _ := errorResult("evaluator fields are only valid with attempt_id")
			return r, nil, nil
		}

		cs, observation, err := applyInteraction(ctx, deps, learnerID, interactionInput{
			Concept:                  params.Concept,
			ActivityType:             params.ActivityType,
			Success:                  assessmentPassed,
			ResponseTimeSeconds:      params.ResponseTimeSeconds,
			Confidence:               params.Confidence,
			ErrorType:                params.ErrorType,
			Notes:                    params.Notes,
			HintsRequested:           params.HintsRequested,
			SelfInitiated:            params.SelfInitiated,
			CalibrationID:            params.CalibrationID,
			MisconceptionType:        params.MisconceptionType,
			MisconceptionDetail:      params.MisconceptionDetail,
			DomainID:                 domain.ID,
			SessionID:                learningSession.ID,
			AssessmentAttemptID:      params.AssessmentAttemptID,
			EvaluatorID:              params.EvaluatorID,
			EvaluationMethod:         models.EvaluationMethod(params.EvaluationMethod),
			EvaluationProvenanceJSON: evaluationProvenanceJSON,
			AssessmentScore:          assessmentScore,
			RubricJSON:               rubricJSON,
			RubricScoreJSON:          rubricScoreJSON,
			Rubric:                   rubric,
			RubricScore:              rubricScore,
			RubricWarnings:           rubricWarnings,
			RubricScoreWarnings:      rubricScoreWarnings,
			SemanticObservation:      semanticObservation,
			InterpretationBrief:      interpretationBrief,
			TransferDimension:        transferDimension,
			TransferScore:            params.TransferScore,
			TransferSessionID:        params.TransferSessionID,
		}, now)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to record interaction", err)
			return r, nil, nil
		}

		var degradedComponents []string
		if err := deps.Store.UpdateLastActive(ctx, learnerID); err != nil {
			degradedComponents = append(degradedComponents, "last_active")
			deps.Logger.Warn("record_interaction: update last active failed", append(diagnosticErrorAttrs(err), "learner", learnerID)...)
		}
		pushNow := time.Now().UTC()
		if err := deps.Store.MarkWebhookPushConceptAddressed(ctx, learnerID, domain.ID, params.Concept, pushNow, pushNow.Add(-7*24*time.Hour)); err != nil {
			degradedComponents = append(degradedComponents, "webhook_open_loop")
			deps.Logger.Warn("record_interaction: close webhook open loop failed", append(diagnosticErrorAttrs(err), "learner", learnerID, "domain", domain.ID)...)
		}

		deps.Logger.Debug("interaction recorded",
			"learner", learnerID,
			"session", learningSession.ID,
			"concept", params.Concept,
			"activity_type", params.ActivityType,
			"success", assessmentPassed,
			"assessment_attempt", params.AssessmentAttemptID,
			"hints_requested", params.HintsRequested,
			"self_initiated", params.SelfInitiated,
			"new_mastery_estimate", cs.PMastery,
			"new_theta", cs.Theta,
			"reps", cs.Reps,
		)

		// Compute engagement signal
		engagementSignal := "stable"
		if params.Confidence >= 0.8 && assessmentPassed {
			engagementSignal = "positive"
		} else if !assessmentPassed && params.Confidence < 0.3 {
			engagementSignal = "declining"
		}

		// Compute cognitive signals from session patterns
		sessionInteractions, err := deps.Store.GetInteractionsBySession(ctx, learnerID, learningSession.ID)
		if err != nil {
			degradedComponents = append(degradedComponents, "cognitive_signals")
			deps.Logger.Warn("record_interaction: session signal read failed", append(diagnosticErrorAttrs(err), "learner", learnerID)...)
		}
		fatigueSignal, frustrationSignal := computeCognitiveSignals(sessionInteractions)

		nextReviewHours := float64(cs.ScheduledDays) * 24.0

		evidenceVerification := "unverified_observation"
		if params.AssessmentAttemptID != "" {
			evidenceVerification = "assessment_linked_untrusted_evaluation"
		}
		payload := map[string]interface{}{
			"updated":               true,
			"session_id":            learningSession.ID,
			"new_mastery_estimate":  cs.PMastery,
			"new_mastery":           cs.PMastery, // deprecated estimate alias
			"next_review_in_hours":  nextReviewHours,
			"engagement_signal":     engagementSignal,
			"fatigue_signal":        fatigueSignal,
			"frustration_signal":    frustrationSignal,
			"evidence_verification": evidenceVerification,
		}
		if len(observation) > 0 {
			payload["observation"] = observation
		}
		if params.AssessmentAttemptID != "" {
			payload["assessment_attempt_id"] = params.AssessmentAttemptID
			payload["assessment_status"] = models.AssessmentAttemptEvaluated
			payload["derived_success"] = assessmentPassed
			payload["evaluation_trusted"] = false
		}
		if params.ActivityType == string(models.ActivityTransferProbe) {
			payload["transfer_recorded"] = true
			if records, transferErr := deps.Store.GetTransferScoresInDomain(ctx, learnerID, domain.ID, params.Concept); transferErr != nil {
				degradedComponents = append(degradedComponents, "transfer_profile")
				deps.Logger.Warn("record_interaction: transfer profile read failed", append(diagnosticErrorAttrs(transferErr), "learner", learnerID, "concept", params.Concept)...)
			} else {
				payload["transfer_profile"] = engine.BuildTransferProfile(params.Concept, records)
			}
		}
		if len(degradedComponents) > 0 {
			payload["degraded_components"] = degradedComponents
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

func normalizeTransferEvidence(activityType, dimension string, score *float64, sessionID string) (string, error) {
	isTransfer := activityType == string(models.ActivityTransferProbe)
	if !isTransfer {
		if strings.TrimSpace(dimension) != "" || score != nil || strings.TrimSpace(sessionID) != "" {
			return "", fmt.Errorf("transfer_dimension, transfer_score and transfer_session_id are only valid for TRANSFER_PROBE")
		}
		return "", nil
	}
	if strings.TrimSpace(dimension) == "" {
		return "", fmt.Errorf("transfer_dimension is required for TRANSFER_PROBE")
	}
	normalized, ok := engine.NormalizeTransferDimension(dimension)
	if !ok {
		return "", fmt.Errorf("transfer_dimension must be one of: near, far, debugging, teaching, creative (got %q)", dimension)
	}
	if score == nil {
		return "", fmt.Errorf("transfer_score is required for TRANSFER_PROBE")
	}
	if err := validateUnitInterval("transfer_score", *score); err != nil {
		return "", err
	}
	return string(normalized), nil
}

// linkedTransferScoreTolerance is the largest accepted gap between a declared
// transfer_score and the ratio derived from the frozen rubric.
const linkedTransferScoreTolerance = 0.05

// checkLinkedTransferScore rejects an attempt-linked transfer_score that does
// not describe the graded response. The stored score is always the derived
// ratio; the check only stops a host from reporting a contradictory number.
func checkLinkedTransferScore(declared, total, maxTotal float64) error {
	if maxTotal <= 0 {
		return nil
	}
	derived := min(1, max(0, total/maxTotal))
	if math.Abs(declared-derived) > linkedTransferScoreTolerance {
		return fmt.Errorf("transfer_score %.2f contradicts the frozen rubric: criterion scores total %.4g of %.4g (ratio %.2f); send transfer_score=%.2f", declared, total, maxTotal, derived, derived)
	}
	return nil
}

func normalizeInterpretationBrief(explicit, notes string) string {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed
	}
	return extractInterpretationBrief(notes)
}

func extractInterpretationBrief(raw string) string {
	lines := strings.Split(raw, "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), "## Interpretation brief") {
			start = i + 1
			continue
		}
		if start != -1 && strings.HasPrefix(strings.TrimSpace(line), "## ") {
			end = i
			break
		}
	}
	if start == -1 || start >= len(lines) {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

func normalizeSemanticObservationJSON(raw string) (map[string]any, error) {
	return normalizeJSONObject("semantic_observation", raw)
}

func normalizeJSONObject(field, raw string) (map[string]any, error) {
	if err := validateString(field, raw, maxLongTextLen); err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %v", field, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("%s must be valid JSON: %v", field, err)
		}
		return nil, fmt.Errorf("%s must contain a single JSON value", field)
	}

	observation, ok := parsed.(map[string]any)
	if !ok || observation == nil {
		return nil, fmt.Errorf("%s must be a JSON object", field)
	}
	return observation, nil
}

// computeCognitiveSignals analyzes session interaction patterns for fatigue and frustration.
func computeCognitiveSignals(sessionInteractions []*models.Interaction) (fatigue string, frustration string) {
	fatigue = "none"
	frustration = "none"

	if len(sessionInteractions) < 3 {
		return
	}

	// Fatigue: declining accuracy + increasing response time in last N interactions
	// Look at the most recent 5 interactions (they're sorted newest-first)
	window := sessionInteractions
	if len(window) > 5 {
		window = window[:5]
	}

	recentSuccesses := 0
	recentTotalTime := 0
	for _, i := range window {
		if i.Success {
			recentSuccesses++
		}
		recentTotalTime += i.ResponseTime
	}
	recentRate := float64(recentSuccesses) / float64(len(window))
	avgRecentTime := float64(recentTotalTime) / float64(len(window))

	// Compare with earlier interactions if available
	if len(sessionInteractions) >= 6 {
		earlier := sessionInteractions[len(window):]
		if len(earlier) > 5 {
			earlier = earlier[:5]
		}
		earlySuccesses := 0
		earlyTotalTime := 0
		for _, i := range earlier {
			if i.Success {
				earlySuccesses++
			}
			earlyTotalTime += i.ResponseTime
		}
		earlyRate := float64(earlySuccesses) / float64(len(earlier))
		avgEarlyTime := float64(earlyTotalTime) / float64(len(earlier))

		// Fatigue: accuracy drops AND response time increases
		if recentRate < earlyRate-0.2 && avgRecentTime > avgEarlyTime*1.3 {
			fatigue = "high"
		} else if recentRate < earlyRate-0.1 || avgRecentTime > avgEarlyTime*1.2 {
			fatigue = "moderate"
		}
	}

	// Frustration: consecutive failures + low confidence
	consecutiveFailures := 0
	lowConfidenceCount := 0
	for _, i := range window {
		if !i.Success {
			consecutiveFailures++
			if i.Confidence < 0.3 {
				lowConfidenceCount++
			}
		} else {
			break
		}
	}

	if consecutiveFailures >= 3 && lowConfidenceCount >= 2 {
		frustration = "high"
	} else if consecutiveFailures >= 2 && lowConfidenceCount >= 1 {
		frustration = "moderate"
	}

	return
}
