// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ImplementationIntentionInput struct {
	Trigger      string `json:"trigger" jsonschema:"'when' clause (e.g. 'tomorrow morning at the coffee shop')"`
	Action       string `json:"action" jsonschema:"'then I will' clause (e.g. 'do 1 derivatives exercise')"`
	ScheduledFor string `json:"scheduled_for,omitempty" jsonschema:"optional ISO 8601 timestamp (UTC)"`
}

type RecordSessionCloseParams struct {
	DomainID                string                        `json:"domain_id,omitempty" jsonschema:"domain ID (optional)"`
	ImplementationIntention *ImplementationIntentionInput `json:"implementation_intention,omitempty" jsonschema:"optional if-then commitment (Gollwitzer)"`
}

func registerRecordSessionClose(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_session_close",
		Description: "Close the session: optionally record an implementation intention (if-then) and return structured signals for composing the closing message (concepts practiced, wins, intent prompt, message queue filler).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params RecordSessionCloseParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "record_session_close", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		domain, err := resolveDomain(ctx, deps.Store, learnerID, params.DomainID)
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

		// Persist the implementation intention if provided.
		if params.ImplementationIntention != nil &&
			params.ImplementationIntention.Trigger != "" &&
			params.ImplementationIntention.Action != "" {
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
			var scheduled time.Time
			if params.ImplementationIntention.ScheduledFor != "" {
				parsed, parseErr := time.Parse(time.RFC3339, params.ImplementationIntention.ScheduledFor)
				if parseErr != nil {
					r, _ := errorResult("implementation_intention.scheduled_for must be RFC3339")
					return r, nil, nil
				}
				scheduled = parsed
			}
			if _, err := deps.Store.InsertImplementationIntention(ctx,
				learnerID, domain.ID,
				params.ImplementationIntention.Trigger,
				params.ImplementationIntention.Action,
				scheduled,
			); err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to record implementation intention", err)
				return r, nil, nil
			}
		}

		recap, err := buildRecapBrief(ctx, deps, learnerID, domain)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to build session recap", err)
			return r, nil, nil
		}
		payload := map[string]any{"recap_brief": recap}
		if memory.Enabled() {
			payload["summary_request"] = map[string]any{
				"template": memory.SessionSummaryTemplate,
				"expected_calls": []string{
					"update_learner_memory(scope='session', ...)",
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
func buildRecapBrief(ctx context.Context, deps *Deps, learnerID string, domain *models.Domain) (*models.RecapBrief, error) {
	domainSet := make(map[string]bool, len(domain.Graph.Concepts))
	for _, c := range domain.Graph.Concepts {
		domainSet[c] = true
	}

	sessionInteractions, err := deps.Store.GetSessionInteractionsInDomain(ctx, learnerID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("load session interactions: %w", err)
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
