// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package assessment validates frozen generated rubrics and criterion scores.
// It verifies structure and arithmetic, never evaluator identity or semantic truth.
package assessment

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"tutor-mcp/models"
)

// ParseRubric is deliberately separate from the permissive
// legacy normalizer. A bound assessment must preserve every declared scoring
// rule or reject the rubric before the learner sees the task.
func ParseRubric(raw string) (models.AssessmentRubric, error) {
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
	if err := ValidateRubric(rubric); err != nil {
		return rubric, err
	}
	encoded, err := json.Marshal(RubricMap(rubric))
	if err != nil || len(encoded) > MaxJSONBytes {
		return rubric, fmt.Errorf("canonical rubric_json exceeds %d bytes or contains invalid data", MaxJSONBytes)
	}
	return rubric, nil
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

func ValidateRubric(rubric models.AssessmentRubric) error {
	if len(rubric.Criteria) == 0 {
		return fmt.Errorf("bound assessment rubric must contain at least one defined criterion")
	}
	maxByID := make(map[string]float64, len(rubric.Criteria))
	maxTotal := new(big.Rat)
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
		maxTotal.Add(maxTotal, decimal(criterion.MaxScore))
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
	maxValue, _ := maxTotal.Float64()
	if !rubricFinite(maxValue) || !rubricFinite(rubric.PassingScore) || rubric.PassingScore <= 0 || decimal(rubric.PassingScore).Cmp(maxTotal) > 0 {
		return fmt.Errorf("bound assessment passing_score must be positive and no greater than the finite criteria maximum")
	}
	return nil
}

// RubricMap bridges the canonical envelope to existing score and
// persistence helpers without dropping its generated answer key or anchors.
func RubricMap(rubric models.AssessmentRubric) map[string]any {
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
