// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"fmt"
	"math"
	"strings"

	"tutor-mcp/models"
)

// validateConceptInDomain checks that concept is a member of d.Graph.Concepts.
// It is the shared write-side guard for record_interaction and any other tool
// that mutates a learner's cognitive state on a per-concept basis. The error
// string names the unknown concept and the active domain so the LLM can
// self-correct (call get_learner_context to refresh the concept list).
func validateConceptInDomain(d *models.Domain, concept string) error {
	if d == nil {
		return fmt.Errorf("no active domain - call init_domain first")
	}
	for _, c := range d.Graph.Concepts {
		if c == concept {
			return nil
		}
	}
	return fmt.Errorf(
		"concept %q is not part of domain %q (call get_learner_context to see the concept list)",
		concept, d.Name,
	)
}

// normalizeConceptParam canonicalizes concept-targeting tool params.
// `concept` is the public key; `concept_id` is kept as a temporary
// compatibility alias for older callers (issue #86).
func normalizeConceptParam(concept, legacyConceptID string) (string, error) {
	if err := validateString("concept", concept, maxShortLabelLen); err != nil {
		return "", err
	}
	if err := validateString("concept_id", legacyConceptID, maxShortLabelLen); err != nil {
		return "", err
	}
	if concept != "" && legacyConceptID != "" && concept != legacyConceptID {
		return "", fmt.Errorf("concept and concept_id must match when both are provided")
	}
	if concept != "" {
		return concept, nil
	}
	return legacyConceptID, nil
}

// validateUnitInterval rejects NaN/Inf and any value outside [0, 1].
// Used for probabilities & confidence-style scores fed into BKT/FSRS/IRT
// chains where silent clamping has historically corrupted estimator state
// (issue #25). The error names the offending field so the calling LLM
// can self-correct.
func validateUnitInterval(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number in [0, 1] (got non-finite value)", field)
	}
	if v < 0 || v > 1 {
		return fmt.Errorf("%s must be in [0, 1] (got %v)", field, v)
	}
	return nil
}

// validateLikertInt rejects integer Likert ratings outside [min, max].
// Use min=1, max=4 for the affect scale and min=1, max=5 for the
// calibration self-assessment scale. A value of 0 is treated as "not
// provided" by the upstream omitempty tag and must be allowed through.
func validateLikertInt(field string, v, min, max int) error {
	if v == 0 {
		return nil // omitted / not provided
	}
	if v < min || v > max {
		return fmt.Errorf("%s must be in [%d, %d] (got %d)", field, min, max, v)
	}
	return nil
}

// validateLikertFloat rejects NaN/Inf and float Likert ratings outside
// [min, max]. Used by calibration_check whose PredictedMastery field is
// a float64 1-5 scale.
func validateLikertFloat(field string, v, min, max float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number in [%v, %v] (got non-finite value)", field, min, max)
	}
	if v < min || v > max {
		return fmt.Errorf("%s must be in [%v, %v] (got %v)", field, min, max, v)
	}
	return nil
}

// validateBoundedFinite rejects NaN/Inf and any value outside [min, max].
// Used for signed scores like calibration_bias whose legitimate range
// straddles zero ([-1, 1]) — validateUnitInterval would wrongly reject
// negative values, and validateLikertFloat is for ordinal Likert scales.
// Issue #85: chat-side LLM was free to push +Inf into profile_json, which
// marshals to the literal `+Inf` and breaks every downstream consumer.
func validateBoundedFinite(field string, v, min, max float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number in [%v, %v] (got non-finite value)", field, min, max)
	}
	if v < min || v > max {
		return fmt.Errorf("%s must be in [%v, %v] (got %v)", field, min, max, v)
	}
	return nil
}

// validateNonNegativeDuration rejects NaN/Inf, negative values, and
// values exceeding maxSeconds. Used for response_time_seconds where
// negative or absurdly large values were silently feeding the FSRS
// stability/difficulty update.
func validateNonNegativeDuration(field string, v float64, maxSeconds float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number in [0, %v] seconds (got non-finite value)", field, maxSeconds)
	}
	if v < 0 || v > maxSeconds {
		return fmt.Errorf("%s must be in [0, %v] seconds (got %v)", field, maxSeconds, v)
	}
	return nil
}

// validateNonNegativeCount rejects negative integer counts and values
// exceeding max. Used for hints_requested.
func validateNonNegativeCount(field string, v, max int) error {
	if v < 0 || v > max {
		return fmt.Errorf("%s must be in [0, %d] (got %d)", field, max, v)
	}
	return nil
}

// String length budgets for free-text params persisted to SQLite (issue
// #31). Without these caps a misbehaving LLM (or an authenticated
// learner) can post multi-MB strings, exhausting host storage and
// poisoning downstream telemetry. The values err on the side of
// generosity: legitimate concept names and notes are well under these.
const (
	maxShortLabelLen = 200    // concept names, error_type, activity_type, session_id, kind labels
	maxNoteLen       = 4096   // notes, misconception_detail, intent prompts
	maxLongTextLen   = 16_384 // free-form payloads (rationale, content) - last-resort cap
)

// validateString rejects strings longer than max bytes. Empty values are
// allowed — combine with a separate "required" check if non-empty is needed.
// The cap is byte-counted (len) on purpose: SQLite TEXT pays per byte and a
// pathological multibyte string still has to fit.
func validateString(field, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("%s exceeds max length: got %d bytes, max %d", field, len(value), max)
	}
	return nil
}

// validateEnum rejects values that are not in the allowed set. The error
// names the field, the offending value, and the full accepted vocabulary
// so the calling LLM can self-correct without a round-trip to the docs.
// Used for record_interaction.activity_type and error_type (issue #88) where
// a free-form string forces the LLM to guess from prose hints in the schema
// description, leading to silent persistence of typos like "RECALL" instead
// of "RECALL_EXERCISE" — those rows then escape downstream filters that key
// off the canonical vocabulary.
func validateEnum(field, value string, allowed []string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of: %s (got %q)", field, strings.Join(allowed, ", "), value)
}

// allowedActivityTypes is the evidence-bearing subset accepted by
// record_interaction. REST, SETUP_DOMAIN and CLOSE_SESSION are orchestration
// events, not learner responses; accepting them here would incorrectly advance
// BKT, FSRS and IRT.
var allowedActivityTypes = []string{
	string(models.ActivityRecall),           // RECALL_EXERCISE
	string(models.ActivityNewConcept),       // NEW_CONCEPT
	string(models.ActivityMasteryChallenge), // MASTERY_CHALLENGE
	string(models.ActivityDebuggingCase),    // DEBUGGING_CASE
	string(models.ActivityPractice),         // PRACTICE
	string(models.ActivityDebugMisconception),
	string(models.ActivityFeynmanPrompt), // FEYNMAN_PROMPT
	string(models.ActivityTransferProbe), // TRANSFER_PROBE
}

// allowedErrorTypes is the vocabulary the BKT heuristic
// (algorithms.BKTUpdateHeuristicSlipByErrorType) branches on. Anything else
// is silently treated as standard BKT, so accepting arbitrary labels causes
// audit-trail noise and breaks alert.go's errorTypeCounts aggregation.
var allowedErrorTypes = []string{
	"SYNTAX_ERROR",
	"LOGIC_ERROR",
	"KNOWLEDGE_GAP",
}
