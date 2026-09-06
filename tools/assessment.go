// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type PrepareAssessmentAttemptParams struct {
	IdempotentMutationParams
	DomainID        string   `json:"domain_id,omitempty" jsonschema:"domain ID (optional; active domain used when absent)"`
	Concept         string   `json:"concept" jsonschema:"concept assessed"`
	SessionID       string   `json:"session_id,omitempty" jsonschema:"durable learning session ID when available"`
	ActivityID      string   `json:"activity_id,omitempty" jsonschema:"stable activity identifier; derived from immutable content when absent"`
	ActivityVersion int      `json:"activity_version,omitempty" jsonschema:"positive activity version; defaults to 1"`
	DecisionID      string   `json:"decision_id,omitempty" jsonschema:"decision_id returned by get_next_activity; omission explicitly creates an unbound standalone attempt"`
	OutcomeIDs      []string `json:"outcome_ids,omitempty" jsonschema:"stable outcome IDs from the decision competency; required when bound competency defines outcomes"`
	ActivityType    string   `json:"activity_type" jsonschema:"evidence-bearing activity type"`
	Observable      string   `json:"observable" jsonschema:"observable learning outcome assessed by this task"`
	TaskText        string   `json:"task_text,omitempty" jsonschema:"exact learner-facing task; task_content_hash may be used instead for sensitive artefacts"`
	TaskContentHash string   `json:"task_content_hash,omitempty" jsonschema:"optional lowercase SHA-256 hex digest of the exact task"`
	RubricJSON      string   `json:"rubric_json" jsonschema:"rubric fixed before the response, including criteria and passing_score"`
}

type SubmitAssessmentAttemptParams struct {
	IdempotentMutationParams
	AttemptID           string `json:"attempt_id" jsonschema:"prepared assessment attempt ID"`
	LearnerResponse     string `json:"learner_response,omitempty" jsonschema:"exact learner response; response_content_hash may be used instead for sensitive or external artefacts"`
	ResponseContentHash string `json:"response_content_hash,omitempty" jsonschema:"optional lowercase SHA-256 hex digest of the exact learner response"`
}

type CancelAssessmentAttemptParams struct {
	IdempotentMutationParams
	AttemptID string `json:"attempt_id" jsonschema:"prepared or submitted attempt ID"`
}

func registerPrepareAssessmentAttempt(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "prepare_assessment_attempt",
		Description: "Freeze an assessment task and passing rubric before showing it to the learner. Returns attempt_id; submit_assessment_attempt must be called only after the learner has committed a response.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params PrepareAssessmentAttemptParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "prepare_assessment_attempt", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		for _, field := range []struct {
			name, value string
			max         int
		}{
			{"domain_id", params.DomainID, maxShortLabelLen},
			{"concept", params.Concept, maxShortLabelLen},
			{"session_id", params.SessionID, maxShortLabelLen},
			{"activity_id", params.ActivityID, maxShortLabelLen},
			{"decision_id", params.DecisionID, maxShortLabelLen},
			{"activity_type", params.ActivityType, maxShortLabelLen},
			{"observable", params.Observable, maxNoteLen},
			{"task_text", params.TaskText, maxLongTextLen},
			{"task_content_hash", params.TaskContentHash, 64},
		} {
			if err := validateString(field.name, field.value, field.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		if strings.TrimSpace(params.Concept) == "" || strings.TrimSpace(params.Observable) == "" {
			r, _ := errorResult("concept and observable are required")
			return r, nil, nil
		}
		if err := validateEnum("activity_type", params.ActivityType, allowedActivityTypes); err != nil || !isCognitiveEvidenceActivity(params.ActivityType) {
			if err == nil {
				err = fmt.Errorf("activity_type must be evidence-bearing")
			}
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if strings.TrimSpace(params.TaskText) == "" && strings.TrimSpace(params.TaskContentHash) == "" {
			r, _ := errorResult("task_text or task_content_hash is required")
			return r, nil, nil
		}
		taskHash, err := canonicalContentHash(params.TaskText, params.TaskContentHash)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		var rubric map[string]any
		var warnings []string
		if params.DecisionID != "" {
			var strict models.AssessmentRubric
			strict, err = normalizeAssessmentRubric(params.RubricJSON)
			if err == nil {
				rubric, warnings = assessmentRubricMap(strict), nil
			}
		} else {
			rubric, warnings, err = normalizeRubricJSON(params.RubricJSON)
		}
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if rubric == nil {
			r, _ := errorResult("rubric_json is required")
			return r, nil, nil
		}
		passingScore, _, ok := rubricFiniteNumber(rubric["passing_score"])
		_, maxTotal := rubricMaxScores(rubric)
		if !ok || passingScore <= 0 || maxTotal <= 0 || passingScore > maxTotal {
			r, _ := errorResult("rubric_json must contain a positive passing_score no greater than the criteria maximum")
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			r, payload := noActiveDomainResult()
			return r, payload, nil
		}
		if err := validateConceptInDomain(domain, params.Concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		now := time.Now().UTC()
		learningSession, err := resolveOpenLearningSession(ctx, deps, learnerID, domain.ID, params.SessionID, now)
		if err != nil {
			return learningSessionResolutionErrorResult(deps, err), nil, nil
		}

		version := params.ActivityVersion
		if version == 0 {
			version = 1
		}
		if version < 1 || version > 1_000_000 {
			r, _ := errorResult("activity_version must be between 1 and 1000000")
			return r, nil, nil
		}
		rubricJSON := mustSnapshotJSON(rubric)
		activityID := strings.TrimSpace(params.ActivityID)
		if activityID == "" {
			activityID = deriveActivityID(domain.ID, params.Concept, params.ActivityType, taskHash, rubricJSON)
		}
		attemptID, err := generateAssessmentID()
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to create assessment identifier", err)
			return r, nil, nil
		}
		attempt := &models.AssessmentAttempt{
			ID:              attemptID,
			LearnerID:       learnerID,
			DomainID:        domain.ID,
			ConceptID:       params.Concept,
			SessionID:       learningSession.ID,
			ActivityID:      activityID,
			ActivityVersion: version,
			DecisionID:      params.DecisionID,
			ActivityType:    params.ActivityType,
			Observable:      strings.TrimSpace(params.Observable),
			TaskText:        params.TaskText,
			TaskContentHash: taskHash,
			RubricJSON:      rubricJSON,
			PassingScore:    passingScore,
			Status:          models.AssessmentAttemptPrepared,
			CreatedAt:       now,
		}
		if err := bindAssessmentCurriculum(ctx, deps, attempt, domain, params.OutcomeIDs); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		bindingStatus := "decision_bound"
		if attempt.DecisionID == "" {
			bindingStatus = "standalone_unbound"
			warnings = append(warnings, "No decision_id: this attempt does not attest compliance with a runtime decision.")
		}
		if err := deps.Store.CreateAssessmentAttempt(ctx, attempt); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to prepare assessment attempt", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{
			"attempt_id":         attempt.ID,
			"status":             attempt.Status,
			"domain_id":          attempt.DomainID,
			"concept":            attempt.ConceptID,
			"session_id":         attempt.SessionID,
			"activity_id":        attempt.ActivityID,
			"activity_version":   attempt.ActivityVersion,
			"decision_id":        attempt.DecisionID,
			"binding_status":     bindingStatus,
			"curriculum_version": attempt.CurriculumVersion,
			"outcome_ids":        params.OutcomeIDs,
			"task_content_hash":  attempt.TaskContentHash,
			"rubric":             rubric,
			"rubric_warnings":    warnings,
			"passing_score":      attempt.PassingScore,
		})
		return r, nil, nil
	})
}

func registerSubmitAssessmentAttempt(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "submit_assessment_attempt",
		Description: "Persist the learner's committed response (or its integrity hash) against a previously frozen assessment attempt. The task and rubric cannot change at this stage.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params SubmitAssessmentAttemptParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "submit_assessment_attempt", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("attempt_id", params.AttemptID, maxShortLabelLen); err != nil || strings.TrimSpace(params.AttemptID) == "" {
			if err == nil {
				err = fmt.Errorf("attempt_id is required")
			}
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("learner_response", params.LearnerResponse, maxLongTextLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		responseHash, err := canonicalContentHash(params.LearnerResponse, params.ResponseContentHash)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := deps.Store.SubmitAssessmentAttempt(ctx, learnerID, params.AttemptID, params.LearnerResponse, responseHash, time.Now().UTC()); err != nil {
			r, _ := safeErrorResult(deps.Logger, "assessment attempt cannot be submitted", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{
			"attempt_id":            params.AttemptID,
			"status":                models.AssessmentAttemptSubmitted,
			"response_content_hash": responseHash,
			"next_action":           "evaluate the frozen rubric, then call record_interaction with this attempt_id",
		})
		return r, nil, nil
	})
}

func registerCancelAssessmentAttempt(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "cancel_assessment_attempt",
		Description: "Cancel a prepared or submitted assessment attempt without changing the learner model or creating evidence.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params CancelAssessmentAttemptParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "cancel_assessment_attempt", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("attempt_id", params.AttemptID, maxShortLabelLen); err != nil || strings.TrimSpace(params.AttemptID) == "" {
			if err == nil {
				err = fmt.Errorf("attempt_id is required")
			}
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := deps.Store.CancelAssessmentAttempt(ctx, learnerID, params.AttemptID, time.Now().UTC()); err != nil {
			r, _ := safeErrorResult(deps.Logger, "assessment attempt cannot be cancelled", err)
			return r, nil, nil
		}
		r, _ := jsonResult(map[string]any{"attempt_id": params.AttemptID, "status": models.AssessmentAttemptCancelled})
		return r, nil, nil
	})
}

func canonicalContentHash(content, supplied string) (string, error) {
	supplied = strings.ToLower(strings.TrimSpace(supplied))
	if supplied != "" {
		decoded, err := hex.DecodeString(supplied)
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("content hash must be a 64-character lowercase SHA-256 hex digest")
		}
	}
	if content == "" {
		if supplied == "" {
			return "", fmt.Errorf("content or content hash is required")
		}
		return supplied, nil
	}
	sum := sha256.Sum256([]byte(content))
	computed := hex.EncodeToString(sum[:])
	if supplied != "" && supplied != computed {
		return "", fmt.Errorf("supplied content hash does not match content")
	}
	return computed, nil
}

func deriveActivityID(domainID, concept, activityType, taskHash, rubricJSON string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{domainID, concept, activityType, taskHash, rubricJSON}, "\x00")))
	return "act_" + hex.EncodeToString(sum[:12])
}

func generateAssessmentID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate assessment id: %w", err)
	}
	return "att_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// deriveAssessmentOutcome is deliberately stricter than the compatibility
// rubric normalizer: every frozen criterion must be scored exactly once and no
// score may exceed its declared maximum. The pass/fail outcome is then derived
// from the frozen passing_score, never accepted as an independent host field.
func deriveAssessmentOutcome(rubric, score map[string]any) (float64, bool, error) {
	maxByID, _ := rubricMaxScores(rubric)
	criterionIDs := make([]string, 0, len(maxByID))
	for id := range maxByID {
		criterionIDs = append(criterionIDs, id)
	}
	sort.Strings(criterionIDs)
	maxTotal := 0.0
	for _, id := range criterionIDs {
		maxTotal += maxByID[id]
	}
	passing, _, ok := rubricFiniteNumber(rubric["passing_score"])
	if !ok || passing <= 0 || maxTotal <= 0 || passing > maxTotal {
		return 0, false, fmt.Errorf("assessment rubric has an invalid passing rule")
	}
	rawScores, ok := score["criteria_scores"]
	if !ok {
		return 0, false, fmt.Errorf("assessment score must contain criteria_scores")
	}
	scoresByID := make(map[string]float64, len(maxByID))
	add := func(item map[string]any) error {
		id, _ := item["id"].(string)
		id, ok = normalizeRubricID(id)
		if !ok {
			return fmt.Errorf("assessment score contains an invalid criterion id")
		}
		max, exists := maxByID[id]
		if !exists {
			return fmt.Errorf("assessment score contains unknown criterion %q", id)
		}
		if _, duplicate := scoresByID[id]; duplicate {
			return fmt.Errorf("assessment score contains duplicate criterion %q", id)
		}
		value, _, valid := rubricFiniteNumber(item["score"])
		if !valid || value < 0 || value > max {
			return fmt.Errorf("assessment score for %q must be between 0 and %v", id, max)
		}
		scoresByID[id] = value
		return nil
	}
	switch items := rawScores.(type) {
	case []map[string]any:
		for _, item := range items {
			if err := add(item); err != nil {
				return 0, false, err
			}
		}
	case []any:
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return 0, false, fmt.Errorf("assessment criteria_scores must contain objects")
			}
			if err := add(item); err != nil {
				return 0, false, err
			}
		}
	default:
		return 0, false, fmt.Errorf("assessment criteria_scores must be an array")
	}
	if len(scoresByID) != len(maxByID) {
		return 0, false, fmt.Errorf("assessment score must cover every frozen rubric criterion")
	}
	total := 0.0
	for _, id := range criterionIDs {
		total += scoresByID[id]
	}
	return total, total >= passing || rubricFloatEqual(total, passing), nil
}

// normalizeAssessmentRubric is deliberately separate from the permissive
// legacy normalizer. A bound assessment must preserve every declared scoring
// rule or reject the rubric before the learner sees the task.
func normalizeAssessmentRubric(raw string) (models.AssessmentRubric, error) {
	var rubric models.AssessmentRubric
	parsed, err := parseRubricSchemaJSON("rubric_json", raw)
	if err != nil {
		return rubric, err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return rubric, fmt.Errorf("bound assessment rubric_json must be a canonical JSON object")
	}
	if err := rejectUnknownAssessmentRubricFields(object, "rubric_json", "criteria", "passing_score", "answer_key"); err != nil {
		return rubric, err
	}
	passing, coerced, valid := rubricFiniteNumber(object["passing_score"])
	if !valid || coerced {
		return rubric, fmt.Errorf("rubric_json.passing_score must be a finite JSON number")
	}
	rubric.PassingScore = passing
	if value, exists := object["answer_key"]; exists {
		answerKey, ok := value.(string)
		if !ok || strings.TrimSpace(answerKey) == "" {
			return rubric, fmt.Errorf("rubric_json.answer_key must be a non-empty string when present")
		}
		rubric.AnswerKey = answerKey
	}
	criteria, ok := object["criteria"].([]any)
	if !ok {
		return rubric, fmt.Errorf("rubric_json.criteria must be an array of defined criteria")
	}
	for i, value := range criteria {
		criterionObject, ok := value.(map[string]any)
		if !ok {
			return rubric, fmt.Errorf("rubric_json.criteria[%d] must be an object", i)
		}
		field := fmt.Sprintf("rubric_json.criteria[%d]", i)
		if err := rejectUnknownAssessmentRubricFields(criterionObject, field, "id", "description", "max_score", "anchors"); err != nil {
			return rubric, err
		}
		criterion := models.AssessmentRubricCriterion{}
		criterion.ID, _ = criterionObject["id"].(string)
		criterion.Description, _ = criterionObject["description"].(string)
		maxScore, coerced, valid := rubricFiniteNumber(criterionObject["max_score"])
		if !valid || coerced {
			return rubric, fmt.Errorf("%s.max_score must be a finite JSON number", field)
		}
		criterion.MaxScore = maxScore
		if value, exists := criterionObject["anchors"]; exists {
			anchors, ok := value.([]any)
			if !ok {
				return rubric, fmt.Errorf("%s.anchors must be an array", field)
			}
			for j, value := range anchors {
				anchorObject, ok := value.(map[string]any)
				if !ok {
					return rubric, fmt.Errorf("%s.anchors[%d] must be an object", field, j)
				}
				anchorField := fmt.Sprintf("%s.anchors[%d]", field, j)
				if err := rejectUnknownAssessmentRubricFields(anchorObject, anchorField, "score", "description"); err != nil {
					return rubric, err
				}
				score, coerced, valid := rubricFiniteNumber(anchorObject["score"])
				if !valid || coerced {
					return rubric, fmt.Errorf("%s.score must be a finite JSON number", anchorField)
				}
				description, _ := anchorObject["description"].(string)
				criterion.Anchors = append(criterion.Anchors, models.AssessmentRubricAnchor{Score: score, Description: description})
			}
		}
		rubric.Criteria = append(rubric.Criteria, criterion)
	}
	return rubric, validateBoundAssessmentRubric(rubric)
}

func rejectUnknownAssessmentRubricFields(object map[string]any, field string, allowed ...string) error {
	for _, key := range sortedRubricKeys(object) {
		known := false
		for _, candidate := range allowed {
			if key == candidate {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%s contains unsupported field %q; supported fields: %s", field, key, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func validateBoundAssessmentRubric(rubric models.AssessmentRubric) error {
	if len(rubric.Criteria) == 0 {
		return fmt.Errorf("bound assessment rubric must contain at least one defined criterion")
	}
	maxByID := make(map[string]float64, len(rubric.Criteria))
	criterionIDs := make([]string, 0, len(rubric.Criteria))
	for i, criterion := range rubric.Criteria {
		id, ok := normalizeRubricID(criterion.ID)
		_, duplicate := maxByID[id]
		if !ok || id != criterion.ID || duplicate {
			return fmt.Errorf("rubric_json.criteria[%d].id must be a unique canonical criterion ID", i)
		}
		if strings.TrimSpace(criterion.Description) == "" {
			return fmt.Errorf("rubric_json.criteria[%d].description must define the observable performance being scored", i)
		}
		if !rubricFinite(criterion.MaxScore) || criterion.MaxScore <= 0 {
			return fmt.Errorf("rubric_json.criteria[%d].max_score must be positive and finite", i)
		}
		maxByID[id] = criterion.MaxScore
		criterionIDs = append(criterionIDs, id)
		anchorScores := make(map[float64]bool, len(criterion.Anchors))
		for j, anchor := range criterion.Anchors {
			if !rubricFinite(anchor.Score) || anchor.Score < 0 || anchor.Score > criterion.MaxScore || anchorScores[anchor.Score] {
				return fmt.Errorf("rubric_json.criteria[%d].anchors[%d].score must be unique and between zero and max_score", i, j)
			}
			anchorScores[anchor.Score] = true
			if strings.TrimSpace(anchor.Description) == "" {
				return fmt.Errorf("rubric_json.criteria[%d].anchors[%d].description must define observable performance", i, j)
			}
		}
	}
	sort.Strings(criterionIDs)
	maxTotal := 0.0
	for _, id := range criterionIDs {
		maxTotal += maxByID[id]
	}
	if !rubricFinite(maxTotal) || !rubricFinite(rubric.PassingScore) || rubric.PassingScore <= 0 || rubric.PassingScore > maxTotal {
		return fmt.Errorf("bound assessment passing_score must be positive and no greater than the finite criteria maximum")
	}
	return nil
}

// assessmentRubricMap bridges the canonical envelope to existing score and
// persistence helpers without dropping its generated answer key or anchors.
func assessmentRubricMap(rubric models.AssessmentRubric) map[string]any {
	criteria := make([]map[string]any, 0, len(rubric.Criteria))
	for _, criterion := range rubric.Criteria {
		item := map[string]any{"id": criterion.ID, "description": criterion.Description, "max_score": criterion.MaxScore}
		if len(criterion.Anchors) > 0 {
			anchors := make([]map[string]any, 0, len(criterion.Anchors))
			for _, anchor := range criterion.Anchors {
				anchors = append(anchors, map[string]any{"score": anchor.Score, "description": anchor.Description})
			}
			item["anchors"] = anchors
		}
		criteria = append(criteria, item)
	}
	out := map[string]any{"criteria": criteria, "passing_score": rubric.PassingScore}
	if rubric.AnswerKey != "" {
		out["answer_key"] = rubric.AnswerKey
	}
	return out
}

// deriveBoundAssessmentOutcome requires an observation of learner performance
// for every criterion. The server verifies completeness and arithmetic; it does
// not claim to independently validate the host's semantic judgment.
func deriveBoundAssessmentOutcome(rubric models.AssessmentRubric, score map[string]any) (float64, bool, error) {
	if err := validateBoundAssessmentRubric(rubric); err != nil {
		return 0, false, err
	}
	var items []map[string]any
	switch values := score["criteria_scores"].(type) {
	case []map[string]any:
		items = values
	case []any:
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return 0, false, fmt.Errorf("assessment criteria_scores must contain objects")
			}
			items = append(items, item)
		}
	default:
		return 0, false, fmt.Errorf("assessment criteria_scores must be an array")
	}
	for _, item := range items {
		evidence, _ := item["evidence"].(string)
		if strings.TrimSpace(evidence) == "" {
			return 0, false, fmt.Errorf("bound assessment score for criterion %q requires non-empty evidence from the learner response", item["id"])
		}
	}
	return deriveAssessmentOutcome(assessmentRubricMap(rubric), score)
}
