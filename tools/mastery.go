// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/engine"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CheckMasteryParams struct {
	Concept   string `json:"concept,omitempty" jsonschema:"the concept to check for mastery; canonical key for concept-targeting tools; required unless concept_id is used"`
	ConceptID string `json:"concept_id,omitempty" jsonschema:"deprecated compatibility alias for concept; prefer concept"`
	DomainID  string `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
}

func registerCheckMastery(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "check_mastery",
		Description: "Check whether a concept is ready for the mastery challenge using BKT plus evidence diversity and uncertainty.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params CheckMasteryParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "check_mastery", err)
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
		if err := validateString("domain_id", params.DomainID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("check_mastery: domain not found", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			r, payload := noActiveDomainResult()
			return r, payload, nil
		}
		if err := validateConceptInDomain(domain, concept); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		domainID := domain.ID

		cs, err := deps.Store.GetConceptStateInDomain(ctx, learnerID, domainID, concept)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "concept state not found", err)
			return r, nil, nil
		}

		recent, err := deps.Store.GetRecentInteractionsInDomain(ctx, learnerID, domainID, concept, 50)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to compute mastery evidence", err)
			return r, nil, nil
		}
		now := time.Now().UTC()
		evidenceProfile := engine.BuildEvidenceProfile(learnerID, concept, recent, now)
		evidenceQuality := engine.MasteryEvidenceQuality(evidenceProfile)
		uncertainty := engine.ComputeMasteryUncertainty(cs, recent, engine.MasteryEvidenceProfile{Now: now})
		transferRecords, err := deps.Store.GetTransferScoresInDomain(ctx, learnerID, domainID, concept)
		if err != nil {
			deps.Logger.Warn("check_mastery: transfer profile fetch failed", "err", err, "learner", learnerID, "concept", concept)
			transferRecords = nil
		}
		assessments, err := deps.Store.GetEvaluatedAssessmentAttemptsInDomain(ctx, learnerID, domainID, concept, 100)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to read evaluated assessment evidence", err)
			return r, nil, nil
		}
		transferProfile := engine.BuildTrustedTransferProfileFromEvidence(concept, transferRecords, assessments, now)
		masteryStatus := engine.AssessMasteryStatus(learnerID, concept, cs, recent, transferRecords, assessments, now)
		bktMastered := masteryStatus.Estimated
		evidenceOK := evidenceQuality.Quality != engine.EvidenceQualityWeak
		uncertaintyOK := uncertainty.ConfidenceLabel != engine.MasteryConfidenceLow
		transferOK := transferProfile.ReadinessLabel != engine.TransferReadinessBlocked
		challengeReady := engine.ReadyForMasteryChallenge(masteryStatus, evidenceQuality, uncertainty, transferProfile)

		if !challengeReady {
			message := fmt.Sprintf("Not ready yet. Current model estimate: %.0f%%, routing threshold: %.0f%%", cs.PMastery*100, algorithms.MasteryBKT()*100)
			if bktMastered && !masteryStatus.Retained {
				message = "The mastery estimate is above threshold, but retention is below the recall threshold: retrieve the concept successfully before a mastery challenge."
			} else if bktMastered && !evidenceOK {
				message = "BKT is above threshold, but the evidence is still not varied enough for a mastery challenge."
			}
			if bktMastered && evidenceOK && !uncertaintyOK {
				message = "BKT is above threshold, but model uncertainty is still too high for a mastery challenge."
			}
			if bktMastered && evidenceOK && uncertaintyOK && !transferOK {
				message = "BKT is above threshold, but recent transfer is blocked: revisit the concept in another context before the mastery challenge."
			}
			r, _ := jsonResult(map[string]interface{}{
				"mastery_ready":       false,
				"challenge_ready":     false,
				"bkt_mastery_ready":   bktMastered,
				"mastery_estimate":    cs.PMastery,
				"mastery":             cs.PMastery, // deprecated estimate alias
				"mastery_status":      masteryStatus,
				"threshold":           algorithms.MasteryBKT(),
				"evidence_profile":    evidenceProfile,
				"evidence_quality":    evidenceQuality,
				"mastery_uncertainty": uncertainty,
				"transfer_profile":    transferProfile,
				"message":             message,
			})
			return r, nil, nil
		}

		r, _ := jsonResult(map[string]interface{}{
			"mastery_ready":       true,
			"challenge_ready":     true,
			"bkt_mastery_ready":   bktMastered,
			"mastery_estimate":    cs.PMastery,
			"mastery":             cs.PMastery, // deprecated estimate alias
			"mastery_status":      masteryStatus,
			"evidence_profile":    evidenceProfile,
			"evidence_quality":    evidenceQuality,
			"mastery_uncertainty": uncertainty,
			"transfer_profile":    transferProfile,
			"challenge": map[string]interface{}{
				"prompt_for_llm": fmt.Sprintf(
					"Generate a mastery challenge on %s. "+
						"The learner must produce a complete, domain-appropriate performance that integrates the concept in a novel task. "+
						"Evaluate correctness, completeness, autonomous application, domain-appropriate boundary conditions or counterexamples, and clarity of reasoning or execution. "+
						"Do not guide - observe whether the learner can apply the concept alone.",
					concept,
				),
				"evaluation_criteria": []string{
					"Autonomous application without help",
					"Correct and complete response for the domain",
					"Handles relevant boundary conditions or counterexamples",
					"Clear reasoning or execution",
				},
			},
		})
		return r, nil, nil
	})
}
