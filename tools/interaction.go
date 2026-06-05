// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type RecordInteractionParams struct {
	Concept                 string  `json:"concept" jsonschema:"the concept being practiced"`
	ActivityType            string  `json:"activity_type" jsonschema:"activity type - MUST be one of the canonical values: RECALL_EXERCISE, NEW_CONCEPT, MASTERY_CHALLENGE, DEBUGGING_CASE, REST, SETUP_DOMAIN, PRACTICE, DEBUG_MISCONCEPTION, FEYNMAN_PROMPT, TRANSFER_PROBE, CLOSE_SESSION"`
	Success                 bool    `json:"success" jsonschema:"whether the exercise was completed successfully"`
	ResponseTimeSeconds     float64 `json:"response_time_seconds" jsonschema:"response time in seconds"`
	Confidence              float64 `json:"confidence" jsonschema:"estimated confidence as a 0..1 float"`
	ErrorType               string  `json:"error_type,omitempty" jsonschema:"error type on failure - leave empty or use exactly: SYNTAX_ERROR, LOGIC_ERROR, KNOWLEDGE_GAP"`
	Notes                   string  `json:"notes" jsonschema:"optional notes about the interaction"`
	DomainID                string  `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
	HintsRequested          int     `json:"hints_requested,omitempty" jsonschema:"number of hints requested during the exchange (optional, default 0)"`
	SelfInitiated           bool    `json:"self_initiated,omitempty" jsonschema:"true if the session started without a webhook alert"`
	CalibrationID           string  `json:"calibration_id,omitempty" jsonschema:"id of the associated calibration prediction (optional)"`
	MisconceptionType       string  `json:"misconception_type,omitempty" jsonschema:"free-form label of the detected misconception (optional, ignored if success=true)"`
	MisconceptionDetail     string  `json:"misconception_detail,omitempty" jsonschema:"one-sentence description of the misconception (optional)"`
	RubricJSON              string  `json:"rubric_json,omitempty" jsonschema:"optional rubric as a JSON object or array"`
	RubricScoreJSON         string  `json:"rubric_score_json,omitempty" jsonschema:"optional rubric scoring result as a JSON object or array"`
	SemanticObservationJSON string  `json:"semantic_observation_json,omitempty" jsonschema:"optional semantic observation as a JSON object"`
	InterpretationBrief     string  `json:"interpretation_brief,omitempty" jsonschema:"optional brief hypothesis produced before the activity, stored for pedagogical audit"`
}

func registerRecordInteraction(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_interaction",
		Description: "Record the result of an exercise and update the learner's cognitive state. Supports error_type to adjust the BKT model according to the error type, optional rubric_json/rubric_score_json for structured grading evidence, and optional semantic_observation_json for richer qualitative observations.",
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
		rubric, rubricWarnings, err := normalizeRubricJSON(params.RubricJSON)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		rubricScore, rubricScoreWarnings, err := normalizeRubricScoreJSON(params.RubricScoreJSON, rubric)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		rubricJSON := ""
		if rubric != nil {
			rubricJSON = mustSnapshotJSON(rubric)
		}
		rubricScoreJSON := ""
		if rubricScore != nil {
			rubricScoreJSON = mustSnapshotJSON(rubricScore)
		}
		semanticObservation, err := normalizeSemanticObservationJSON(params.SemanticObservationJSON)
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
				deps.Logger.Error("record_interaction: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("record_interaction: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}
		if err := validateConceptInDomain(domain, params.Concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		cs, observation, err := applyInteraction(ctx, deps, learnerID, interactionInput{
			Concept:             params.Concept,
			ActivityType:        params.ActivityType,
			Success:             params.Success,
			ResponseTimeSeconds: params.ResponseTimeSeconds,
			Confidence:          params.Confidence,
			ErrorType:           params.ErrorType,
			Notes:               params.Notes,
			HintsRequested:      params.HintsRequested,
			SelfInitiated:       params.SelfInitiated,
			CalibrationID:       params.CalibrationID,
			MisconceptionType:   params.MisconceptionType,
			MisconceptionDetail: params.MisconceptionDetail,
			DomainID:            domain.ID,
			RubricJSON:          rubricJSON,
			RubricScoreJSON:     rubricScoreJSON,
			Rubric:              rubric,
			RubricScore:         rubricScore,
			RubricWarnings:      rubricWarnings,
			RubricScoreWarnings: rubricScoreWarnings,
			SemanticObservation: semanticObservation,
			InterpretationBrief: interpretationBrief,
		}, time.Now().UTC())
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to record interaction", err)
			return r, nil, nil
		}

		// Update last active
		_ = deps.Store.UpdateLastActive(ctx, learnerID)
		pushNow := time.Now().UTC()
		_ = deps.Store.MarkWebhookPushConceptAddressed(ctx, learnerID, domain.ID, params.Concept, pushNow, pushNow.Add(-7*24*time.Hour))

		deps.Logger.Info("interaction recorded",
			"learner", learnerID,
			"concept", params.Concept,
			"activity_type", params.ActivityType,
			"success", params.Success,
			"hints_requested", params.HintsRequested,
			"self_initiated", params.SelfInitiated,
			"new_mastery", cs.PMastery,
			"new_theta", cs.Theta,
			"reps", cs.Reps,
		)

		// Compute engagement signal
		engagementSignal := "stable"
		if params.Confidence >= 0.8 && params.Success {
			engagementSignal = "positive"
		} else if !params.Success && params.Confidence < 0.3 {
			engagementSignal = "declining"
		}

		// Compute cognitive signals from session patterns
		sessionInteractions, _ := deps.Store.GetSessionInteractions(ctx, learnerID)
		fatigueSignal, frustrationSignal := computeCognitiveSignals(sessionInteractions)

		nextReviewHours := float64(cs.ScheduledDays) * 24.0

		payload := map[string]interface{}{
			"updated":              true,
			"new_mastery":          cs.PMastery,
			"next_review_in_hours": nextReviewHours,
			"engagement_signal":    engagementSignal,
			"fatigue_signal":       fatigueSignal,
			"frustration_signal":   frustrationSignal,
		}
		if len(observation) > 0 {
			payload["observation"] = observation
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
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
	if err := validateString("semantic_observation_json", raw, maxLongTextLen); err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("semantic_observation_json must be valid JSON: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("semantic_observation_json must be valid JSON: %v", err)
		}
		return nil, fmt.Errorf("semantic_observation_json must contain a single JSON value")
	}

	observation, ok := parsed.(map[string]any)
	if !ok || observation == nil {
		return nil, fmt.Errorf("semantic_observation_json must be a JSON object")
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
