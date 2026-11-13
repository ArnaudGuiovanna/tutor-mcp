// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
)

// rawJSONText returns an object argument as JSON text for the shared
// normalizers; an absent or null argument is "".
func rawJSONText(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return ""
	}
	return text
}

// publicRubricError names the MCP arguments (rubric, rubric_score) in errors
// produced by the shared assessment parser, which still says rubric_json.
func publicRubricError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.NewReplacer("rubric_score_json", "rubric_score", "rubric_json", "rubric").Replace(err.Error())
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}

// rubricInputSchema is the canonical rubric object frozen by
// prepare_assessment_attempt (requirePassing) or attached to an unverified
// observation. The server also checks unique IDs, score bounds and the
// passing threshold.
func rubricInputSchema(description string, requirePassing bool) *jsonschema.Schema {
	zero, one, positive := 0.0, 1, 0.0
	closed := func() *jsonschema.Schema { return &jsonschema.Schema{Not: &jsonschema.Schema{}} }
	text := func(description string) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", MinLength: &one, Description: description}
	}
	required := []string{"criteria"}
	if requirePassing {
		required = append(required, "passing_score")
	}
	return &jsonschema.Schema{
		Type: "object", Description: description, Required: required, AdditionalProperties: closed(),
		Properties: map[string]*jsonschema.Schema{
			"criteria": {Type: "array", MinItems: &one, Description: "Scored criteria; every id is unique.", Items: &jsonschema.Schema{
				Type: "object", Required: []string{"id", "max_score"}, AdditionalProperties: closed(),
				Properties: map[string]*jsonschema.Schema{
					"id":          {Type: "string", MinLength: &one, Description: "Unique, case-sensitive criterion identity containing letters, digits, underscores or hyphens (at least one letter or digit). Preserve curriculum criterion IDs exactly; never lowercase or replace their hyphens."},
					"description": text("What this criterion observes; required with decision_id."),
					"max_score":   {Type: "number", ExclusiveMinimum: &positive, Description: "Maximum score of this criterion."},
					"anchors": {Type: "array", Description: "Optional score anchors, never an object keyed by score.", Items: &jsonschema.Schema{
						Type: "object", Required: []string{"score", "description"}, AdditionalProperties: closed(),
						Properties: map[string]*jsonschema.Schema{
							"score":       {Type: "number", Minimum: &zero, Description: "Anchored score."},
							"description": text("Observable performance at this score."),
						},
					}},
				},
			}},
			"passing_score": {Type: "number", ExclusiveMinimum: &positive, Description: "Pass threshold, no greater than the sum of max_score."},
			"answer_key":    text("Optional expected answer, as a string."),
			"max_total":     {Type: "number", ExclusiveMinimum: &positive, Description: "Optional redundant sum of criteria max_score; must agree exactly."},
		},
	}
}

// rubricScoreInputSchema is the canonical scoring object of record_interaction.
func rubricScoreInputSchema(description string) *jsonschema.Schema {
	zero, one, minConfidence, maxConfidence := 0.0, 1, 0.0, 1.0
	return &jsonschema.Schema{
		Type: "object", Description: description, Required: []string{"criteria_scores"}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		Properties: map[string]*jsonschema.Schema{
			"criteria_scores": {Type: "array", MinItems: &one, Description: "Exactly one entry per frozen rubric criterion.", Items: &jsonschema.Schema{
				Type: "object", Required: []string{"id", "score"}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
				Properties: map[string]*jsonschema.Schema{
					"id":         {Type: "string", MinLength: &one, Description: "Exact frozen criterion id."},
					"score":      {Type: "number", Minimum: &zero, Description: "Finite numeric score, at most the criterion max_score."},
					"max_score":  {Type: "number", Minimum: &zero, Description: "Optional copy of the criterion max_score; must agree."},
					"evidence":   {Type: "string", Description: "Concrete observation from the learner's actual answer; required for decision-bound attempts."},
					"error_type": {Type: "string", Description: "Optional error class: SYNTAX_ERROR, LOGIC_ERROR or KNOWLEDGE_GAP."},
				},
			}},
			"total":      {Type: "number", Minimum: &zero, Description: "Optional sum of scores; must agree with criteria_scores."},
			"max_total":  {Type: "number", Minimum: &zero, Description: "Optional sum of max_score; must agree with the rubric."},
			"summary":    {Type: "string", Description: "Short grading summary."},
			"confidence": {Type: "number", Minimum: &minConfidence, Maximum: &maxConfidence, Description: "Optional grader confidence from 0 to 1; not evaluator trust."},
		},
	}
}

// freeObjectSchema is a free-form JSON object argument.
func freeObjectSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Description: description}
}

// normalizeRubricJSON parses a standalone rubric (an observation or an
// attempt without decision_id) and returns a canonical map shaped as:
// {"criteria":[{"id","description?","max_score"}], "passing_score?"}.
// The input is the closed object advertised by rubricInputSchema; the SDK has
// already rejected every other shape, so only the canonical one is decoded.
// Criterion IDs are normalized (lowercase, underscores) and reported as a
// warning; the decision-bound path (assessment.ParseRubric) preserves them.
func normalizeRubricJSON(raw string) (map[string]any, []string, error) {
	parsed, err := parseRubricSchemaJSON("rubric", raw)
	if err != nil || parsed == nil {
		return nil, nil, err
	}
	return normalizeRubricValue(parsed)
}

// normalizeRubricScoreJSON parses rubric_score and returns a canonical map
// shaped as:
// {"criteria_scores":[{"id","score","evidence?","error_type?"}],
//
//	"total", "max_total", "summary?", "confidence?"}.
//
// normalizedRubric is optional. When provided, its criteria ids and max_score
// values are used to infer max_total and emit cross-schema warnings.
func normalizeRubricScoreJSON(raw string, normalizedRubric map[string]any) (map[string]any, []string, error) {
	parsed, err := parseRubricSchemaJSON("rubric_score", raw)
	if err != nil || parsed == nil {
		return nil, nil, err
	}
	return normalizeRubricScoreValue(parsed, normalizedRubric)
}

// normalizeRubricValue normalizes an already-decoded rubric value.
func normalizeRubricValue(value any) (map[string]any, []string, error) {
	var warnings []string
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("rubric must be an object with a criteria array")
	}
	items, ok := object["criteria"].([]any)
	if !ok || len(items) == 0 {
		return nil, nil, fmt.Errorf("rubric.criteria must be a non-empty array of {id, max_score} objects")
	}
	criteria := make([]map[string]any, 0, len(items))
	seen := make(map[string]bool, len(items))
	maxTotal := 0.0
	for i, item := range items {
		criterion, ok := item.(map[string]any)
		if !ok {
			return nil, warnings, fmt.Errorf("rubric.criteria[%d] must be an object", i)
		}
		rawID, _ := criterion["id"].(string)
		id, err := normalizedRubricID(rawID, fmt.Sprintf("rubric.criteria[%d].id", i), &warnings)
		if err != nil {
			return nil, warnings, err
		}
		if seen[id] {
			return nil, warnings, fmt.Errorf("rubric.criteria contains duplicate id %q", id)
		}
		seen[id] = true
		maxScore, ok := rubricFiniteNumber(criterion["max_score"])
		if !ok || maxScore <= 0 {
			return nil, warnings, fmt.Errorf("rubric.criteria[%d].max_score must be a positive number", i)
		}
		out := map[string]any{"id": id, "max_score": maxScore}
		if description, _ := criterion["description"].(string); strings.TrimSpace(description) != "" {
			out["description"] = strings.TrimSpace(description)
		}
		criteria = append(criteria, out)
		maxTotal += maxScore
	}
	if supplied, present := object["max_total"]; present {
		value, ok := rubricFiniteNumber(supplied)
		if !ok || !rubricFloatEqual(value, maxTotal) {
			return nil, warnings, fmt.Errorf("rubric.max_total contradicts the criteria, which total %v", maxTotal)
		}
	}
	out := map[string]any{"criteria": criteria}
	if supplied, present := object["passing_score"]; present {
		passing, ok := rubricFiniteNumber(supplied)
		if !ok || passing <= 0 {
			return nil, warnings, fmt.Errorf("rubric.passing_score must be a positive number")
		}
		if passing > maxTotal && !rubricFloatEqual(passing, maxTotal) {
			warnings = append(warnings, "rubric.passing_score exceeds criteria max_total")
		}
		out["passing_score"] = passing
	}
	return out, warnings, nil
}

// normalizeRubricScoreValue normalizes an already-decoded rubric_score value.
// Cross-schema inconsistencies against normalizedRubric are warnings, not hard
// failures: a standalone observation has no frozen rubric to enforce.
func normalizeRubricScoreValue(value any, normalizedRubric map[string]any) (map[string]any, []string, error) {
	var warnings []string
	rubricMaxByID, rubricMaxTotal := rubricMaxScores(normalizedRubric)
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("rubric_score must be an object with a criteria_scores array")
	}
	items, ok := object["criteria_scores"].([]any)
	if !ok || len(items) == 0 {
		return nil, nil, fmt.Errorf("rubric_score.criteria_scores must be a non-empty array of {id, score} objects")
	}

	outScores := make([]map[string]any, 0, len(items))
	seen := make(map[string]bool, len(items))
	// inferredMaxTotal is the fallback without max_total or rubric: each
	// criterion's max_score, else 1 for a 0..1 score, else the score itself.
	total, inferredMaxTotal := 0.0, 0.0
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, warnings, fmt.Errorf("rubric_score.criteria_scores[%d] must be an object", i)
		}
		rawID, _ := entry["id"].(string)
		id, err := normalizedRubricID(rawID, fmt.Sprintf("rubric_score.criteria_scores[%d].id", i), &warnings)
		if err != nil {
			return nil, warnings, err
		}
		if seen[id] {
			return nil, warnings, fmt.Errorf("rubric_score.criteria_scores contains duplicate id %q", id)
		}
		seen[id] = true
		score, ok := rubricFiniteNumber(entry["score"])
		if !ok || score < 0 {
			return nil, warnings, fmt.Errorf("rubric_score.criteria_scores[%d].score must be a non-negative number", i)
		}
		total += score
		out := map[string]any{"id": id, "score": score}
		if evidence, _ := entry["evidence"].(string); strings.TrimSpace(evidence) != "" {
			out["evidence"] = strings.TrimSpace(evidence)
		}
		if errorType, _ := entry["error_type"].(string); strings.TrimSpace(errorType) != "" {
			errorType = strings.TrimSpace(errorType)
			out["error_type"] = errorType
			if err := validateEnum("error_type", errorType, allowedErrorTypes); err != nil {
				warnings = append(warnings, fmt.Sprintf("rubric_score.criteria_scores[%d].error_type is not canonical: %s", i, errorType))
			}
		}
		if supplied, present := entry["max_score"]; present {
			maxScore, ok := rubricFiniteNumber(supplied)
			if !ok || maxScore <= 0 {
				return nil, warnings, fmt.Errorf("rubric_score.criteria_scores[%d].max_score must be a positive number", i)
			}
			inferredMaxTotal += maxScore
		} else {
			inferredMaxTotal += math.Max(score, 1)
		}
		if max, ok := rubricMaxByID[id]; ok && score > max && !rubricFloatEqual(score, max) {
			warnings = append(warnings, fmt.Sprintf("rubric_score.criteria_scores[%s].score exceeds rubric max_score", id))
		}
		if _, ok := rubricMaxByID[id]; len(rubricMaxByID) > 0 && !ok {
			warnings = append(warnings, fmt.Sprintf("rubric_score.criteria_scores[%s].id is not present in rubric.criteria", id))
		}
		outScores = append(outScores, out)
	}
	for id := range rubricMaxByID {
		if !seen[id] {
			warnings = append(warnings, fmt.Sprintf("rubric_score.criteria_scores missing rubric criterion %q", id))
		}
	}

	if supplied, present := object["total"]; present {
		providedTotal, ok := rubricFiniteNumber(supplied)
		if !ok || providedTotal < 0 {
			return nil, warnings, fmt.Errorf("rubric_score.total must be a non-negative number")
		}
		if !rubricFloatEqual(providedTotal, total) {
			warnings = append(warnings, "rubric_score.total recomputed from criteria_scores")
		}
	}

	// The rubric's criteria are authoritative for max_total; without a rubric
	// the caller's max_total, then the inferred one, is used.
	var maxTotal float64
	switch supplied, present := object["max_total"]; {
	case present:
		value, ok := rubricFiniteNumber(supplied)
		if !ok || value <= 0 {
			return nil, warnings, fmt.Errorf("rubric_score.max_total must be a positive number")
		}
		maxTotal = value
		if rubricMaxTotal > 0 {
			if !rubricFloatEqual(value, rubricMaxTotal) {
				warnings = append(warnings, "rubric_score.max_total differs from rubric criteria max_total; using rubric")
			}
			maxTotal = rubricMaxTotal
		}
	case rubricMaxTotal > 0:
		maxTotal = rubricMaxTotal
		warnings = append(warnings, "rubric_score.max_total missing; inferred from rubric")
	default:
		maxTotal = inferredMaxTotal
		warnings = append(warnings, "rubric_score.max_total missing; inferred from criteria_scores")
	}
	if maxTotal < total && !rubricFloatEqual(maxTotal, total) {
		warnings = append(warnings, "rubric_score.total exceeds max_total")
	}

	out := map[string]any{
		"criteria_scores": outScores,
		"total":           total,
		"max_total":       maxTotal,
	}
	if summary, _ := object["summary"].(string); strings.TrimSpace(summary) != "" {
		out["summary"] = strings.TrimSpace(summary)
	}
	if supplied, present := object["confidence"]; present {
		confidence, ok := rubricFiniteNumber(supplied)
		if !ok || confidence < 0 || confidence > 1 {
			return nil, warnings, fmt.Errorf("rubric_score.confidence must be a number in [0, 1]")
		}
		out["confidence"] = confidence
	}
	sort.Strings(warnings)
	return out, warnings, nil
}

func parseRubricSchemaJSON(field, raw string) (any, error) {
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
	return parsed, nil
}

func rubricMaxScores(normalizedRubric map[string]any) (map[string]float64, float64) {
	out := map[string]float64{}
	if normalizedRubric == nil {
		return out, 0
	}
	criteriaValue, ok := normalizedRubric["criteria"]
	if !ok {
		return out, 0
	}

	add := func(item map[string]any) {
		rawID, _ := item["id"].(string)
		id, ok := normalizeRubricID(rawID)
		if !ok {
			return
		}
		maxScore, ok := rubricFiniteNumber(item["max_score"])
		if !ok || maxScore <= 0 {
			return
		}
		out[id] = maxScore
	}

	switch criteria := criteriaValue.(type) {
	case []map[string]any:
		for _, item := range criteria {
			add(item)
		}
	case []any:
		for _, raw := range criteria {
			if item, ok := raw.(map[string]any); ok {
				add(item)
			}
		}
	}

	total := 0.0
	for _, maxScore := range out {
		total += maxScore
	}
	return out, total
}

// rubricFiniteNumber reads a JSON number (decoded with UseNumber) or a value
// of an already-normalized map. Strings are not numbers: the schemas declare
// number types and the SDK has rejected them.
func rubricFiniteNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		return n, err == nil && rubricFinite(n)
	case float64:
		return v, rubricFinite(v)
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

func rubricFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func rubricFloatEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// normalizedRubricID returns the canonical criterion id, warning when the
// caller's spelling changed.
func normalizedRubricID(raw, field string, warnings *[]string) (string, error) {
	id, ok := normalizeRubricID(raw)
	if !ok {
		return "", fmt.Errorf("%s must be a non-empty string containing a letter or digit", field)
	}
	if strings.TrimSpace(raw) != id {
		*warnings = append(*warnings, fmt.Sprintf("%s normalized to %q", field, id))
	}
	return id, nil
}

func normalizeRubricID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var b strings.Builder
	lastSep := false
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			lastSep = false
			continue
		}
		if b.Len() > 0 && !lastSep {
			b.WriteByte('_')
			lastSep = true
		}
	}
	out := strings.Trim(b.String(), "_")
	return out, out != ""
}
