// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/models"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type GetNextActivityParams struct {
	IdempotentMutationParams
	DomainID   string `json:"domain_id,omitempty" jsonschema:"target domain ID; if absent, the learner's last active domain is used"`
	DomainName string `json:"domain_name,omitempty" jsonschema:"target domain name when the learner names a subject but the domain_id is unknown"`
	Intent     string `json:"intent,omitempty" jsonschema:"learner intent: auto or review. Use review when the learner asks to revise/review already studied material"`
}

func registerGetNextActivity(server *mcp.Server, deps *Deps) {
	addTool(server, &mcp.Tool{
		Name: "get_next_activity",
		Description: "Determine the next optimal activity for the learner and aggregate all routing context: metacognitive_mirror, tutor_mode, motivation_brief, active misconceptions. Accounts for the current session to avoid repeating the same concept. " +
			"When to call: this is the main tool of the learning cycle; it already includes alert-aware routing, metacognitive_mirror, tutor_mode and motivation_brief. " +
			"When NOT to call: if another tool just returned needs_domain_setup=true (call init_domain first); do not call get_pending_alerts or get_metacognitive_mirror in the same turn unless the learner explicitly asks for those raw views. " +
			"Precondition: a domain must exist; otherwise needs_domain_setup=true is returned with a setup_domain activity. " +
			"Returns: {needs_domain_setup, session_id, domain_id, domain_name, intent, intent_status, activity, pedagogical_contract, consolidation_request, goal_relevance_status, session_concepts_done, metacognitive_mirror, tutor_mode, active_misconceptions, known_misconception_types, motivation_brief, mastery_evidence, mastery_uncertainty, transfer_profile, degraded_components?}.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params GetNextActivityParams) (*mcp.CallToolResult, any, error) {
		totalStart := time.Now()
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "get_next_activity", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		for _, f := range []struct {
			name  string
			value string
		}{
			{"domain_id", params.DomainID},
			{"domain_name", params.DomainName},
			{"intent", params.Intent},
		} {
			if err := validateString(f.name, f.value, maxShortLabelLen); err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
		}
		intent, err := normalizeActivityIntent(params.Intent)
		if err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}

		// Check if domain exists
		domainStart := time.Now()
		domain, err := resolveActivityDomain(ctx, deps.Store, learnerID, params.DomainID, params.DomainName)
		domainMs := time.Since(domainStart).Milliseconds()
		if err != nil || domain == nil {
			if params.DomainName != "" {
				msg := "domain not found"
				if err != nil {
					msg = err.Error()
				}
				r, _ := errorResult(msg)
				return r, nil, nil
			}
			r, _ := jsonResult(map[string]interface{}{
				"needs_domain_setup": true,
				"activity": models.Activity{
					Type:         models.ActivitySetupDomain,
					Rationale:    "no domain configured",
					PromptForLLM: "The learner has no domain yet. Analyse their objective, break it down into concepts, and call init_domain().",
				},
			})
			return r, nil, nil
		}

		prefetchStart := time.Now()
		now := time.Now().UTC()
		learningSession, err := deps.Store.OpenLearningSession(ctx, learnerID, domain.ID, "", now)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to open learning session", err)
			return r, nil, nil
		}
		states, err := deps.Store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load concept states", err)
			return r, nil, nil
		}
		interactions, err := deps.Store.GetRecentInteractionsByDomain(
			ctx, learnerID, domain.ID, engine.DefaultRecentInteractionsWindow,
		)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load recent interactions", err)
			return r, nil, nil
		}
		sessionStart, err := deps.Store.GetSessionStart(ctx, learnerID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load session state", err)
			return r, nil, nil
		}

		// Get session interactions to track what was already practiced
		sessionInteractions, err := deps.Store.GetInteractionsBySessionInDomain(ctx, learnerID, learningSession.ID, domain.ID)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load session interactions", err)
			return r, nil, nil
		}

		// Filter states to only those in the current domain
		domainConcepts := make(map[string]bool)
		for _, c := range domain.Graph.Concepts {
			domainConcepts[c] = true
		}
		var domainStates []*models.ConceptState
		for _, cs := range states {
			if domainConcepts[cs.Concept] {
				domainStates = append(domainStates, cs)
			}
		}

		// Filter interactions to domain concepts
		var domainInteractions []*models.Interaction
		for _, i := range interactions {
			if domainConcepts[i.Concept] {
				domainInteractions = append(domainInteractions, i)
			}
		}

		// Compute alerts from the same attempt-linked retention and trusted
		// transfer evidence used by check_mastery.
		evidenceSnapshot, err := engine.LoadEvidenceSnapshot(
			ctx, deps.Store, learnerID, domain.ID, domain.Graph.Concepts,
		)
		if err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to load mastery alert evidence", err)
			return r, nil, nil
		}
		alertEvidence := evidenceSnapshot.ForMasteryAlerts(domainStates)
		alerts := engine.ComputeAlertsWithEvidenceAt(domainStates, domainInteractions, alertEvidence, sessionStart, now)

		// Build set of concepts already practiced in this session
		sessionConcepts := make(map[string]int)
		for _, i := range sessionInteractions {
			if domainConcepts[i.Concept] {
				sessionConcepts[i.Concept]++
			}
		}
		prefetchMs := time.Since(prefetchStart).Milliseconds()
		extra := map[string]any{}
		var degradedComponents []string
		markDegraded := func(component string, err error) {
			degradedComponents = append(degradedComponents, component)
			deps.Logger.Warn("get_next_activity: optional component degraded",
				"component", component, "err", err, "learner", learnerID, "domain", domain.ID)
		}

		// Route to next activity through the regulation pipeline, or through
		// the explicit review override when the learner asks to revise.
		orchestratorStart := time.Now()
		var activity models.Activity
		var orchPhase models.Phase
		var orchErr error
		intentStatus := "auto"
		route := "orchestrator"
		input := engine.OrchestratorInput{
			LearnerID:        learnerID,
			DomainID:         domain.ID,
			Now:              now,
			SessionStart:     sessionStart,
			Config:           engine.NewDefaultPhaseConfig(),
			Logger:           deps.Logger,
			EvidenceSnapshot: evidenceSnapshot,
		}
		if intent == activityIntentReview {
			input.ReviewOnly = true
			intentStatus = "applied"
			route = "orchestrator_review"
		}
		// OrchestrateWithPhase returns the post-orchestrate phase so we
		// can audit-log it without re-reading the domain row (perf #91).
		activity, orchPhase, orchErr = engine.OrchestrateWithPhase(ctx, deps.Store, input)
		if input.ReviewOnly && strings.Contains(activity.Rationale, "no_reviewable_concept") {
			intentStatus = "no_reviewable_concept"
		}
		orchestratorMs := time.Since(orchestratorStart).Milliseconds()
		if orchErr != nil {
			deps.Logger.Error("orchestrator failed", "err", orchErr, "learner", learnerID, "domain", domain.ID)
			r, _ := errorResult("could not compute next activity")
			return r, nil, nil
		}
		pushSince := now.Add(-7 * 24 * time.Hour)
		activeWebhookNudge, err := deps.Store.GetLatestOpenWebhookPush(ctx, learnerID, domain.ID, pushSince)
		if err != nil {
			markDegraded("active_webhook_nudge", err)
		}
		if err := deps.Store.MarkWebhookPushSessionOpened(ctx, learnerID, now, pushSince); err != nil {
			markDegraded("webhook_session_tracking", err)
		}
		if activeWebhookNudge != nil {
			extra["active_webhook_nudge"] = activeWebhookNudge
			if activeWebhookNudge.OpenLoop != "" || activeWebhookNudge.NextAction != "" {
				activity.PromptForLLM += fmt.Sprintf(
					"\nThe learner may be returning from a Discord nudge. Start by reconnecting to this learner-facing open loop without naming internal tools: %s %s",
					activeWebhookNudge.OpenLoop,
					activeWebhookNudge.NextAction,
				)
			}
		}
		overrideResult := LearningNegotiationOverrideConsumeResult{Status: LearningNegotiationOverrideConsumeNone}
		if overrideActivity, consumed, err := ConsumeLearningNegotiationOverride(ctx, deps.Store, learnerID, domain, activity, alerts, now); err != nil {
			markDegraded("learning_negotiation_override", err)
		} else {
			overrideResult = consumed
			if consumed.Status == LearningNegotiationOverrideConsumeConsumed {
				activity = overrideActivity
				route = "negotiation_override"
			}
		}

		// Pipeline decision audit — one line per get_next_activity call.
		// Phase comes straight from the orchestrator (any FSM transition
		// or NoFringe fallback already applied and persisted). The
		// orchestrator now fails the tool call if a transition cannot be
		// stored, so this audit line cannot describe a phase that diverges
		// from the database.
		loggedPhase := "INSTRUCTION" // orchestrator's NULL fallback
		if orchPhase != "" {
			loggedPhase = string(orchPhase)
		}
		deps.Logger.Debug("pipeline decision",
			"learner", learnerID,
			"session", learningSession.ID,
			"domain", domain.ID,
			"route", route,
			"phase", loggedPhase,
			"intent_status", intentStatus,
			"negotiation_override_status", overrideResult.Status,
			"activity_type", activity.Type,
			"concept", activity.Concept,
		)

		// Metacognitive mirror
		mirrorStart := time.Now()
		since := time.Now().UTC().Add(-7 * 24 * time.Hour)
		allInteractions, err := deps.Store.GetInteractionsSinceInDomain(ctx, learnerID, domain.ID, since)
		if err != nil {
			// The orchestrator may already have persisted a phase transition and
			// the one-shot negotiation override may already have been consumed.
			// Enrichment reads after those effects must not turn the whole call
			// into an MCP error: the idempotency middleware would release the key
			// and a retry could no longer reproduce the selected activity.
			markDegraded("metacognitive_interactions", err)
			allInteractions = nil
		}
		calibBias, err := deps.Store.GetCalibrationBiasInDomain(ctx, learnerID, domain.ID, 20)
		if err != nil {
			markDegraded("calibration_state", err)
			calibBias = 0
		}
		calibHistory, err := deps.Store.GetCalibrationBiasHistoryInDomain(ctx, learnerID, domain.ID, 20)
		if err != nil {
			markDegraded("calibration_evidence", err)
			calibHistory = nil
		}
		affects, err := deps.Store.GetRecentAffectStates(ctx, learnerID, 10)
		if err != nil {
			markDegraded("affect_state", err)
			affects = nil
		}

		var autonomyScores []float64
		for _, a := range affects {
			autonomyScores = append(autonomyScores, a.AutonomyScore)
		}
		mirrorSessionCount := len(engine.GroupIntoSessionsExported(allInteractions, 2*time.Hour))

		mirror := engine.DetectMirrorPattern(engine.MirrorInput{
			Interactions:       allInteractions,
			ConceptStates:      domainStates,
			AutonomyScores:     autonomyScores,
			CalibrationBias:    calibBias,
			CalibrationSamples: len(calibHistory),
			SessionCount:       mirrorSessionCount,
		})

		// Persist & enqueue the mirror so it can be pushed proactively via
		// the webhook queue (#59). Per-day dedup lives in EnqueueMirrorWebhook
		// so a learner who hits get_next_activity multiple times in a day
		// only sees one queued nudge.
		if mirror != nil {
			if _, _, err := engine.EnqueueMirrorWebhook(ctx, deps.Store, learnerID, mirror, time.Now().UTC()); err != nil {
				deps.Logger.Warn("get_next_activity: mirror enqueue failed", "err", err, "learner", learnerID)
			}
		}
		mirrorMs := time.Since(mirrorStart).Milliseconds()

		// Tutor mode
		var currentAffect *models.AffectState
		if len(affects) > 0 {
			currentAffect = affects[0]
		}
		tutorMode := engine.ComputeTutorMode(currentAffect, alerts)

		// Apply tutor_mode adjustments to activity difficulty
		switch tutorMode {
		case "lighter":
			activity.DifficultyTarget *= 0.7
			activity.EstimatedMinutes = int(float64(activity.EstimatedMinutes) * 0.6)
			if activity.EstimatedMinutes < 5 {
				activity.EstimatedMinutes = 5
			}
		case "scaffolding":
			activity.DifficultyTarget *= 0.75
		}

		// Apply calibration bias adjustment
		// Positive bias = over-estimates → increase difficulty
		// Negative bias = under-estimates → decrease difficulty
		if calibBias != 0 {
			activity.DifficultyTarget += calibBias * 0.1
			if activity.DifficultyTarget < 0.3 {
				activity.DifficultyTarget = 0.3
			}
			if activity.DifficultyTarget > 0.85 {
				activity.DifficultyTarget = 0.85
			}
		}

		// Misconception enrichment for selected concept
		enrichmentStart := time.Now()
		var activeMisconceptions any = []any{}
		var knownMisconceptionTypes any = []string{}
		activeMisconceptionCount := 0

		if activity.Concept != "" {
			if active, err := deps.Store.GetActiveMisconceptionsInDomain(ctx, learnerID, domain.ID, activity.Concept); err != nil {
				markDegraded("active_misconceptions", err)
			} else if len(active) > 0 {
				activeMisconceptions = active
				activeMisconceptionCount = len(active)

				// Inject misconceptions into prompt
				misconceptionPrompt := fmt.Sprintf("\nWARNING: the learner has %d active misconception(s): ", len(active))
				for i, m := range active {
					if i > 0 {
						misconceptionPrompt += " ; "
					}
					misconceptionPrompt += m.MisconceptionType
					if m.LastErrorDetail != "" {
						misconceptionPrompt += " - " + m.LastErrorDetail
					}
				}
				misconceptionPrompt += ". Target these confusions in your explanation and exercise. Do not explicitly mention the misconceptions - design the exercise so the learner confronts them naturally."
				activity.PromptForLLM += misconceptionPrompt
			}

			if types, err := deps.Store.GetDistinctMisconceptionTypesInDomain(ctx, learnerID, domain.ID, activity.Concept); err != nil {
				markDegraded("known_misconception_types", err)
			} else if len(types) > 0 {
				knownMisconceptionTypes = types
			}
		}
		enrichmentMs := time.Since(enrichmentStart).Milliseconds()

		// Motivation layer — compose a context-adaptive brief for this activity.
		// Detect plateau active on the chosen concept from the alerts list.
		motivationStart := time.Now()
		plateauActive := false
		for _, a := range alerts {
			if a.Type == models.AlertPlateau && a.Concept == activity.Concept {
				plateauActive = true
				break
			}
		}
		motivationEngine := engine.NewMotivationEngine(deps.Store)
		motivationBrief, motivationErr := motivationEngine.Build(
			ctx, learnerID, domain, activity.Concept, activity.Type,
			plateauActive, len(sessionConcepts),
		)
		if motivationErr != nil {
			markDegraded("motivation_brief", motivationErr)
		}
		motivationMs := time.Since(motivationStart).Milliseconds()

		// [6] FadeController — post-decision module gated on
		// REGULATION_FADE=on (default OFF). Maps autonomy
		// score+trend to handover params (verbosity, webhook
		// cadence, ZPD aggressiveness, proactive review). When
		// the flag is OFF, the result JSON is byte-identical to
		// the pre-fade behaviour: no fade_params key, no mutation
		// of motivation_brief. See
		// docs/regulation-design/06-fade-controller.md.
		if regulationFadeEnabled() {
			autonomyMetrics := engine.ComputeAutonomyMetrics(engine.AutonomyInput{
				Interactions:    allInteractions,
				ConceptStates:   domainStates,
				CalibrationBias: calibBias,
				SessionGap:      2 * time.Hour,
			})
			trend := engine.AutonomyTrend(engine.ComputeAutonomyTrendExported(autonomyScores))
			fadeParams := engine.Decide(autonomyMetrics.Score, trend)
			motivationBrief = applyFadeToMotivation(motivationBrief, fadeParams.HintLevel)
			extra["fade_params"] = fadeParams
			extra["autonomy_score"] = autonomyMetrics.Score
			extra["autonomy_trend"] = string(trend)
		}

		diagnosticsStart := time.Now()
		var masteryEvidence any = map[string]any{}
		var masteryUncertainty any = map[string]any{}
		var transferProfile any = map[string]any{}
		var selectedState *models.ConceptState
		var evidenceQuality engine.EvidenceQualityAssessment
		var uncertainty engine.MasteryUncertainty
		var typedTransferProfile engine.TransferProfile
		if activity.Concept != "" {
			for _, cs := range domainStates {
				if cs.Concept == activity.Concept {
					selectedState = cs
					break
				}
			}
			conceptInteractions, err := deps.Store.GetRecentInteractionsInDomain(ctx, learnerID, domain.ID, activity.Concept, 50)
			if err != nil {
				markDegraded("mastery_diagnostics", err)
			} else {
				now := time.Now().UTC()
				profile := engine.BuildEvidenceProfile(learnerID, activity.Concept, conceptInteractions, now)
				evidenceQuality = engine.MasteryEvidenceQuality(profile)
				masteryEvidence = map[string]any{
					"profile": profile,
					"quality": evidenceQuality,
				}
				uncertainty = engine.ComputeMasteryUncertainty(selectedState, conceptInteractions, engine.MasteryEvidenceProfile{Now: now})
				masteryUncertainty = uncertainty
			}
			conceptEvidence := evidenceSnapshot.ForConcept(activity.Concept)
			typedTransferProfile = engine.BuildTransferProfile(activity.Concept, conceptEvidence.Transfers)
			transferProfile = typedTransferProfile
		}
		if decision := engine.ApplyEvidenceController(engine.EvidenceControllerInput{
			Activity:           activity,
			ConceptState:       selectedState,
			EvidenceQuality:    evidenceQuality,
			MasteryUncertainty: uncertainty,
			TransferProfile:    typedTransferProfile,
		}); decision.Adjusted {
			activity = decision.Activity
			extra["evidence_adjustment"] = decision.Rationale
			if active, err := deps.Store.GetActiveMisconceptionsInDomain(ctx, learnerID, domain.ID, activity.Concept); err != nil {
				markDegraded("adjusted_activity_misconceptions", err)
			} else if len(active) > 0 {
				activeMisconceptions = active
				activeMisconceptionCount = len(active)
				misconceptionPrompt := fmt.Sprintf("\nWARNING: the learner has %d active misconception(s): ", len(active))
				for i, m := range active {
					if i > 0 {
						misconceptionPrompt += " ; "
					}
					misconceptionPrompt += m.MisconceptionType
					if m.LastErrorDetail != "" {
						misconceptionPrompt += " - " + m.LastErrorDetail
					}
				}
				misconceptionPrompt += ". Target these confusions in your explanation and exercise. Do not explicitly mention the misconceptions - design the exercise so the learner confronts them naturally."
				activity.PromptForLLM += misconceptionPrompt
			}
		}
		diagnosticsMs := time.Since(diagnosticsStart).Milliseconds()
		if len(degradedComponents) > 0 {
			extra["degraded_components"] = degradedComponents
		}
		goalRelevanceStatus := buildGoalRelevanceStatus(domain)
		episodicContext, olmSnapshot := loadEpisodicContextForActivity(
			ctx, deps, learnerID, domain, domainStates, activity.Concept, alerts, now, evidenceSnapshot,
		)
		contract := buildPedagogicalContract(activity, intent, evidenceQuality, uncertainty, typedTransferProfile, goalRelevanceStatus, extra["fade_params"], episodicContext, activeMisconceptionCount, olmSnapshot)

		out := map[string]any{
			"needs_domain_setup":        false,
			"session_id":                learningSession.ID,
			"domain_id":                 domain.ID,
			"domain_name":               domain.Name,
			"intent":                    intent,
			"intent_status":             intentStatus,
			"activity":                  activity,
			"session_concepts_done":     len(sessionConcepts),
			"metacognitive_mirror":      mirror,
			"tutor_mode":                tutorMode,
			"active_misconceptions":     activeMisconceptions,
			"known_misconception_types": knownMisconceptionTypes,
			"motivation_brief":          motivationBrief,
			"mastery_evidence":          masteryEvidence,
			"mastery_uncertainty":       masteryUncertainty,
			"transfer_profile":          transferProfile,
			"goal_relevance_status":     goalRelevanceStatus,
			"pedagogical_contract":      contract,
			"audit_rationale":           contract.AuditRationale,
			"llm_instruction":           contract.LLMInstruction,
			"learner_explanation":       contract.LearnerExplanation,
		}
		if episodicContextVisible(episodicContext) {
			out["episodic_context"] = episodicContext
		}
		if consolidationRequest := maybeBuildConsolidationRequest(ctx, deps, learnerID, now); consolidationRequest != nil {
			out["consolidation_request"] = consolidationRequest
		}
		if overrideResult.Status != LearningNegotiationOverrideConsumeNone {
			out["learning_negotiation_override"] = overrideResult
		}
		for k, v := range extra {
			out[k] = v
		}
		jsonStart := time.Now()
		r, _ := jsonResult(out)
		jsonMs := time.Since(jsonStart).Milliseconds()
		if deps.Logger.Enabled(ctx, slog.LevelDebug) {
			deps.Logger.Debug("get_next_activity timings",
				"learner", learnerID,
				"domain", domain.ID,
				"concepts", len(domain.Graph.Concepts),
				"domain_ms", domainMs,
				"prefetch_ms", prefetchMs,
				"orchestrator_ms", orchestratorMs,
				"mirror_ms", mirrorMs,
				"enrichment_ms", enrichmentMs,
				"motivation_ms", motivationMs,
				"diagnostics_ms", diagnosticsMs,
				"json_ms", jsonMs,
				"total_ms", time.Since(totalStart).Milliseconds(),
			)
		}
		return r, nil, nil
	})
}

// applyFadeToMotivation modulates a MotivationBrief based on the fade
// HintLevel. The contract:
//
//   - HintLevelFull    : brief returned unchanged (legacy behaviour).
//   - HintLevelPartial : Instruction is collapsed to a one-line
//     concise form; structured fields (ValueFraming, ProgressDelta,
//     etc.) are preserved so the LLM still has the context to weave
//     in if it chooses, but the explicit phrasing guidance is shorter.
//   - HintLevelNone    : Kind is cleared and Instruction is emptied.
//     Per the system prompt, kind == "" means "no motivational
//     preamble". Structured fields are also cleared to make the
//     suppression unambiguous on the wire.
//
// Returns brief unchanged if it's nil or already silent (kind == "").
func applyFadeToMotivation(brief *models.MotivationBrief, level engine.HintLevel) *models.MotivationBrief {
	if brief == nil {
		return brief
	}
	switch level {
	case engine.HintLevelNone:
		return &models.MotivationBrief{Kind: "", Instruction: ""}
	case engine.HintLevelPartial:
		if brief.Kind == "" {
			return brief
		}
		// Replace the verbose tutor-direction Instruction with a
		// terse one-liner. The kind + structured fields stay so
		// downstream context is preserved.
		clone := *brief
		clone.Instruction = "Keep it brief and minimal."
		return &clone
	default: // HintLevelFull
		return brief
	}
}

func loadEpisodicContextForActivity(
	ctx context.Context,
	deps *Deps,
	learnerID string,
	domain *models.Domain,
	domainStates []*models.ConceptState,
	focusConcept string,
	alerts []models.Alert,
	now time.Time,
	evidenceSnapshot *engine.EvidenceSnapshot,
) (*memory.EpisodicContext, *engine.OLMSnapshot) {
	if !memory.Enabled() {
		return nil, nil
	}
	var olmSnapshot *engine.OLMSnapshot
	if snap, err := engine.BuildOLMSnapshotAtWithEvidence(
		ctx, deps.Store, learnerID, domain.ID, now, evidenceSnapshot,
	); err == nil {
		olmSnapshot = snap
	} else if deps.Logger != nil {
		deps.Logger.Warn("get_next_activity: OLM snapshot for memory context failed", "err", err, "learner", learnerID, "domain", domain.ID)
	}
	view := &memory.OLMView{
		FocusConcept:  focusConcept,
		ConceptBucket: buildMemoryConceptBuckets(domain, domainStates),
	}
	if olmSnapshot != nil && olmSnapshot.FocusConcept != "" {
		view.FocusConcept = olmSnapshot.FocusConcept
	}
	ec, err := memory.LoadContextForDomain(learnerID, domain.ID, focusConcept, view, alerts)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("get_next_activity: episodic context load failed", "err", err, "learner", learnerID)
		}
		return nil, olmSnapshot
	}
	return ec, olmSnapshot
}

func buildMemoryConceptBuckets(domain *models.Domain, states []*models.ConceptState) map[string]string {
	out := map[string]string{}
	stateByConcept := map[string]*models.ConceptState{}
	for _, cs := range states {
		if cs != nil {
			stateByConcept[cs.Concept] = cs
		}
	}
	if domain == nil {
		return out
	}
	for _, concept := range domain.Graph.Concepts {
		out[concept] = string(engine.NodeClassify(stateByConcept[concept]))
	}
	return out
}

func episodicContextVisible(ec *memory.EpisodicContext) bool {
	if ec == nil {
		return false
	}
	return strings.TrimSpace(ec.LearnerMemory) != "" ||
		strings.TrimSpace(ec.PendingMemory) != "" ||
		strings.TrimSpace(ec.ConceptNotes) != "" ||
		len(ec.RecentSessions) > 0 ||
		len(ec.RecentArchives) > 0 ||
		len(ec.OLMInconsistencies) > 0
}

func buildGoalRelevanceStatus(domain *models.Domain) models.GoalRelevanceStatus {
	gr := domain.ParseGoalRelevance()
	if gr == nil {
		return models.GoalRelevanceStatus{
			Status:          "missing",
			Message:         "Goal-aware routing is using uniform relevance because no relevance vector exists.",
			RecommendedTool: "set_goal_relevance",
			MissingConcepts: append([]string(nil), domain.Graph.Concepts...),
		}
	}
	missing := domain.UncoveredConcepts()
	if len(missing) > 0 {
		return models.GoalRelevanceStatus{
			Status:          "partial",
			Message:         "Goal-aware routing has a relevance vector, but some domain concepts are not covered.",
			RecommendedTool: "set_goal_relevance",
			MissingConcepts: missing,
			Stale:           domain.IsGoalRelevanceStale(),
		}
	}
	if domain.IsGoalRelevanceStale() {
		return models.GoalRelevanceStatus{
			Status:          "stale",
			Message:         "Goal-aware routing has a complete relevance vector, but it was set for an older graph version.",
			RecommendedTool: "set_goal_relevance",
			Stale:           true,
		}
	}
	return models.GoalRelevanceStatus{
		Status:  "valid",
		Message: "Goal-aware routing has a complete relevance vector for the current graph.",
	}
}

func buildPedagogicalContract(
	activity models.Activity,
	intent string,
	evidenceQuality engine.EvidenceQualityAssessment,
	uncertainty engine.MasteryUncertainty,
	transferProfile engine.TransferProfile,
	goalStatus models.GoalRelevanceStatus,
	fadeValue any,
	episodicContext *memory.EpisodicContext,
	activeMisconceptionCount int,
	olmSnapshot *engine.OLMSnapshot,
) models.PedagogicalContract {
	constraints := models.PedagogicalConstraints{
		MustCollect: []string{"learner_answer", "rubric_score"},
		Avoid: []string{
			"introducing_new_prerequisite",
			"long_explanation_first",
			"marking_mastery_without_evidence",
		},
	}
	if activity.Type == models.ActivityDiagnosticAssessment {
		constraints.MustCollect = append(constraints.MustCollect, "cold_response_before_feedback")
		constraints.Avoid = append(constraints.Avoid,
			"teaching_before_response",
			"hints_before_response",
			"worked_example_before_response",
		)
	}
	if activity.Type == models.ActivityTransferProbe {
		constraints.MustCollect = append(constraints.MustCollect, "transfer_dimension", "transfer_score")
	}
	if goalStatus.Status == "missing" || goalStatus.Status == "partial" || goalStatus.Status == "stale" {
		constraints.Avoid = append(constraints.Avoid, "assuming_goal_relevance_is_complete")
	}
	if evidenceQuality.Quality == engine.EvidenceQualityWeak || uncertainty.ConfidenceLabel == engine.MasteryConfidenceLow {
		constraints.MustCollect = append(constraints.MustCollect, "reasoning_trace")
	}
	if transferProfile.ReadinessLabel == engine.TransferReadinessBlocked {
		constraints.Avoid = append(constraints.Avoid, "jumping_to_mastery_challenge")
	}

	contract := models.PedagogicalContract{
		Intent:                  contractIntent(intent, activity),
		TargetConcept:           activity.Concept,
		RecommendedActivityType: activity.Type,
		Constraints:             constraints,
		AllowedVariants:         allowedVariants(activity.Type),
		LLMDiscretion: models.PedagogicalLLMDiscretion{
			CanChangeFormat:                  activity.Type != models.ActivityCloseSession,
			CanRequestClarification:          true,
			CanProposeNegotiation:            activity.Type != models.ActivityCloseSession,
			CannotMarkMasteryWithoutEvidence: true,
		},
		FadeGuidance:       fadeGuidance(fadeValue),
		EpisodicContext:    contractEpisodicContext(episodicContext),
		LearnerExplanation: learnerExplanation(activity),
		AuditRationale:     activity.Rationale,
		LLMInstruction:     activity.PromptForLLM,
	}
	if episodicContextVisible(episodicContext) {
		contract.LLMInstruction = strings.TrimSpace(contract.LLMInstruction + "\n\n" + memory.EpisodicContextInstruction)
	}
	contract.ReasoningRequest = buildReasoningRequest(activity, episodicContext, activeMisconceptionCount, olmSnapshot, uncertainty)
	return contract
}

func contractEpisodicContext(ec *memory.EpisodicContext) any {
	if !episodicContextVisible(ec) {
		return nil
	}
	return ec
}

func buildReasoningRequest(
	activity models.Activity,
	episodicContext *memory.EpisodicContext,
	activeMisconceptionCount int,
	olmSnapshot *engine.OLMSnapshot,
	uncertainty engine.MasteryUncertainty,
) *models.ReasoningRequest {
	if skipReasoningActivity(activity.Type) {
		return nil
	}
	var reasons []string
	if episodicContext != nil && (strings.TrimSpace(episodicContext.LearnerMemory) != "" || len(episodicContext.RecentSessions) > 0) {
		reasons = append(reasons, "episodic_context_available")
	}
	if activeMisconceptionCount > 0 {
		reasons = append(reasons, "active_misconceptions_present")
	}
	if olmSnapshot != nil && (olmSnapshot.AutonomyTrend == "declining" || olmSnapshot.AffectTrend == "declining") {
		reasons = append(reasons, "metacognitive_signal_negative")
	}
	if uncertainty.ConfidenceLabel == engine.MasteryConfidenceLow {
		reasons = append(reasons, "high_uncertainty_on_target")
	}
	if episodicContext != nil && strings.TrimSpace(episodicContext.PendingMemory) != "" {
		reasons = append(reasons, "pending_observations_to_arbitrate")
	}
	if episodicContext != nil && len(episodicContext.OLMInconsistencies) > 0 {
		reasons = append(reasons, "olm_inconsistencies_to_compensate")
	}
	if len(reasons) == 0 {
		return nil
	}
	return &models.ReasoningRequest{
		Task:           memory.ReasoningRequestTask,
		Constraints:    append([]string(nil), memory.ReasoningRequestConstraints...),
		OutputRequired: "interpretation_brief+activity",
		EnabledBecause: reasons,
	}
}

func skipReasoningActivity(t models.ActivityType) bool {
	switch t {
	case models.ActivityRest, models.ActivitySetupDomain, models.ActivityCloseSession:
		return true
	default:
		return false
	}
}

func contractIntent(intent string, activity models.Activity) string {
	if intent == activityIntentReview {
		return "review_prior_material"
	}
	switch activity.Type {
	case models.ActivityCloseSession:
		return "close_overloaded_session"
	case models.ActivityRecall:
		return "stabilize_retention"
	case models.ActivityDiagnosticAssessment:
		return "measure_prior_knowledge_without_instruction"
	case models.ActivityPractice:
		return "build_reliable_skill"
	case models.ActivityDebugMisconception:
		return "repair_misconception"
	case models.ActivityFeynmanPrompt:
		return "strengthen_explanation"
	case models.ActivityTransferProbe:
		return "test_transfer"
	case models.ActivityMasteryChallenge:
		return "verify_mastery"
	case models.ActivitySetupDomain:
		return "initialize_domain"
	default:
		return "continue_learning"
	}
}

func allowedVariants(t models.ActivityType) []string {
	switch t {
	case models.ActivityCloseSession:
		return []string{"recap_brief", "next_session_intention"}
	case models.ActivityRecall:
		return []string{"retrieval_prompt", "cloze_recall", "short_application"}
	case models.ActivityDiagnosticAssessment:
		return []string{"short_constructed_response", "cold_application", "misconception_probe"}
	case models.ActivityDebugMisconception:
		return []string{"contrastive_example", "micro_debug", "socratic_prompt"}
	case models.ActivityFeynmanPrompt:
		return []string{"teach_back", "analogy_check", "gap_explanation"}
	case models.ActivityTransferProbe:
		return []string{"near_transfer", "far_transfer", "novel_context_application"}
	case models.ActivityMasteryChallenge:
		return []string{"integrated_performance_task", "explain_then_apply", "rubric_scored_task"}
	default:
		return []string{"socratic_prompt", "worked_example_completion", "micro_practice"}
	}
}

func learnerExplanation(activity models.Activity) string {
	switch activity.Type {
	case models.ActivityCloseSession:
		return "We will close this session with a short recap and a clear next step."
	case models.ActivityRecall:
		return "We will refresh this concept so it stays available when you need it."
	case models.ActivityDiagnosticAssessment:
		return "We will first check what you can already do, without teaching or hints before your answer."
	case models.ActivityDebugMisconception:
		return "We will focus on a likely confusion and resolve it through a targeted example."
	case models.ActivityFeynmanPrompt:
		return "We will make the idea easier to explain, which usually makes it easier to reuse."
	case models.ActivityTransferProbe:
		return "We will check whether this idea transfers beyond the first context."
	case models.ActivityMasteryChallenge:
		return "We will verify that the concept is solid with a more complete challenge."
	case models.ActivitySetupDomain:
		return "We need to set up the learning domain before choosing an activity."
	default:
		return "We will consolidate this concept before moving to the next step."
	}
}

func fadeGuidance(value any) *models.PedagogicalFadeGuidance {
	var params engine.FadeParams
	switch v := value.(type) {
	case engine.FadeParams:
		params = v
	case *engine.FadeParams:
		if v == nil {
			return nil
		}
		params = *v
	default:
		return nil
	}

	instruction := "Use normal scaffolding."
	switch params.HintLevel {
	case engine.HintLevelFull:
		instruction = "Offer explicit scaffolding and check understanding before increasing difficulty."
	case engine.HintLevelPartial:
		instruction = "Keep scaffolding concise and let the learner make the next move."
	case engine.HintLevelNone:
		instruction = "Avoid unsolicited hints; ask only brief clarifying questions when needed."
	}
	return &models.PedagogicalFadeGuidance{
		HintLevel:              string(params.HintLevel),
		WebhookFrequency:       string(params.WebhookFrequency),
		ZPDAggressiveness:      string(params.ZPDAggressiveness),
		ProactiveReviewEnabled: params.ProactiveReviewEnabled,
		Instruction:            instruction,
	}
}
