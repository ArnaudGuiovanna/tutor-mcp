// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TransferChallengeParams struct {
	Concept     string `json:"concept" jsonschema:"the concept to test in transfer (concept key from the domain graph)"`
	ContextType string `json:"context_type,omitempty" jsonschema:"context type: near, far, real_world, interview, teaching, debugging, creative (optional)"`
	DomainID    string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerTransferChallenge(server *mcp.Server, deps *Deps) {
	addTool(server, deps, &mcp.Tool{
		Name:        "transfer_challenge",
		Description: "Generate a novel situation to probe transfer outside the training context. A transferred mastery claim still requires frozen assessment evidence and a trusted evaluation across multiple dimensions.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params TransferChallengeParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "transfer_challenge", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		concept, err := normalizeConceptParam(params.Concept)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if concept == "" {
			r, _ := errorResult("concept is required")
			return r, nil, nil
		}

		// String length caps (issue #82). concept flows into the
		// transfer_records query / response and context_type ends up in the
		// persisted row label — without these guards a misbehaving caller
		// could push multi-MB strings into the read path and bloat downstream
		// telemetry.
		stringFields := []struct {
			name  string
			value string
			max   int
		}{
			{"context_type", params.ContextType, maxShortLabelLen},
			{"domain_id", params.DomainID, maxShortLabelLen},
		}
		for _, f := range stringFields {
			if err := validateString(f.name, f.value, f.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Resolve the active domain (honoring the optional domain_id) and
		// validate the concept against its concept list before touching
		// concept_state. Without this guard the tool silently serves a
		// transfer challenge for a hallucinated or stale concept name that
		// isn't part of the resolved domain — see issue #8 .
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("transfer_challenge: domain not found by id", append(diagnosticErrorAttrs(err), "learner", learnerID, "domain_id", params.DomainID)...)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("transfer_challenge: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult(deps)
			return r, nil, nil
		}
		if err := validateConceptInDomain(domain, concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		cs, err := deps.Store.GetConceptStateInDomain(ctx, learnerID, domain.ID, concept)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "concept not found", err)
			return r, nil, nil
		}

		bktState := algorithms.BKTState{PMastery: cs.PMastery}
		if !algorithms.BKTIsMastered(bktState) {
			r, _ := jsonResult(map[string]interface{}{
				"eligible":         false,
				"mastery_estimate": cs.PMastery,
				"mastery":          cs.PMastery, // deprecated estimate alias
				"message":          "The model estimate has not reached the transfer-probe routing threshold.",
			})
			return r, nil, nil
		}

		existingTransfers, err := deps.Store.GetTransferScoresInDomain(ctx, learnerID, domain.ID, concept)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load transfer history", err)
			return r, nil, nil
		}
		transferProfile := engine.BuildTransferProfile(concept, existingTransfers)

		contextType := params.ContextType
		if contextType == "" {
			contextType = chooseTransferContextType(transferProfile)
		}
		transferDimension, ok := engine.NormalizeTransferDimension(contextType)
		if !ok {
			r, _ := errorResult(fmt.Sprintf(
				"context_type %q is invalid: must be one of: %s",
				contextType, strings.Join(allowedTransferContextTypes, ", "),
			))
			return r, nil, nil
		}

		var testedContexts []string
		for _, tr := range existingTransfers {
			testedContexts = append(testedContexts, fmt.Sprintf("%s (%.0f%%)", tr.ContextType, tr.Score*100))
		}

		promptText := fmt.Sprintf(
			"Generate a completely new situation that tests the transfer of the concept '%s' "+
				"in a context of type '%s'. "+
				"The situation must NOT resemble previous exercises. "+
				"The goal: verify that the learner can apply this concept in a context they have never seen.\n\n"+
				"Before showing it, freeze the task and rubric with prepare_assessment_attempt "+
				"using activity_type=TRANSFER_PROBE. After the learner responds, call "+
				"submit_assessment_attempt, then record_interaction with the attempt_id, "+
				"criterion scores, transfer_dimension and transfer_score.",
			concept, contextType,
		)

		r, _ := jsonResult(map[string]interface{}{
			"eligible":           true,
			"concept":            concept,
			"context_type":       contextType,
			"transfer_dimension": string(transferDimension),
			"prompt_text":        promptText,
			"tested_contexts":    testedContexts,
			"transfer_profile":   transferProfile,
		})
		return r, nil, nil
	})
}

// allowedTransferContextTypes lists the context_type values documented by
// transfer_challenge.
var allowedTransferContextTypes = []string{
	"near", "far", "real_world", "interview", "teaching", "debugging", "creative",
}

func chooseTransferContextType(profile engine.TransferProfile) string {
	if len(profile.MissingDimensions) > 0 {
		return string(profile.MissingDimensions[0])
	}
	if len(profile.WeakestDimensions) > 0 {
		return string(profile.WeakestDimensions[0])
	}
	return string(engine.TransferDimensionFar)
}
