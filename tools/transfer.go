// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TransferChallengeParams struct {
	Concept     string `json:"concept,omitempty" jsonschema:"the concept to test in transfer; canonical key for concept-targeting tools; required unless concept_id is used"`
	ConceptID   string `json:"concept_id,omitempty" jsonschema:"deprecated compatibility alias for concept; prefer concept"`
	ContextType string `json:"context_type,omitempty" jsonschema:"context type: near, far, real_world, interview, teaching, debugging, creative (optional)"`
	DomainID    string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerTransferChallenge(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "transfer_challenge",
		Description: "Generate a novel situation to probe transfer outside the training context. A transferred mastery claim still requires frozen assessment evidence and a trusted evaluation across multiple dimensions.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params TransferChallengeParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "transfer_challenge", err)
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
		// isn't part of the resolved domain — see issue #8 (mirrors the guard
		// in record_transfer_result).
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("transfer_challenge: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("transfer_challenge: no active domain - needs setup", "learner", learnerID)
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
				"criterion scores, transfer_dimension and transfer_score. A direct "+
				"record_transfer_result call is only an unverified legacy observation.",
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

// ─── record_transfer_result ─────────────────────────────────────────────────

type RecordTransferResultParams struct {
	IdempotentMutationParams
	Concept     string  `json:"concept,omitempty" jsonschema:"the concept being tested; canonical key for concept-targeting tools; required unless concept_id is used"`
	ConceptID   string  `json:"concept_id,omitempty" jsonschema:"deprecated compatibility alias for concept; prefer concept"`
	ContextType string  `json:"context_type" jsonschema:"challenge context type: near, far, real_world, interview, teaching, debugging, creative"`
	Score       float64 `json:"score" jsonschema:"transfer score as a 0..1 float (0=failed transfer, 1=perfect transfer)"`
	SessionID   string  `json:"session_id,omitempty" jsonschema:"session ID (optional)"`
	DomainID    string  `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

// allowedRecordTransferContextTypes enumerates the documented context_type
// values for record_transfer_result. Issue #96: a free-string ContextType
// allowed orphan transfer rows ("banana", "") to pollute TRANSFER_BLOCKED
// alerts. Kept inline here (rather than in tools/validate.go) to avoid a
// merge conflict with the cycle-2 sibling PR that introduces validateEnum.
var allowedTransferContextTypes = []string{
	"near", "far", "real_world", "interview", "teaching", "debugging", "creative",
}

func registerRecordTransferResult(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "record_transfer_result",
		Description: "Record a legacy, unverified transfer observation for adaptive routing. It cannot establish transferred mastery; use the assessment-attempt workflow for evidence-bearing transfer probes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordTransferResultParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_transfer_result", err)
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

		// String length caps (issue #82). concept, context_type and
		// session_id end up in transfer_records rows; without these guards a
		// misbehaving caller could push multi-MB strings into the table.
		stringFields := []struct {
			name  string
			value string
			max   int
		}{
			{"context_type", params.ContextType, maxShortLabelLen},
			{"session_id", params.SessionID, maxShortLabelLen},
			{"domain_id", params.DomainID, maxShortLabelLen},
		}
		for _, f := range stringFields {
			if err := validateString(f.name, f.value, f.max); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}

		// Score is a unit-interval transfer rating. Reject NaN/Inf and any
		// value outside [0, 1] to keep transfer_score reports & the
		// blocked < 0.50 gate meaningful (issue #25).
		if err := validateUnitInterval("score", params.Score); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// Enum guard for context_type. Without this, hallucinated labels
		// ("banana", "") flowed into the transfer_records table and
		// poisoned downstream TRANSFER_BLOCKED alerts (issue #96).
		transferDimension, ok := engine.NormalizeTransferDimension(params.ContextType)
		if !ok {
			r, _ := errorResult(fmt.Sprintf(
				"context_type %q is invalid: must be one of: %s",
				params.ContextType, strings.Join(allowedTransferContextTypes, ", "),
			))
			return r, nil, nil
		}

		// Resolve the active domain (honoring the optional domain_id) and
		// validate the concept against its concept list. Without this guard
		// the tool silently inserts orphan transfer rows for hallucinated
		// or stale concept names — see issue #96.
		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("record_transfer_result: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("record_transfer_result: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}
		if err := validateConceptInDomain(domain, concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		learningSession, err := resolveOpenLearningSession(ctx, deps, learnerID, domain.ID, params.SessionID, time.Now().UTC())
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		record := &models.TransferRecord{
			LearnerID:   learnerID,
			DomainID:    domain.ID,
			ConceptID:   concept,
			ContextType: params.ContextType,
			Score:       params.Score,
			SessionID:   learningSession.ID,
		}

		if err := deps.Store.CreateTransferRecord(ctx, record); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to record transfer", err)
			return r, nil, nil
		}

		updatedTransfers, err := deps.Store.GetTransferScoresInDomain(ctx, learnerID, domain.ID, concept)
		if err != nil {
			// The transfer row is already committed. Treat readback as a degraded
			// optional component so an idempotent retry cannot insert it twice.
			deps.Logger.Warn("record_transfer_result: transfer profile readback degraded", "err", err, "learner", learnerID, "domain", domain.ID, "concept", concept)
			r, _ := jsonResult(map[string]interface{}{
				"recorded":            true,
				"session_id":          learningSession.ID,
				"transfer_score":      params.Score,
				"transfer_dimension":  string(transferDimension),
				"transfer_profile":    nil,
				"blocked":             params.Score < engine.TransferFailureThreshold,
				"evidence_trust":      "unverified_observation",
				"degraded_components": []string{"transfer_profile_readback"},
			})
			return r, nil, nil
		}
		transferProfile := engine.BuildTransferProfile(concept, updatedTransfers)

		r, _ := jsonResult(map[string]interface{}{
			"recorded":           true,
			"session_id":         learningSession.ID,
			"transfer_score":     params.Score,
			"transfer_dimension": string(transferDimension),
			"transfer_profile":   transferProfile,
			"blocked":            params.Score < engine.TransferFailureThreshold,
			"evidence_trust":     "unverified_observation",
		})
		return r, nil, nil
	})
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
