// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"tutor-mcp/models"
)

// Result contains mechanically checked scoring, not a claim of evaluator trust.
type Result struct {
	Score  map[string]any
	Total  float64
	Passed bool
}

func EvaluateJSON(rubric models.AssessmentRubric, raw string) (Result, error) {
	parsed, err := parseRubricSchemaJSON("rubric_score_json", raw)
	if err != nil {
		return Result{}, err
	}
	score, ok := parsed.(map[string]any)
	if !ok {
		return Result{}, fmt.Errorf("bound assessment rubric_score_json must be a canonical JSON object")
	}
	return Evaluate(rubric, score)
}

// Evaluate requires all and only frozen criterion IDs, each exactly once.
// Optional aggregate fields are checked, never used to override the rubric.
// Evidence is required prose, not a semantic entailment check of the response.
func Evaluate(rubric models.AssessmentRubric, score map[string]any) (Result, error) {
	if err := ValidateRubric(rubric); err != nil {
		return Result{}, err
	}
	if err := rejectUnknownAssessmentRubricFields(score, "rubric_score_json", "criteria_scores", "total", "max_total", "summary", "confidence"); err != nil {
		return Result{}, err
	}
	var items []map[string]any
	switch values := score["criteria_scores"].(type) {
	case []map[string]any:
		items = values
	case []any:
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return Result{}, fmt.Errorf("assessment criteria_scores must contain objects")
			}
			items = append(items, item)
		}
	default:
		return Result{}, fmt.Errorf("assessment criteria_scores must be an array")
	}
	maxByID := make(map[string]float64, len(rubric.Criteria))
	for _, criterion := range rubric.Criteria {
		maxByID[criterion.ID] = criterion.MaxScore
	}
	byID := make(map[string]map[string]any, len(items))
	total, maximum := new(big.Rat), new(big.Rat)
	for _, item := range items {
		if err := rejectUnknownAssessmentRubricFields(item, "criteria_scores item", "id", "score", "evidence", "error_type", "max_score"); err != nil {
			return Result{}, err
		}
		id, _ := item["id"].(string)
		max, exists := maxByID[id]
		if !exists {
			return Result{}, fmt.Errorf("assessment score contains unknown criterion %q", id)
		}
		if _, duplicate := byID[id]; duplicate {
			return Result{}, fmt.Errorf("assessment score contains duplicate criterion %q", id)
		}
		value, _, valid := rubricFiniteNumber(item["score"])
		if !valid || value < 0 || value > max {
			return Result{}, fmt.Errorf("assessment score for %q must be a finite JSON number between 0 and %v", id, max)
		}
		evidence, ok := item["evidence"].(string)
		if !ok || strings.TrimSpace(evidence) == "" {
			return Result{}, fmt.Errorf("bound assessment score for criterion %q requires non-empty evidence from the learner response", id)
		}
		normalized := map[string]any{"id": id, "score": value, "evidence": evidence}
		if supplied, present := item["max_score"]; present {
			maximum, _, valid := rubricFiniteNumber(supplied)
			if !valid || maximum != max {
				return Result{}, fmt.Errorf("criterion %q max_score contradicts the frozen rubric", id)
			}
			normalized["max_score"] = maximum
		}
		if supplied, present := item["error_type"]; present {
			errorType, ok := supplied.(string)
			if !ok || (errorType != "" && errorType != "SYNTAX_ERROR" && errorType != "LOGIC_ERROR" && errorType != "KNOWLEDGE_GAP") {
				return Result{}, fmt.Errorf("criterion %q has an invalid error_type", id)
			}
			normalized["error_type"] = errorType
		}
		byID[id] = normalized
		total.Add(total, decimal(value))
		maximum.Add(maximum, decimal(max))
	}
	if len(byID) != len(rubric.Criteria) {
		return Result{}, fmt.Errorf("assessment score must cover every frozen rubric criterion")
	}
	totalValue, _ := total.Float64()
	maxValue, _ := maximum.Float64()
	if !rubricFinite(totalValue) || !rubricFinite(maxValue) {
		return Result{}, fmt.Errorf("assessment aggregate must be finite")
	}
	for _, aggregate := range []struct {
		key   string
		value float64
	}{{"total", totalValue}, {"max_total", maxValue}} {
		if supplied, present := score[aggregate.key]; present {
			value, _, valid := rubricFiniteNumber(supplied)
			if !valid || value != aggregate.value {
				return Result{}, fmt.Errorf("rubric_score_json.%s contradicts the frozen rubric and criterion scores", aggregate.key)
			}
		}
	}
	canonical := make([]map[string]any, 0, len(byID))
	// The frozen criterion order is the only presentation order in the result.
	for _, criterion := range rubric.Criteria {
		canonical = append(canonical, byID[criterion.ID])
	}
	out := map[string]any{"criteria_scores": canonical, "total": totalValue, "max_total": maxValue}
	if supplied, present := score["summary"]; present {
		summary, ok := supplied.(string)
		if !ok || strings.TrimSpace(summary) == "" {
			return Result{}, fmt.Errorf("rubric_score_json.summary must be a non-empty string when present")
		}
		out["summary"] = summary
	}
	if supplied, present := score["confidence"]; present {
		confidence, _, valid := rubricFiniteNumber(supplied)
		if !valid || confidence < 0 || confidence > 1 {
			return Result{}, fmt.Errorf("rubric_score_json.confidence must be a finite JSON number in [0, 1]")
		}
		out["confidence"] = confidence
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > MaxJSONBytes {
		return Result{}, fmt.Errorf("canonical rubric_score_json exceeds %d bytes or contains invalid data", MaxJSONBytes)
	}
	return Result{Score: out, Total: totalValue, Passed: total.Cmp(decimal(rubric.PassingScore)) >= 0}, nil
}

// Stored numbers are float64. Sum their canonical decimal representations
// exactly so score order and a fixed epsilon cannot turn a zero into a pass.
// This does not claim arbitrary precision for the original JSON numeric tokens.
func decimal(value float64) *big.Rat {
	result, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	if !ok {
		panic("assessment: decimal called with non-finite value")
	}
	return result
}
