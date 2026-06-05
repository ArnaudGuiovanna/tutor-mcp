// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CalibrationCheckParams struct {
	Concept          string  `json:"concept,omitempty" jsonschema:"the concept to assess; canonical key for concept-targeting tools; required unless concept_id is used"`
	ConceptID        string  `json:"concept_id,omitempty" jsonschema:"deprecated compatibility alias for concept; prefer concept"`
	PredictedMastery float64 `json:"predicted_mastery" jsonschema:"learner self-assessment on a 1..5 Likert scale (1=no mastery, 5=perfect mastery); stored internally as 0..1"`
	DomainID         string  `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerCalibrationCheck(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "calibration_check",
		Description: "Record the learner's self-assessment on a concept before an exercise. Returns a prediction_id for post-exercise comparison.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params CalibrationCheckParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "calibration_check", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		concept, err := normalizeConceptParam(params.Concept, params.ConceptID)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if concept == "" {
			r, _ := errorResult("concept is required")
			return r, nil, nil
		}

		// String length cap (issue #82). concept is persisted into
		// calibration_records and echoed into prompt_text; domain_id is
		// resolved against the learner's domains. Without these guards a
		// misbehaving caller could push multi-MB strings into either path.
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// 1-5 Likert self-assessment. Reject NaN/Inf and out-of-range values
		// rather than silently clamping — clamping a hallucinated 100.0 to
		// 1.0 would corrupt the calibration record. See issue #25.
		if err := validateLikertFloat("predicted_mastery", params.PredictedMastery, 1, 5); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		predicted := (params.PredictedMastery - 1.0) / 4.0

		predictionID := generatePredictionID()
		record := &models.CalibrationRecord{
			PredictionID: predictionID,
			LearnerID:    learnerID,
			ConceptID:    concept,
			Predicted:    predicted,
		}

		if err := deps.Store.CreateCalibrationPrediction(ctx, record); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to create calibration", err)
			return r, nil, nil
		}

		promptText := fmt.Sprintf(
			"You estimated your mastery of '%s' at %.0f/5. Let's check that with an exercise.",
			concept, params.PredictedMastery,
		)

		r, _ := jsonResult(map[string]interface{}{
			"prediction_id": predictionID,
			"prompt_text":   promptText,
		})
		return r, nil, nil
	})
}

type RecordCalibrationResultParams struct {
	PredictionID string  `json:"prediction_id" jsonschema:"prediction ID returned by calibration_check"`
	ActualScore  float64 `json:"actual_score" jsonschema:"actual score as a 0..1 float (0=total failure, 1=perfect success)"`
}

func registerRecordCalibrationResult(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_calibration_result",
		Description: "Compare the learner's prediction with the actual result. Updates the calibration bias.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordCalibrationResultParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_calibration_result", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if params.PredictionID == "" {
			r, _ := errorResult("prediction_id is required")
			return r, nil, nil
		}

		// Reject NaN/Inf and out-of-range scores rather than silently
		// persisting them. The bias estimator (GetCalibrationBias) averages
		// `predicted - actual`, so a single hallucinated 100.0 corrupts the
		// rolling estimate for the learner. See issue #83 (gap left from
		// the #25/#50 numeric-validation pass).
		if err := validateUnitInterval("actual_score", params.ActualScore); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// Ownership is enforced at the DB layer: GetCalibrationRecord returns
		// "not found" if the prediction belongs to another learner (issue #87).
		record, err := deps.Store.GetCalibrationRecord(ctx, params.PredictionID, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "prediction not found", err)
			return r, nil, nil
		}

		delta := record.Predicted - params.ActualScore

		if err := deps.Store.CompleteCalibrationRecord(ctx, params.PredictionID, learnerID, params.ActualScore, delta); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to record result", err)
			return r, nil, nil
		}

		bias, _ := deps.Store.GetCalibrationBias(ctx, learnerID, 20)

		r, _ := jsonResult(map[string]interface{}{
			"delta":                    delta,
			"calibration_bias_updated": bias,
		})
		return r, nil, nil
	})
}

func generatePredictionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "cal_" + base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}
