// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ImplementationIntentionInput struct {
	Trigger      string `json:"trigger" jsonschema:"'when' clause (e.g. 'tomorrow morning at the coffee shop')"`
	Action       string `json:"action" jsonschema:"'then I will' clause (e.g. 'do 1 derivatives exercise')"`
	ScheduledFor string `json:"scheduled_for,omitempty" jsonschema:"optional ISO 8601 timestamp (UTC)"`
}

type RecordSessionCloseParams struct {
	IdempotentMutationParams
	SessionID               string                        `json:"session_id,omitempty" jsonschema:"durable learning session ID; omit to close the learner's active session"`
	DomainID                string                        `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
	ImplementationIntention *ImplementationIntentionInput `json:"implementation_intention,omitempty" jsonschema:"optional if-then commitment (Gollwitzer)"`
}

func registerRecordSessionClose(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name:        "record_session_close",
		Description: "Idempotently close the durable learning session, optionally record its implementation intention (if-then), and return structured recap and summary signals.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordSessionCloseParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_session_close", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if err := validateString("session_id", params.SessionID, maxShortLabelLen); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		var learningSession *models.LearningSession
		if params.SessionID != "" {
			learningSession, err = deps.Store.GetLearningSession(ctx, learnerID, params.SessionID)
			if err != nil {
				if errors.Is(err, storeport.ErrNotFound) {
					r, _ := errorResult("learning session not found")
					return r, nil, nil
				}
				r, _ := safeErrorResult(deps.Logger, "failed to load learning session", err)
				return r, nil, nil
			}
		} else {
			learningSession, err = deps.Store.GetActiveLearningSession(ctx, learnerID)
			if err != nil && !errors.Is(err, storeport.ErrNotFound) {
				r, _ := safeErrorResult(deps.Logger, "failed to load active learning session", err)
				return r, nil, nil
			}
		}

		domainID := params.DomainID
		if domainID == "" && learningSession != nil {
			domainID = learningSession.DomainID
		}
		domain, err := resolveDomain(ctx, deps.Store, learnerID, domainID)
		if err != nil && !errors.Is(err, storeport.ErrNotFound) {
			r, _ := safeErrorResult(deps.Logger, "failed to resolve session domain", err)
			return r, nil, nil
		}
		if err != nil || domain == nil {
			if params.DomainID != "" {
				deps.Logger.Error("record_session_close: domain not found by id", "err", err, "learner", learnerID, "domain_id", params.DomainID)
				r, _ := errorResult("domain not found")
				return r, nil, nil
			}
			deps.Logger.Info("record_session_close: no active domain - needs setup", "learner", learnerID)
			r, _ := noActiveDomainResult()
			return r, nil, nil
		}
		if learningSession == nil {
			// Compatibility for clients that have not called
			// start_learning_session yet: create the durable boundary now.
			learningSession, err = deps.Store.OpenLearningSession(ctx, learnerID, domain.ID, "", time.Now().UTC())
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to open learning session", err)
				return r, nil, nil
			}
		}
		if learningSession.DomainID != "" && learningSession.DomainID != domain.ID {
			r, _ := errorResult("learning session belongs to another domain")
			return r, nil, nil
		}

		// Validate the optional intention before any intention/close write.
		hasIntention := params.ImplementationIntention != nil &&
			params.ImplementationIntention.Trigger != "" &&
			params.ImplementationIntention.Action != ""
		var intentionScheduled time.Time
		if hasIntention {
			// String length caps (issue #82). Trigger / Action are user-authored
			// if-then sentences that flow straight into implementation_intentions
			// rows; without these guards a misbehaving caller could push multi-MB
			// strings into the table. ScheduledFor is an ISO 8601 stamp — capped
			// at maxShortLabelLen to bound parser cost.
			stringFields := []struct {
				name  string
				value string
				max   int
			}{
				{"implementation_intention.trigger", params.ImplementationIntention.Trigger, maxNoteLen},
				{"implementation_intention.action", params.ImplementationIntention.Action, maxNoteLen},
				{"implementation_intention.scheduled_for", params.ImplementationIntention.ScheduledFor, maxShortLabelLen},
			}
			for _, f := range stringFields {
				if err := validateString(f.name, f.value, f.max); err != nil {
					r, _ := errorResult(err.Error())
					return r, nil, nil
				}
			}
			if params.ImplementationIntention.ScheduledFor != "" {
				parsed, parseErr := time.Parse(time.RFC3339, params.ImplementationIntention.ScheduledFor)
				if parseErr != nil {
					r, _ := errorResult("implementation_intention.scheduled_for must be RFC3339")
					return r, nil, nil
				}
				intentionScheduled = parsed
			}
		}

		// Build every fallible read-side enrichment before the atomic write.
		// If recap construction fails, neither an intention nor a closed session
		// is left behind for the idempotency middleware to misclassify.
		recap, err := buildRecapBrief(ctx, deps, learnerID, learningSession.ID, domain)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to build session recap", err)
			return r, nil, nil
		}
		if hasIntention {
			// The intention is committed immediately below in the same transaction
			// as the close, so the response must not ask for another one merely
			// because the pre-write recap query could not see it yet.
			recap.PromptForImplementationIntent = false
		}
		var closedSession *models.LearningSession
		err = deps.Store.WithTx(ctx, func(tx storeport.Store) error {
			if hasIntention {
				if _, err := tx.InsertImplementationIntentionForSession(ctx,
					learnerID, domain.ID, learningSession.ID,
					params.ImplementationIntention.Trigger,
					params.ImplementationIntention.Action,
					intentionScheduled,
				); err != nil {
					return fmt.Errorf("record implementation intention: %w", err)
				}
			}
			var closeErr error
			closedSession, closeErr = tx.CloseLearningSession(ctx, learnerID, learningSession.ID, time.Now().UTC())
			if closeErr != nil {
				return fmt.Errorf("close learning session: %w", closeErr)
			}
			return nil
		})
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to finalize learning session", err)
			return r, nil, nil
		}
		payload := map[string]any{
			"recap_brief": recap,
			"session":     closedSession,
			"session_id":  closedSession.ID,
		}
		if memory.Enabled() {
			payload["summary_request"] = map[string]any{
				"session_id": learningSession.ID,
				"domain_id":  domain.ID,
				"template":   memory.SessionSummaryTemplate,
				"expected_calls": []string{
					"update_learner_memory(scope='session', session_id='" + learningSession.ID + "', ...)",
					"optionally update_learner_memory(scope='concept', ...)",
					"optionally update_learner_memory(scope='memory_pending', ...)",
				},
			}
		}
		r, _ := jsonResult(payload)
		return r, nil, nil
	})
}

// buildRecapBrief produces session-close signals for Claude.
func buildRecapBrief(ctx context.Context, deps *Deps, learnerID, sessionID string, domain *models.Domain) (*models.RecapBrief, error) {
	domainSet := make(map[string]bool, len(domain.Graph.Concepts))
	for _, c := range domain.Graph.Concepts {
		domainSet[c] = true
	}

	sessionInteractions, err := deps.Store.GetInteractionsBySessionInDomain(ctx, learnerID, sessionID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("load session interactions: %w", err)
	}
	if len(sessionInteractions) == 0 {
		// Legacy compatibility: pre-migration interactions have no session_id.
		// They may contribute to the recap, but explicit rows from prior closed
		// sessions are never pulled into the new durable session.
		legacy, legacyErr := deps.Store.GetInteractionsSinceInDomain(ctx, learnerID, domain.ID, time.Now().UTC().Add(-2*time.Hour))
		if legacyErr != nil {
			return nil, fmt.Errorf("load legacy session interactions: %w", legacyErr)
		}
		for _, interaction := range legacy {
			if interaction.SessionID == "" {
				sessionInteractions = append(sessionInteractions, interaction)
			}
		}
	}

	practicedSet := map[string]bool{}
	winsSet := map[string]bool{}
	strugglesSet := map[string]bool{}
	for _, i := range sessionInteractions {
		if !domainSet[i.Concept] {
			continue
		}
		practicedSet[i.Concept] = true
		if i.Success {
			winsSet[i.Concept] = true
		} else {
			strugglesSet[i.Concept] = true
		}
	}

	practiced := mapKeys(practicedSet)
	wins := mapKeys(winsSet)
	// "Interesting" struggles = failed but not completely blocked (partial progress signal heuristic:
	// the learner also had a success on the same concept during the session).
	var interesting []string
	for c := range strugglesSet {
		if winsSet[c] {
			interesting = append(interesting, c)
		}
	}

	// Next scheduled review — earliest next_review across domain states.
	states, err := deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("load domain states: %w", err)
	}
	var next string
	var earliest time.Time
	for _, cs := range states {
		if !domainSet[cs.Concept] || cs.NextReview == nil {
			continue
		}
		if earliest.IsZero() || cs.NextReview.Before(earliest) {
			earliest = *cs.NextReview
			next = fmt.Sprintf("%s (%s)", cs.Concept, cs.NextReview.Format("02/01 15:04 UTC"))
		}
	}

	// Prompt for implementation intention if none recorded in last 7 days for any domain.
	has, err := deps.Store.HasRecentImplementationIntention(ctx, learnerID, "", time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("load recent implementation intentions: %w", err)
	}
	promptIntent := !has

	instruction := "Close the session in 2-3 sentences. Mention a tangible win or a good attempt. " +
		"If prompt_for_implementation_intention is true, ask a concrete question like 'When and where will you practice next?' " +
		"and wait for the answer to call record_session_close again with implementation_intention. " +
		"Then call get_olm_snapshot to retrieve the learner's structured cognitive state, " +
		"and call queue_webhook_message 3 times: daily_motivation for tomorrow at 8h UTC (warm, tied to personal_goal) ; " +
		"olm:<domain_id> for tomorrow at 13h UTC using the structured brief field (why_now + learning_gain + open_loop + next_action, content must MATCH get_olm_snapshot, no pep talk) ; " +
		"daily_recap for tomorrow at 21h UTC (gentle recap). " +
		"Messages must be user-friendly, concrete, learner-facing, and oriented toward learning gain. No raw KPIs, no internal tool names."

	return &models.RecapBrief{
		ConceptsPracticed:             practiced,
		Wins:                          wins,
		InterestingStruggles:          interesting,
		NextScheduledReview:           next,
		PromptForImplementationIntent: promptIntent,
		Instruction:                   instruction,
	}, nil
}

func mapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
