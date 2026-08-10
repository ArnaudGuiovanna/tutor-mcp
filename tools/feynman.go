// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"tutor-mcp/algorithms"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type FeynmanChallengeParams struct {
	Concept   string `json:"concept,omitempty" jsonschema:"the concept to explain using the Feynman method; canonical key for concept-targeting tools; required unless concept_id is used"`
	ConceptID string `json:"concept_id,omitempty" jsonschema:"deprecated compatibility alias for concept; prefer concept"`
	DomainID  string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerFeynmanChallenge(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "feynman_challenge",
		Description: "Ask the learner to explain a concept whose model estimate reached the deep-evidence routing threshold. This probes understanding; it is not itself a demonstrated-mastery claim.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params FeynmanChallengeParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "feynman_challenge", err)
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

		// String length cap (issue #82). concept is already checked by
		// normalizeConceptParam; domain_id is bounded here for consistency
		// with the other concept-targeting tools.
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// Resolve the active domain (honoring the optional domain_id) and
		// validate the concept against its concept list before touching
		// concept_state. Without this guard the tool silently serves a
		// Feynman challenge for a hallucinated or stale concept name that
		// isn't part of the resolved domain — see issue #8 (mirrors the guard
		// in record_transfer_result).
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("feynman_challenge: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("feynman_challenge: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
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
				"threshold":        algorithms.MasteryBKT(),
				"message":          "The model estimate has not reached the Feynman-probe routing threshold. Continue with varied practice first.",
			})
			return r, nil, nil
		}

		promptText := fmt.Sprintf(
			"Explain the concept '%s' as if you were teaching it to someone who knows nothing about it. "+
				"No technical jargon - use analogies and concrete examples. "+
				"The goal is to verify that you truly understood it, not just that you can recite it.\n\n"+
				"After your explanation, I will identify the fuzzy or incomplete points "+
				"and turn them into micro-concepts to work on.",
			concept,
		)

		r, _ := jsonResult(map[string]interface{}{
			"eligible":    true,
			"prompt_text": promptText,
			"concept":     concept,
			"instructions_for_llm": "After the learner's explanation, identify the specific conceptual gaps. " +
				"For each gap, generate a short label and a description. " +
				"Ask the learner for confirmation, reload get_curriculum_snapshot, then add the gaps with its expected_version. " +
				"When a confirmed gap is prerequisite knowledge, make the source concept depend on the new micro-concept; do not reverse that edge.",
			"gap_template": map[string]interface{}{
				"label":            "<short gap name>",
				"description":      "<what is missing from the explanation>",
				"relationship":     "prerequisite_of_source",
				"source_concept":   concept,
				"expected_version": "<version from get_curriculum_snapshot>",
			},
		})
		return r, nil, nil
	})
}
