// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UpdateLearnerProfileParams contains learner-declared preferences only.
// Calibration and autonomy are evidence-derived values and are intentionally
// not writable through this tool.
type UpdateLearnerProfileParams struct {
	IdempotentMutationParams
	Device         string `json:"device,omitempty" jsonschema:"primary device (e.g. laptop, phone, tablet)"`
	Objective      string `json:"objective,omitempty" jsonschema:"updated learning objective"`
	Language       string `json:"language,omitempty" jsonschema:"preferred language for exercises (BCP-47 hint to the LLM)"`
	AffectBaseline string `json:"affect_baseline,omitempty" jsonschema:"learner's emotional baseline"`
}

func registerUpdateLearnerProfile(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "update_learner_profile",
		Description: "Update learner-declared preferences (device, objective, language and affect baseline). Evidence-derived calibration and autonomy cannot be overwritten.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UpdateLearnerProfileParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "update_learner_profile", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		learner, err := deps.Store.GetLearnerByID(ctx, learnerID)
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

		// Load the typed profile. Corrupt persisted JSON is a data-integrity
		// error: silently replacing it would erase learner preferences.
		profile := models.LearnerProfile{}
		if learner.ProfileJSON != "" && learner.ProfileJSON != "{}" {
			if err := json.Unmarshal([]byte(learner.ProfileJSON), &profile); err != nil {
				deps.Logger.Error("stored learner profile is invalid", "err", err, "learner", learnerID)
				r, _ := errorResult("stored learner profile is invalid")
				return r, nil, nil
			}
		}

		// Merge only non-empty fields
		updated := 0
		if params.Device != "" {
			profile.Device = params.Device
			updated++
		}
		if params.Language != "" {
			profile.Language = params.Language
			updated++
		}
		if params.AffectBaseline != "" {
			profile.AffectBaseline = params.AffectBaseline
			updated++
		}

		var objective *string
		if params.Objective != "" {
			objective = &params.Objective
			updated++
		}

		profileJSON, err := json.Marshal(profile)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to encode learner profile", fmt.Errorf("marshal profile: %w", err))
			return r, nil, nil
		}
		if err := deps.Store.UpdateLearnerProfile(ctx, learnerID, string(profileJSON), objective); err != nil {
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
