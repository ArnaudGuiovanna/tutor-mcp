// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UpdateLearnerProfileParams enumerates the fields the chat-side LLM may push
// into a learner's persisted profile blob. Issue #61: `level`, `background`
// and `learning_style` were removed because no downstream component (motivation
// brief, concept selector, alerts, dashboard) consumes them — they were
// write-only metadata that bloated the profile_json blob without informing
// any decision. The forward-only migration in db/migrations.go scrubs the keys
// out of existing rows so a re-introduction won't silently inherit stale data.
// CalibrationBias and AutonomyScore are *float64 (issue #89) so that nil
// distinguishes "caller did not supply this field" from a legitimate 0
// value. `calibration_bias=0` is perfect calibration; `autonomy_score=0` is
// fully dependent — both are values the system itself produces and the LLM
// must be able to push through this tool. Matches the pointer pattern used
// for ImplementationIntention in record_session_close. The ,omitempty on
// the JSON tag is dropped because nil already serialises to absent.
type UpdateLearnerProfileParams struct {
	Device          string   `json:"device,omitempty" jsonschema:"primary device (e.g. laptop, phone, tablet)"`
	Objective       string   `json:"objective,omitempty" jsonschema:"updated learning objective"`
	Language        string   `json:"language,omitempty" jsonschema:"preferred language for exercises (BCP-47 hint to the LLM)"`
	CalibrationBias *float64 `json:"calibration_bias,omitempty" jsonschema:"signed calibration bias as a -1..1 float (positive=over-estimated, negative=under-estimated). Provide explicitly to overwrite; omit to leave unchanged. 0 = perfect calibration."`
	AffectBaseline  string   `json:"affect_baseline,omitempty" jsonschema:"learner's emotional baseline"`
	AutonomyScore   *float64 `json:"autonomy_score,omitempty" jsonschema:"current autonomy score as a 0..1 float. Provide explicitly to overwrite; omit to leave unchanged. 0 = fully dependent."`
}

func registerUpdateLearnerProfile(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_learner_profile",
		Description: "Update the learner's persistent metadata (device, objective, language, calibration, affect, autonomy). Only provided fields are modified.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UpdateLearnerProfileParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "update_learner_profile", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		learner, err := deps.Store.GetLearnerByID(learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "learner not found", err)
			return r, nil, nil
		}

		// Length caps (issue #31). Each field is a free-text input; without
		// these caps a misbehaving caller can inflate the profile_json blob
		// to multiple MB, slowing every subsequent read of this learner.
		stringFields := []struct {
			name  string
			value string
			max   int
		}{
			{"device", params.Device, maxShortLabelLen},
			{"objective", params.Objective, maxNoteLen},
			{"language", params.Language, maxShortLabelLen},
			{"affect_baseline", params.AffectBaseline, maxShortLabelLen},
		}
		for _, f := range stringFields {
			if err := validateString(f.name, f.value, f.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Numeric guards (issue #85). Without these, the chat-side LLM can
		// push NaN/Inf or out-of-range floats into profile_json — NaN cannot
		// survive json.Marshal at all and +Inf marshals to the literal `+Inf`
		// which is invalid JSON, poisoning every downstream consumer that
		// reads profile_json (motivation brief, dashboard, get_olm_snapshot).
		// CalibrationBias / AutonomyScore are *float64 since #89 (so that 0
		// can be distinguished from "not provided"). Skip validation when
		// the caller omitted the field (nil pointer); validate the
		// dereferenced value otherwise.
		if params.CalibrationBias != nil {
			if err := validateBoundedFinite("calibration_bias", *params.CalibrationBias, -1, 1); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		if params.AutonomyScore != nil {
			if err := validateUnitInterval("autonomy_score", *params.AutonomyScore); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Load existing profile
		profile := make(map[string]interface{})
		if learner.ProfileJSON != "" && learner.ProfileJSON != "{}" {
			_ = json.Unmarshal([]byte(learner.ProfileJSON), &profile)
		}

		// Merge only non-empty fields
		updated := 0
		if params.Device != "" {
			profile["device"] = params.Device
			updated++
		}
		if params.Language != "" {
			profile["language"] = params.Language
			updated++
		}
		// nil pointer = caller omitted the field; non-nil = set this exact
		// value (0 included). Issue #89.
		if params.CalibrationBias != nil {
			profile["calibration_bias"] = *params.CalibrationBias
			updated++
		}
		if params.AffectBaseline != "" {
			profile["affect_baseline"] = params.AffectBaseline
			updated++
		}
		if params.AutonomyScore != nil {
			profile["autonomy_score"] = *params.AutonomyScore
			updated++
		}

		// Update objective on learner record if provided
		if params.Objective != "" {
			profile["objective"] = params.Objective
			updated++
		}

		profileJSON, _ := json.Marshal(profile)
		if err := deps.Store.UpdateLearnerProfile(learnerID, string(profileJSON)); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to update profile", err)
			return r, nil, nil
		}

		r, _ := jsonResult(map[string]interface{}{
			"updated":        true,
			"fields_changed": updated,
			"profile":        profile,
		})
		return r, nil, nil
	})
}
