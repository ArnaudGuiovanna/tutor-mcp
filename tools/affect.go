// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"time"

	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type RecordAffectParams struct {
	IdempotentMutationParams
	SessionID           string `json:"session_id" jsonschema:"unique session identifier"`
	Energy              int    `json:"energy,omitempty" jsonschema:"available energy: 1=tired, 2=neutral, 3=motivated, 4=on fire"`
	Confidence          int    `json:"confidence,omitempty" jsonschema:"subject confidence: 1=anxious, 2=foggy, 3=ok, 4=confident"`
	Satisfaction        int    `json:"satisfaction,omitempty" jsonschema:"overall feeling (end of session): 1=frustrating, 2=hard, 3=good, 4=flow"`
	PerceivedDifficulty int    `json:"perceived_difficulty,omitempty" jsonschema:"perceived difficulty (end of session): 1=too hard, 2=challenging, 3=ok, 4=too easy"`
	NextSessionIntent   int    `json:"next_session_intent,omitempty" jsonschema:"next session intent: 1=now, 2=tomorrow, 3=this week, 4=not sure"`
}

func registerRecordAffect(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_affect",
		Description: "Record the learner's emotional state. Call at session start (energy, confidence) and at session end (satisfaction, perceived_difficulty, next_session_intent).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordAffectParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_affect", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		if params.SessionID == "" {
			r, _ := errorResult("session_id is required")
			return r, nil, nil
		}
		// Length cap (issue #31) — without this guard a multi-MB session_id
		// would be silently persisted, bloating the affect/calibration tables.
		if err := validateString("session_id", params.SessionID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		learningSession, err := deps.Store.GetLearningSession(ctx, learnerID, params.SessionID)
		if err != nil {
			r, _ := errorResult("learning session not found")
			return r, nil, nil
		}
		if learningSession.Status != models.LearningSessionStatusOpen {
			r, _ := errorResult("learning session is closed")
			return r, nil, nil
		}

		// Likert-scale guards (1..4 per AffectState model docs). Each field
		// uses omitempty so 0 means "not provided" and is allowed through;
		// any other out-of-range value would silently corrupt downstream
		// calibration_bias_delta and tutor_mode_override logic.
		for _, c := range []struct {
			field string
			value int
		}{
			{"energy", params.Energy},
			{"confidence", params.Confidence},
			{"satisfaction", params.Satisfaction},
			{"perceived_difficulty", params.PerceivedDifficulty},
			{"next_session_intent", params.NextSessionIntent},
		} {
			if err := validateLikertInt(c.field, c.value, 1, 4); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		if _, err := deps.Store.TouchLearningSession(ctx, learnerID, params.SessionID, time.Now().UTC()); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update learning session", err)
			return r, nil, nil
		}

		affect := &models.AffectState{
			LearnerID:           learnerID,
			SessionID:           params.SessionID,
			Energy:              params.Energy,
			SubjectConfidence:   params.Confidence,
			Satisfaction:        params.Satisfaction,
			PerceivedDifficulty: params.PerceivedDifficulty,
			NextSessionIntent:   params.NextSessionIntent,
		}

		if err := deps.Store.UpsertAffectState(ctx, affect); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to record affect", err)
			return r, nil, nil
		}

		var degradedComponents []string
		markDegraded := func(component string, err error) {
			degradedComponents = append(degradedComponents, component)
			deps.Logger.Warn("record_affect: optional component degraded",
				"component", component, "err", err, "learner", learnerID, "session", params.SessionID)
		}
		saved, err := deps.Store.GetAffectBySession(ctx, learnerID, params.SessionID)
		if err != nil {
			markDegraded("stored_affect_readback", err)
			saved = affect
		}

		result := map[string]interface{}{
			"affect_state": saved,
		}

		// Compute tutor_mode_override from start-of-session affect
		if saved.SubjectConfidence == 1 {
			result["tutor_mode_override"] = "scaffolding"
		} else if saved.Energy == 1 {
			result["tutor_mode_override"] = "lighter"
		}

		// End-of-session: compute calibration_bias_delta
		if params.Satisfaction > 0 && params.PerceivedDifficulty > 0 {
			perceivedAbility := float64(params.PerceivedDifficulty) / 4.0
			sessionInteractions, sessionErr := deps.Store.GetInteractionsBySession(ctx, learnerID, params.SessionID)
			if sessionErr != nil {
				markDegraded("calibration_delta", sessionErr)
			} else if len(sessionInteractions) > 0 {
				successes := 0
				for _, i := range sessionInteractions {
					if i.Success {
						successes++
					}
				}
				actualRate := float64(successes) / float64(len(sessionInteractions))
				calibDelta := perceivedAbility - actualRate
				result["calibration_bias_delta"] = calibDelta
			}

			// Compute and persist autonomy score
			since := time.Now().UTC().Add(-30 * 24 * time.Hour)
			allInteractions, interactionsErr := deps.Store.GetInteractionsSince(ctx, learnerID, since)
			allStates, statesErr := deps.Store.GetConceptStatesByLearner(ctx, learnerID)
			calibBias, calibrationErr := deps.Store.GetCalibrationBias(ctx, learnerID, 20)
			switch {
			case interactionsErr != nil:
				markDegraded("autonomy_score", interactionsErr)
			case statesErr != nil:
				markDegraded("autonomy_score", statesErr)
			case calibrationErr != nil:
				markDegraded("autonomy_score", calibrationErr)
			default:
				autonomy := engine.ComputeAutonomyMetrics(engine.AutonomyInput{
					Interactions:    allInteractions,
					ConceptStates:   allStates,
					CalibrationBias: calibBias,
					SessionGap:      2 * time.Hour,
				})
				if err := deps.Store.UpdateAffectAutonomyScore(ctx, learnerID, params.SessionID, autonomy.Score); err != nil {
					markDegraded("autonomy_score_persistence", err)
				} else {
					result["autonomy_score"] = autonomy.Score
				}
			}
		}
		if len(degradedComponents) > 0 {
			result["degraded_components"] = degradedComponents
		}

		r, _ := jsonResult(result)
		return r, nil, nil
	})
}
