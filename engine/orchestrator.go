// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package engine — [2] PhaseController orchestrator (runtime).
//
// Orchestrate is the runtime entry point for the regulation pipeline.
// It :
//
//  1. Reads the current phase from the store (NULL → INSTRUCTION
//     fallback per OQ-2.1.b).
//  2. Pre-fetches the observables (states, alerts, recent concepts,
//     active misconceptions, goal_relevance) and computes the mean
//     binary entropy of P(L).
//  3. Evaluates the FSM (pure call to EvaluatePhase).
//  4. Persists the transition if any.
//  5. Runs the Gate → ConceptSelector → ActionSelector pipeline
//     with one-shot retry on NoFringe (the FSM is re-evaluated and
//     the pipeline retried once).
//  6. Returns a models.Activity with concept and prompt composed.
//
// The function takes a *db.Store (impure) but its sub-helpers
// (EvaluatePhase, runPipeline, etc.) are pure functions tested in
// isolation. Layered design supports unit + integration testing.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// ErrUnknownDomain is returned when the orchestrator cannot find the
// domain referenced by OrchestratorInput.DomainID.
var ErrUnknownDomain = errors.New("orchestrator: unknown domain")

// OrchestratorInput carries the read-only context one Orchestrate
// call needs. The Store and time are explicit dependencies (not
// globals) for testability.
type OrchestratorInput struct {
	LearnerID string
	DomainID  string
	// Now is the wall-clock used for phase_changed_at and any time
	// arithmetic. Tests pass deterministic timestamps.
	Now time.Time
	// SessionStart is the start timestamp of the active learning
	// session. It is distinct from Now: OVERLOAD alerts compare the
	// current clock to the session start.
	SessionStart time.Time
	// ReviewOnly biases the pipeline toward previously studied material
	// while preserving Gate and ActionSelector constraints.
	ReviewOnly bool
	// Config is injected — typically NewDefaultPhaseConfig() in
	// production. Tests can pass narrower configs to drive specific
	// scenarios.
	Config PhaseConfig
	// Logger receives FSM and persistence diagnostics. Nil disables
	// orchestrator logging for callers that have not opted in.
	Logger *slog.Logger
}

// orchestratorMaxRetries is the upper bound on FSM-driven pipeline
// retries inside a single Orchestrate call. Set to 1 (one retry on
// NoFringe — see §4 of the design doc). Hard-coded because the
// retry is structural, not tunable.
const orchestratorMaxRetries = 1

// Orchestrate runs the full regulation pipeline for one
// get_next_activity call. Returns a models.Activity ready for the
// LLM-side post-processing (tutor_mode, calibration_bias,
// motivation_brief — handled by tools/activity.go as today).
//
// Thin wrapper around OrchestrateWithPhase for callers that don't
// need the post-orchestrate phase (e.g. learning_negotiation).
func Orchestrate(ctx context.Context, store storeport.Store, input OrchestratorInput) (models.Activity, error) {
	activity, _, err := OrchestrateWithPhase(ctx, store, input)
	return activity, err
}

// OrchestrateWithPhase is identical to Orchestrate but also returns the
// phase the orchestrator settled on (after FSM transition and any
// NoFringe fallback). This lets callers — notably get_next_activity —
// observe the post-orchestrate phase without re-reading the domain
// from the DB. The returned phase is the one the activity was
// produced under and matches what was persisted by
// store.UpdateDomainPhase during the call.
//
// On error, the returned phase is the empty string ("").
func OrchestrateWithPhase(ctx context.Context, store storeport.Store, input OrchestratorInput) (models.Activity, models.Phase, error) {
	logger := input.logger()
	domain, err := store.GetDomainByID(ctx, input.DomainID)
	if err != nil {
		return models.Activity{}, "", fmt.Errorf("%w: %q: %v", ErrUnknownDomain, input.DomainID, err)
	}

	// 1. Read current phase ; NULL → INSTRUCTION fallback (OQ-2.1.b).
	currentPhase := domain.Phase
	if currentPhase == "" {
		currentPhase = models.PhaseInstruction
	}

	// 2. Fetch observables (states, recent, misconceptions, alerts).
	pf, err := fetchPipelineFixtures(ctx, store, domain, input)
	if err != nil {
		return models.Activity{}, "", err
	}

	// 3. Build PhaseObservables and evaluate the FSM. Review requests
	// stay inside the pipeline but deliberately bias phase selection
	// toward maintenance instead of advancing the domain FSM.
	fsmTransitioned := false
	if input.ReviewOnly {
		currentPhase = models.PhaseMaintenance
	} else {
		obs := buildObservables(domain, pf, input.Config, input.Now)
		eval := EvaluatePhase(currentPhase, obs, input.Config)
		fsmTransitioned = eval.Transitioned
		if fsmTransitioned {
			entryEntropy := 0.0
			if eval.To == models.PhaseDiagnostic {
				entryEntropy = obs.MeanEntropy
			}
			logger.Info("phase transition (FSM)",
				"domain", domain.ID, "from", eval.From, "to", eval.To,
				"entry_entropy", entryEntropy, "rationale", eval.Rationale)
			if persistErr := store.UpdateDomainPhase(ctx, domain.ID, eval.To, entryEntropy, input.Now); persistErr != nil {
				logger.Error("orchestrator: failed to persist phase transition",
					"domain", domain.ID, "from", eval.From, "to", eval.To, "err", persistErr)
				return models.Activity{}, "", fmt.Errorf("persist phase transition: %w", persistErr)
			}
			currentPhase = eval.To
		}
	}

	// 4. Run pipeline with one-shot retry on NoFringe.
	//
	// Important : if the FSM just transitioned this call, we do NOT
	// retry on NoFringe — that would risk *undoing* the transition
	// (e.g. MAINTENANCE → INSTRUCTION on retention drop, then INSTRUCTION
	// has no fringe because the concept is still BKT-mastered, then
	// retry sends us back to MAINTENANCE). The FSM decision wins ;
	// NoFringe in the new phase is reported via REST.
	for retry := 0; retry <= orchestratorMaxRetries; retry++ {
		activity, sig, err := runPipeline(ctx, store, domain, pf, currentPhase, input)
		if err != nil {
			return models.Activity{}, "", err
		}
		if !sig.IsNoFringe {
			return activity, currentPhase, nil
		}
		if input.ReviewOnly {
			break
		}
		if fsmTransitioned || retry >= orchestratorMaxRetries {
			break
		}
		// NoFringe — try a single phase fallback (only if FSM didn't
		// just decide).
		next := noFringeFallbackPhase(currentPhase)
		if next == currentPhase {
			break
		}
		logger.Info("phase fallback (NoFringe)",
			"domain", domain.ID, "from", currentPhase, "to", next, "retry", retry)
		if persistErr := store.UpdateDomainPhase(ctx, domain.ID, next, 0, input.Now); persistErr != nil {
			logger.Error("orchestrator: failed to persist NoFringe fallback transition",
				"domain", domain.ID, "from", currentPhase, "to", next, "err", persistErr)
			return models.Activity{}, "", fmt.Errorf("persist fallback phase transition: %w", persistErr)
		}
		currentPhase = next
	}

	if input.ReviewOnly {
		return models.Activity{
			Type:         models.ActivityRest,
			Rationale:    "[intent=review] no_reviewable_concept: no previously studied concept is available in this domain",
			PromptForLLM: "No reviewed concept is available in this domain. Tell the learner there is nothing to revise yet in this domain and ask whether they want to start a new concept.",
		}, currentPhase, nil
	}

	return models.Activity{
		Type:         models.ActivityRest,
		Rationale:    "pipeline_exhausted: NoFringe persists after retry",
		PromptForLLM: "No eligible activity remains after retry. Ask the learner what they want to work on next.",
	}, currentPhase, nil
}

func (input OrchestratorInput) logger() *slog.Logger {
	if input.Logger != nil {
		return input.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pipelineSignal lets runPipeline communicate "no candidate, please
// retry with a different phase" without dressing it up as an error
// (NoFringe is a *signal*, not a failure — cf. OQ-3.1).
type pipelineSignal struct {
	IsNoFringe bool
}

// pipelineFixtures bundles the data fetched once per Orchestrate call
// and reused across the FSM evaluation and the pipeline run.
type pipelineFixtures struct {
	StatesList         []*models.ConceptState
	StatesByConcept    map[string]*models.ConceptState
	GoalRelevance      map[string]float64 // nil if vector absent/parse-failed
	ActiveMisc         map[string]bool
	RecentConcepts     []string
	RecentInteractions []*models.Interaction
	Alerts             []models.Alert
	DiagnosticItems    int // count since phase_changed_at
}

func fetchPipelineFixtures(ctx context.Context, store storeport.Store, domain *models.Domain, input OrchestratorInput) (*pipelineFixtures, error) {
	states, err := store.GetConceptStatesByDomain(ctx, input.LearnerID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("get states: %w", err)
	}
	domainConcepts := make(map[string]bool, len(domain.Graph.Concepts))
	for _, c := range domain.Graph.Concepts {
		domainConcepts[c] = true
	}
	domainStates := make([]*models.ConceptState, 0, len(domain.Graph.Concepts))
	stateMap := make(map[string]*models.ConceptState, len(states))
	for _, cs := range states {
		if !domainConcepts[cs.Concept] {
			continue
		}
		domainStates = append(domainStates, cs)
		stateMap[cs.Concept] = cs
	}

	var goalRelevance map[string]float64
	if gr := domain.ParseGoalRelevance(); gr != nil {
		goalRelevance = gr.Relevance
	}

	activeMisc, err := store.GetActiveMisconceptionsBatchInDomain(ctx, input.LearnerID, domain.ID, domain.Graph.Concepts)
	if err != nil {
		return nil, fmt.Errorf("get active misconceptions: %w", err)
	}

	recent, err := store.GetRecentConceptsInDomain(ctx, input.LearnerID, domain.ID, 20)
	if err != nil {
		return nil, fmt.Errorf("get recent concepts: %w", err)
	}

	// Diagnostic items count since phase_changed_at — only meaningful
	// when current phase is DIAGNOSTIC, but cheap enough to always
	// fetch.
	var diagItems int
	if !domain.PhaseChangedAt.IsZero() {
		diagItems, err = store.CountInteractionsSinceInDomain(ctx, input.LearnerID, domain.ID, domain.PhaseChangedAt)
		if err != nil {
			return nil, fmt.Errorf("count diagnostic items: %w", err)
		}
	}

	// Alerts: re-derive from current state + recent interactions.
	// Match what the existing get_next_activity flow does.
	recentInteractions, err := store.GetRecentInteractionsByDomain(ctx, input.LearnerID, domain.ID, 50)
	if err != nil {
		return nil, fmt.Errorf("get recent interactions: %w", err)
	}
	domainInteractions := make([]*models.Interaction, 0, len(recentInteractions))
	for _, interaction := range recentInteractions {
		if domainConcepts[interaction.Concept] {
			domainInteractions = append(domainInteractions, interaction)
		}
	}
	sessionStart := input.SessionStart
	if sessionStart.IsZero() {
		var sessionErr error
		sessionStart, sessionErr = store.GetSessionStart(ctx, input.LearnerID)
		if sessionErr != nil {
			return nil, fmt.Errorf("get session start: %w", sessionErr)
		}
	}
	alerts := ComputeAlertsAt(domainStates, domainInteractions, sessionStart, input.Now)

	return &pipelineFixtures{
		StatesList:         domainStates,
		StatesByConcept:    stateMap,
		GoalRelevance:      goalRelevance,
		ActiveMisc:         activeMisc,
		RecentConcepts:     recent,
		RecentInteractions: domainInteractions,
		Alerts:             alerts,
		DiagnosticItems:    diagItems,
	}, nil
}

func buildObservables(domain *models.Domain, pf *pipelineFixtures, cfg PhaseConfig, now time.Time) PhaseObservables {
	meanH := MeanBinaryEntropyOverGraph(domain.Graph, pf.StatesByConcept)

	bkt := algorithms.MasteryBKT()
	mastered := 0
	totalGoalRelevant := 0
	belowRetention := false

	for _, c := range domain.Graph.Concepts {
		rel, hasRel := pf.GoalRelevance[c]
		if pf.GoalRelevance == nil {
			// Uniform fallback : every concept counts as goal-relevant.
			rel = 1.0
			hasRel = true
		}
		if !hasRel {
			// Uncovered concept — excluded from the goal-relevant set
			// (consistent with OQ-2.7 and [4] OQ-4.3 = B').
			continue
		}
		if rel <= cfg.GoalRelevantCutoff {
			continue
		}
		totalGoalRelevant++

		cs := pf.StatesByConcept[c]
		if cs == nil {
			// No state ≡ never practised ≡ not mastered, not
			// "below retention" (nothing to forget).
			continue
		}
		if cs.PMastery >= bkt {
			mastered++
			// Even mastered concepts can be "below retention" —
			// the MAINTENANCE → INSTRUCTION trigger looks at
			// retention drop on goal-relevants, mastered or not.
		}
		if cs.CardState != "new" {
			retention := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
			if retention < cfg.RetentionRecallThreshold {
				belowRetention = true
			}
		}
	}

	return PhaseObservables{
		MeanEntropy:                meanH,
		PhaseEntryEntropy:          domain.PhaseEntryEntropy,
		DiagnosticItemsCount:       pf.DiagnosticItems,
		MasteredGoalRelevant:       mastered,
		TotalGoalRelevant:          totalGoalRelevant,
		GoalRelevantBelowRetention: belowRetention,
	}
}

// noFringeFallbackPhase suggests a reasonable phase to fall back to
// when the pipeline returns NoFringe in the current phase. Used only
// in the one-shot retry inside Orchestrate.
func noFringeFallbackPhase(current models.Phase) models.Phase {
	switch current {
	case models.PhaseInstruction:
		// Probably everything's mastered — try MAINTENANCE.
		return models.PhaseMaintenance
	case models.PhaseMaintenance:
		// Probably nothing mastered — back to INSTRUCTION.
		return models.PhaseInstruction
	case models.PhaseDiagnostic:
		// Stuck at saturation — go to INSTRUCTION.
		return models.PhaseInstruction
	default:
		return current
	}
}

// runPipeline executes Gate → ConceptSelector → ActionSelector and
// composes the resulting models.Activity. Returns IsNoFringe=true
// when [3] or [4] signal an empty pool (so Orchestrate can attempt
// the one-shot phase retry).
func runPipeline(
	ctx context.Context,
	store storeport.Store,
	domain *models.Domain,
	pf *pipelineFixtures,
	phase models.Phase,
	input OrchestratorInput,
) (models.Activity, pipelineSignal, error) {
	// ── [3] Gate ───────────────────────────────────────────────────
	antiRep := input.Config.AntiRepeatWindow
	if antiRep == 0 {
		antiRep = DefaultAntiRepeatWindow
	}
	gateResult, err := ApplyGate(GateInput{
		Phase:                phase,
		Concepts:             domain.Graph.Concepts,
		States:               pf.StatesByConcept,
		Graph:                domain.Graph,
		ActiveMisconceptions: pf.ActiveMisc,
		RecentConcepts:       pf.RecentConcepts,
		Alerts:               pf.Alerts,
		AntiRepeatWindow:     antiRep,
	})
	if err != nil {
		return models.Activity{}, pipelineSignal{}, fmt.Errorf("gate: %w", err)
	}
	if gateResult.EscapeAction != nil {
		return composeEscapeActivity(*gateResult.EscapeAction), pipelineSignal{}, nil
	}
	if !input.ReviewOnly {
		if selection, ok := criticalForgettingBypassSelection(pf.Alerts, phase); ok {
			action, err := selectActionForSelection(ctx, store, pf, selection, input)
			if err != nil {
				return models.Activity{}, pipelineSignal{}, err
			}
			return composeActivity(action, selection, phase), pipelineSignal{}, nil
		}
	}
	if gateResult.NoCandidate {
		return models.Activity{}, pipelineSignal{IsNoFringe: true}, nil
	}

	// ── [4] ConceptSelector — restricted to gate's allowed pool ────
	allowedSet := make(map[string]bool, len(gateResult.AllowedConcepts))
	for _, c := range gateResult.AllowedConcepts {
		allowedSet[c] = true
	}

	filteredGraph := models.KnowledgeSpace{
		Concepts:      gateResult.AllowedConcepts,
		Prerequisites: filterPrerequisites(domain.Graph.Prerequisites, allowedSet),
	}
	var selection Selection
	if input.ReviewOnly {
		selection = SelectReviewConceptAt(gateResult.AllowedConcepts, pf.StatesByConcept, pf.RecentInteractions, pf.ActiveMisc, input.Now)
	} else {
		selection, err = SelectConceptAt(phase, pf.StatesList, filteredGraph, pf.GoalRelevance, input.Now)
		if err != nil {
			return models.Activity{}, pipelineSignal{}, fmt.Errorf("concept_selector: %w", err)
		}
	}
	if selection.NoFringe {
		return models.Activity{}, pipelineSignal{IsNoFringe: true}, nil
	}

	// ── [5] ActionSelector — on the chosen concept ────────────────
	action, err := selectActionForSelection(ctx, store, pf, selection, input)
	if err != nil {
		return models.Activity{}, pipelineSignal{}, err
	}
	if input.ReviewOnly {
		action = constrainReviewAction(action, pf.StatesByConcept[selection.Concept], input.Now)
	}

	// Honor Gate's ActionRestriction (defensive — [5] already
	// prioritises misconception, so this is belt + braces).
	if restrictions, ok := gateResult.ActionRestriction[selection.Concept]; ok && len(restrictions) > 0 {
		if !containsActivityType(restrictions, action.Type) {
			action.Type = restrictions[0]
			action.Rationale = "gate ActionRestriction override : " + action.Rationale
		}
	}

	return composeActivity(action, selection, phase), pipelineSignal{}, nil
}

func criticalForgettingBypassSelection(alerts []models.Alert, phase models.Phase) (Selection, bool) {
	bestConcept := ""
	bestRetention := 1.0
	for _, alert := range alerts {
		if alert.Type != models.AlertForgetting || alert.Urgency != models.UrgencyCritical {
			continue
		}
		if bestConcept == "" || alert.Retention < bestRetention {
			bestConcept = alert.Concept
			bestRetention = alert.Retention
		}
	}
	if bestConcept == "" {
		return Selection{}, false
	}
	return Selection{
		Concept: bestConcept,
		Score:   1 - bestRetention,
		Phase:   models.Phase(fmt.Sprintf("%s+bypass_forgetting", phase)),
		Rationale: fmt.Sprintf(
			"FORGETTING-Critical bypass retention=%.2f",
			bestRetention,
		),
	}, true
}

func selectActionForSelection(
	ctx context.Context,
	store storeport.Store,
	pf *pipelineFixtures,
	selection Selection,
	input OrchestratorInput,
) (Action, error) {
	cs := pf.StatesByConcept[selection.Concept]
	if cs == nil {
		// Concept in the graph but no state — create a default state for
		// SelectAction (mastery=0, theta=0) to avoid the panic.
		cs = models.NewConceptStateInDomain(input.LearnerID, input.DomainID, selection.Concept)
	}
	var mc *models.MisconceptionGroup
	if pf.ActiveMisc[selection.Concept] {
		var err error
		mc, err = store.GetFirstActiveMisconceptionInDomain(ctx, input.LearnerID, input.DomainID, selection.Concept)
		if err != nil {
			return Action{}, fmt.Errorf("fetch misconception: %w", err)
		}
	}
	history, err := store.GetActionHistoryForConceptInDomain(ctx, input.LearnerID, input.DomainID, selection.Concept, 50)
	if err != nil {
		return Action{}, fmt.Errorf("action history: %w", err)
	}
	return SelectActionAt(selection.Concept, cs, mc, ActionHistory{
		InteractionsAboveBKT:  history.InteractionsAboveBKT,
		MasteryChallengeCount: history.MasteryChallengeCount,
		FeynmanCount:          history.FeynmanCount,
		TransferCount:         history.TransferCount,
	}, input.Now), nil
}

func filterPrerequisites(src map[string][]string, allowed map[string]bool) map[string][]string {
	if src == nil {
		return nil
	}
	out := make(map[string][]string, len(allowed))
	for c := range allowed {
		if pre, ok := src[c]; ok {
			out[c] = pre
		}
	}
	return out
}

func containsActivityType(set []models.ActivityType, t models.ActivityType) bool {
	return slices.Contains(set, t)
}

func composeActivity(a Action, sel Selection, phase models.Phase) models.Activity {
	phaseLabel := phase
	if sel.Phase != "" {
		phaseLabel = sel.Phase
	}
	return models.Activity{
		Type:             a.Type,
		Concept:          sel.Concept,
		DifficultyTarget: a.DifficultyTarget,
		Format:           a.Format,
		EstimatedMinutes: a.EstimatedMinutes,
		Rationale:        fmt.Sprintf("[phase=%s] %s - %s", phaseLabel, sel.Rationale, a.Rationale),
		PromptForLLM:     BuildActivityPrompt(a.Type, sel.Concept, a.Format),
	}
}

func composeEscapeActivity(esc EscapeAction) models.Activity {
	return models.Activity{
		Type:         esc.Type,
		Format:       esc.Format,
		Rationale:    esc.Rationale,
		PromptForLLM: "Session terminee. Emets le recap_brief et appelle record_session_close.",
	}
}
