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
//  4. Runs the Gate → ConceptSelector → ActionSelector pipeline
//     with one-shot retry on NoFringe (the FSM is re-evaluated and
//     the pipeline retried once).
//  5. CAS-persists the settled transition only after the pipeline succeeds
//     (unless PreviewOnly is requested).
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
	// DecisionScope, when set, lets the cadences count the decisions already
	// served to this learner, not only the interactions the host recorded.
	DecisionScope models.TenantScope
	// ReviewOnly biases the pipeline toward previously studied material
	// while preserving Gate and ActionSelector constraints.
	ReviewOnly bool
	// PreviewOnly projects FSM transitions and selects an activity without
	// persisting phase changes. It is intended for planning surfaces such as
	// learning negotiation; only serving an activity should advance runtime
	// phase state.
	PreviewOnly bool
	// Config is injected — typically NewDefaultPhaseConfig() in
	// production. Tests can pass narrower configs to drive specific
	// scenarios.
	Config PhaseConfig
	// Logger receives FSM and persistence diagnostics. Nil disables
	// orchestrator logging for callers that have not opted in.
	Logger *slog.Logger
	// EvidenceSnapshot lets a request that already loaded domain evidence reuse
	// the same two-query snapshot across alerting and downstream enrichment.
	// Nil preserves the standalone Orchestrate behaviour.
	EvidenceSnapshot *EvidenceSnapshot
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
// from the DB. Unless PreviewOnly is set, the returned phase is the one
// persisted by store.CompareAndSwapDomainPhase during the call. PreviewOnly
// returns the projected phase without changing storage.
//
// On error, the returned phase is the empty string ("").
func OrchestrateWithPhase(ctx context.Context, store storeport.Store, input OrchestratorInput) (models.Activity, models.Phase, error) {
	logger := input.logger()
	domain, err := store.GetLearnerDomainByID(ctx, input.LearnerID, input.DomainID)
	if err != nil {
		return models.Activity{}, "", fmt.Errorf("%w: %q: %v", ErrUnknownDomain, input.DomainID, err)
	}

	// 1. Read current phase ; NULL → INSTRUCTION fallback (OQ-2.1.b).
	storedPhase := domain.Phase
	currentPhase := storedPhase
	if currentPhase == "" {
		currentPhase = models.PhaseInstruction
	}
	phaseWritePending := false
	phaseEntryEntropy := 0.0
	persistSettledPhase := func() error {
		if input.PreviewOnly || !phaseWritePending {
			return nil
		}
		if err := store.CompareAndSwapDomainPhase(
			ctx, domain.ID, storedPhase, currentPhase, phaseEntryEntropy, input.Now,
		); err != nil {
			return err
		}
		storedPhase = currentPhase
		phaseWritePending = false
		return nil
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
				"entry_entropy", entryEntropy)
			currentPhase = eval.To
			phaseEntryEntropy = entryEntropy
			phaseWritePending = true
		}
	}

	// 4. Run pipeline with one-shot retry on NoFringe.
	//
	// A settled FSM transition is not undone by an empty selection pool.
	// Diversity is relaxed inside runPipeline before it reports NoFringe;
	// recall needs are selected independently of the acquisition fringe.
	for retry := 0; retry <= orchestratorMaxRetries; retry++ {
		activity, sig, err := runPipeline(ctx, store, domain, pf, currentPhase, input)
		if err != nil {
			return models.Activity{}, "", err
		}
		if !sig.IsNoFringe {
			if persistErr := persistSettledPhase(); persistErr != nil {
				logger.Error("orchestrator: failed to persist settled phase",
					"domain", domain.ID, "to", currentPhase, "err", persistErr)
				return models.Activity{}, "", fmt.Errorf("persist phase transition: %w", persistErr)
			}
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
		currentPhase = next
		phaseEntryEntropy = 0
		phaseWritePending = true
	}
	if persistErr := persistSettledPhase(); persistErr != nil {
		logger.Error("orchestrator: failed to persist settled fallback phase",
			"domain", domain.ID, "to", currentPhase, "err", persistErr)
		return models.Activity{}, "", fmt.Errorf("persist fallback phase transition: %w", persistErr)
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
	DiagnosticItems    int             // count since phase_changed_at
	DiagnosedConcepts  map[string]bool // qualified diagnostic coverage since phase_changed_at
	// Evidence is the domain evidence snapshot shared with alerting.
	Evidence *EvidenceSnapshot
	// VerifiedConcepts holds concepts with a passed evaluated assessment
	// attempt or a spaced FSRS success (see PrerequisiteRoutingMastery).
	VerifiedConcepts map[string]bool
	// RecentDecisions are the decisions served for the domain, newest first.
	// They count proposals the host never recorded as interactions.
	RecentDecisions []*models.PedagogicalDecision
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

	// A stale vector grants a default weight to concepts added since it was
	// set, so add_concepts never hides a concept from routing. A current
	// vector keeps omission as "not goal-relevant".
	goalRelevance := domain.EffectiveGoalRelevance()

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
	diagnosed := map[string]bool{}
	if !domain.PhaseChangedAt.IsZero() {
		qualifiedConcepts, queryErr := store.GetQualifiedDiagnosticConceptsSinceInDomain(ctx, input.LearnerID, domain.ID, domain.PhaseChangedAt)
		err = queryErr
		if err != nil {
			return nil, fmt.Errorf("get qualified diagnostic concept coverage: %w", err)
		}
		for _, concept := range qualifiedConcepts {
			if domainConcepts[concept] {
				diagItems++
				diagnosed[concept] = true
			}
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
	var recentDecisions []*models.PedagogicalDecision
	if input.DecisionScope.LearnerID != "" {
		if input.DecisionScope.LearnerID != input.LearnerID {
			return nil, fmt.Errorf("get recent decisions: scope does not match learner")
		}
		recentDecisions, err = store.ListRecentPedagogicalDecisions(ctx, input.DecisionScope, domain.ID, recentServedWindow)
		if err != nil {
			return nil, fmt.Errorf("get recent decisions: %w", err)
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
	evidenceSnapshot, err := ensureEvidenceSnapshot(
		ctx, store, input.LearnerID, domain.ID, domain.Graph.Concepts, input.EvidenceSnapshot,
	)
	if err != nil {
		return nil, fmt.Errorf("load alert evidence: %w", err)
	}
	alertEvidence := evidenceSnapshot.ForMasteryAlerts(domainStates)
	alerts := ComputeAlertsWithEvidenceAt(domainStates, domainInteractions, alertEvidence, sessionStart, input.Now)
	verified := verifiedConcepts(input.LearnerID, domain.Graph.Concepts, stateMap, evidenceSnapshot, input.Now)

	return &pipelineFixtures{
		StatesList:         domainStates,
		StatesByConcept:    stateMap,
		GoalRelevance:      goalRelevance,
		ActiveMisc:         activeMisc,
		RecentConcepts:     recent,
		RecentInteractions: domainInteractions,
		Alerts:             alerts,
		DiagnosticItems:    diagItems,
		DiagnosedConcepts:  diagnosed,
		Evidence:           evidenceSnapshot,
		VerifiedConcepts:   verified,
		RecentDecisions:    recentDecisions,
	}, nil
}

// verifiedConcepts marks the concepts whose success is backed by more than a
// host declaration: a passed evaluated assessment attempt (completed together
// with its linked interaction), or a successful FSRS review at least one day
// after the previous one. The result is always non-nil.
func verifiedConcepts(learnerID string, concepts []string, states map[string]*models.ConceptState, evidence *EvidenceSnapshot, now time.Time) map[string]bool {
	verified := make(map[string]bool, len(concepts))
	for _, concept := range concepts {
		if SpacedSuccessVerified(states[concept]) {
			verified[concept] = true
			continue
		}
		for _, attempt := range assessmentEvidenceAt(learnerID, concept, evidence.ForConcept(concept).Assessments, now) {
			if attempt.Passed {
				verified[concept] = true
				break
			}
		}
	}
	return verified
}

func buildObservables(domain *models.Domain, pf *pipelineFixtures, cfg PhaseConfig, now time.Time) PhaseObservables {
	meanH := MeanBinaryEntropyOverGraph(domain.Graph, pf.StatesByConcept)

	bkt := algorithms.MasteryBKT()
	exit := cfg.EffectiveMasteryExitThreshold()
	estimated := 0
	belowExit := 0
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
			// No state ≡ never practised ≡ not estimated, below the exit
			// floor, and not "below retention" (nothing to forget).
			belowExit++
			continue
		}
		if cs.PMastery >= bkt {
			estimated++
			// High estimates and recall needs are separate signals.
		}
		if cs.PMastery < exit {
			belowExit++
		}
		if RecallEligible(cs) {
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
		DiagnosticCoverageTarget:   min(len(domain.Graph.Concepts), cfg.NDiagnosticMax),
		EstimatedGoalRelevant:      estimated,
		TotalGoalRelevant:          totalGoalRelevant,
		BelowExitGoalRelevant:      belowExit,
		GoalRelevantBelowRetention: belowRetention,
	}
}

// noFringeFallbackPhase suggests a reasonable phase to fall back to
// when the pipeline returns NoFringe in the current phase. Used only
// in the one-shot retry inside Orchestrate.
func noFringeFallbackPhase(current models.Phase) models.Phase {
	switch current {
	case models.PhaseInstruction:
		// Probably every estimate crossed the routing threshold — try
		// MAINTENANCE without presenting this as demonstrated mastery.
		return models.PhaseMaintenance
	case models.PhaseMaintenance:
		// Probably nothing mastered — back to INSTRUCTION.
		return models.PhaseInstruction
	case models.PhaseDiagnostic:
		// Diagnostic coverage is an integrity gate. A temporary NoFringe must
		// not bypass the required attempt-linked concept coverage.
		return models.PhaseDiagnostic
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
	gateResult, err := ApplyGate(GateInput{
		Phase:                phase,
		Concepts:             domain.Graph.Concepts,
		States:               pf.StatesByConcept,
		Graph:                domain.Graph,
		ActiveMisconceptions: pf.ActiveMisc,
		RecentConcepts:       pf.RecentConcepts,
		Alerts:               pf.Alerts,
		AntiRepeatWindow:     antiRep,
		VerifiedConcepts:     pf.VerifiedConcepts,
	})
	if err != nil {
		return models.Activity{}, pipelineSignal{}, fmt.Errorf("gate: %w", err)
	}
	if gateResult.EscapeAction != nil {
		return composeEscapeActivity(*gateResult.EscapeAction), pipelineSignal{}, nil
	}
	if !input.ReviewOnly && phase != models.PhaseDiagnostic {
		if selection, ok := criticalForgettingBypassSelection(pf.Alerts, phase); ok {
			action, err := selectActionForSelection(ctx, store, pf, selection, phase, input)
			if err != nil {
				return models.Activity{}, pipelineSignal{}, err
			}
			return composeActivity(action, selection, phase), pipelineSignal{}, nil
		}
	}
	recallSelection, recallDue := Selection{}, false
	if !input.ReviewOnly && phase != models.PhaseDiagnostic {
		recallSelection, recallDue = recallNeedSelection(domain, pf, phase, input)
	}
	routeRecall := func() (models.Activity, pipelineSignal, error) {
		action, err := selectActionForSelection(ctx, store, pf, recallSelection, phase, input)
		if err != nil {
			return models.Activity{}, pipelineSignal{}, err
		}
		return composeActivity(recallActionFor(action), recallSelection, phase), pipelineSignal{}, nil
	}
	if recallDue && recallQuotaAllows(pf.RecentInteractions, phase) {
		return routeRecall()
	}
	if !input.ReviewOnly && phase != models.PhaseDiagnostic {
		if selection, ok := masteryChallengeSelection(domain, pf, phase, input); ok {
			return composeActivity(masteryChallengeAction(), selection, phase), pipelineSignal{}, nil
		}
	}
	if !input.ReviewOnly && phase == models.PhaseMaintenance && !recallDue {
		if action, selection, ok := demonstratedMaintenanceSelection(domain, pf, phase, input); ok {
			return composeActivity(action, selection, phase), pipelineSignal{}, nil
		}
	}
	if gateResult.NoCandidate {
		if recallDue {
			// Nothing to acquire: a due recall is better than a phase retry.
			return routeRecall()
		}
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
		selection, err = SelectConceptAtWithContext(phase, pf.StatesList, filteredGraph, pf.GoalRelevance, input.Now, SelectionContext{
			MaintenanceMasteryFloor: input.Config.EffectiveMasteryExitThreshold(),
			DiagnosedConcepts:       pf.DiagnosedConcepts,
			VerifiedConcepts:        pf.VerifiedConcepts,
			DomainGraph:             &domain.Graph,
		})
		if err != nil {
			return models.Activity{}, pipelineSignal{}, fmt.Errorf("concept_selector: %w", err)
		}
	}
	if selection.NoFringe {
		// The phase's eligible fringe can be narrower than the gate's pool
		// (e.g. only one concept still needs acquisition). Relax diversity once
		// before interpreting that empty fringe as a phase-level signal.
		if antiRep > 0 {
			input.Config.AntiRepeatWindow = 0
			activity, signal, err := runPipeline(ctx, store, domain, pf, phase, input)
			if err == nil && !signal.IsNoFringe {
				activity.Rationale += " - anti-repetition relaxed: no phase-eligible activity remained"
			}
			return activity, signal, err
		}
		if recallDue {
			return routeRecall()
		}
		return models.Activity{}, pipelineSignal{IsNoFringe: true}, nil
	}

	// ── [5] ActionSelector — on the chosen concept ────────────────
	action, err := selectActionForSelection(ctx, store, pf, selection, phase, input)
	if err != nil {
		return models.Activity{}, pipelineSignal{}, err
	}
	if input.ReviewOnly {
		action = constrainReviewAction(action, pf.StatesByConcept[selection.Concept], input.Now)
	}

	// Honor Gate's ActionRestriction (defensive — [5] already
	// prioritises misconception, so this is belt + braces).
	if phase != models.PhaseDiagnostic {
		if restrictions, ok := gateResult.ActionRestriction[selection.Concept]; ok && len(restrictions) > 0 {
			if !containsActivityType(restrictions, action.Type) {
				action.Type = restrictions[0]
				action.Rationale = "gate ActionRestriction override : " + action.Rationale
			}
		}
	}

	return composeActivity(action, selection, phase), pipelineSignal{}, nil
}

// RecallInterleaveInstructionActivities is the number of non-recall
// activities required after a routed recall before the next due recall in
// INSTRUCTION: at most one due recall for two acquisition activities, so a
// backlog of due cards cannot stall acquisition. MAINTENANCE has no quota.
const RecallInterleaveInstructionActivities = 2

// MasteryChallengeCadence is the minimum number of interactions between two
// mastery challenges routed outside the high-mastery rotation.
const MasteryChallengeCadence = 5

// recallNeedSelection picks the most overdue previously acquired concept.
// A concept is due when its FSRS NextReview has passed, or when its current
// retrievability is below the FSRS desired retention (0.9) for a card with
// no stored schedule. Concepts are ranked by relative lateness
// (now - NextReview) / ScheduledDays, then lower retention, then concept ID.
// The 0.40/0.30 retention bands remain the user-facing FORGETTING alerts.
func recallNeedSelection(domain *models.Domain, pf *pipelineFixtures, phase models.Phase, input OrchestratorInput) (Selection, bool) {
	best := Selection{Phase: phase}
	bestOverdue, bestRetention := 0.0, 0.0
	for _, concept := range domain.Graph.Concepts {
		relevance, eligible := resolveRelevance(pf.GoalRelevance, concept)
		if !eligible || relevance <= input.Config.GoalRelevantCutoff {
			continue
		}
		cs := pf.StatesByConcept[concept]
		if reviewedWithin(cs, input.Now, RecallMinimumSpacing) {
			continue
		}
		overdue, retention, due := RecallDue(cs, input.Now)
		if !due {
			continue
		}
		if best.Concept == "" || overdue > bestOverdue ||
			(overdue == bestOverdue && (retention < bestRetention || (retention == bestRetention && concept < best.Concept))) {
			best.Concept = concept
			best.Score = overdue
			best.Rationale = fmt.Sprintf("recall due: relative lateness %.2f, current retention %.2f", overdue, retention)
			bestOverdue, bestRetention = overdue, retention
		}
	}
	return best, best.Concept != ""
}

// RecallMinimumSpacing keeps a card that was just reviewed out of recall
// selection. A failed review reschedules the card immediately; without this
// spacing the same recall would be served again before any other activity.
const RecallMinimumSpacing = 10 * time.Minute

func reviewedWithin(cs *models.ConceptState, now time.Time, window time.Duration) bool {
	if cs == nil || cs.LastReview == nil || cs.LastReview.IsZero() {
		return false
	}
	elapsed := now.Sub(*cs.LastReview)
	return elapsed >= 0 && elapsed < window
}

// RecallDue reports whether an acquired card is due for spaced retrieval and
// its relative lateness (now - NextReview) / max(1 day, ScheduledDays).
func RecallDue(cs *models.ConceptState, now time.Time) (overdue, retention float64, due bool) {
	if !RecallEligible(cs) {
		return 0, 1, false
	}
	retention = algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
	next := cs.NextReview
	if next == nil || next.IsZero() {
		if !(retention < algorithms.FSRSDesiredRetention) {
			return 0, retention, false
		}
		scheduled := cs.LastReview.Add(time.Duration(cs.ScheduledDays) * 24 * time.Hour)
		next = &scheduled
	} else if next.After(now) {
		return 0, retention, false
	}
	interval := time.Duration(max(1, cs.ScheduledDays)) * 24 * time.Hour
	return max(0, float64(now.Sub(*next))/float64(interval)), retention, true
}

// recallQuotaAllows enforces RecallInterleaveInstructionActivities on the
// most recent interactions of the domain (newest first).
func recallQuotaAllows(recent []*models.Interaction, phase models.Phase) bool {
	if phase == models.PhaseMaintenance {
		return true
	}
	for i, interaction := range recent {
		if i >= RecallInterleaveInstructionActivities {
			break
		}
		if interaction != nil && models.ActivityType(interaction.ActivityType) == models.ActivityRecall {
			return false
		}
	}
	return true
}

// recallActionFor turns the selected action into a retrieval, except when an
// active misconception must be confronted first.
func recallActionFor(action Action) Action {
	if action.Type == models.ActivityDebugMisconception {
		return action
	}
	return Action{
		Type:             models.ActivityRecall,
		DifficultyTarget: 0.65,
		Format:           "spaced_retrieval",
		EstimatedMinutes: 8,
		Rationale:        "spaced retrieval of a due card",
	}
}

// recentServedWindow bounds the decision history read for cadence and
// rotation.
const recentServedWindow = 50

// servedWithinCadence reports whether activityType was served among the last
// MasteryChallengeCadence recorded interactions or served decisions of the
// domain. Decisions cover hosts that never record the activity itself.
func servedWithinCadence(pf *pipelineFixtures, activityType models.ActivityType) bool {
	for i, interaction := range pf.RecentInteractions {
		if i >= MasteryChallengeCadence {
			break
		}
		if interaction != nil && models.ActivityType(interaction.ActivityType) == activityType {
			return true
		}
	}
	for i, decision := range pf.RecentDecisions {
		if i >= MasteryChallengeCadence {
			break
		}
		if decision != nil && decision.Contract.RecommendedActivityType == activityType {
			return true
		}
	}
	return false
}

// lastServedAt returns, per concept, when activityType was last served as an
// interaction or a decision.
func lastServedAt(pf *pipelineFixtures, activityType models.ActivityType) map[string]time.Time {
	last := map[string]time.Time{}
	note := func(concept string, at time.Time) {
		if concept != "" && at.After(last[concept]) {
			last[concept] = at
		}
	}
	for _, interaction := range pf.RecentInteractions {
		if interaction != nil && models.ActivityType(interaction.ActivityType) == activityType {
			note(interaction.Concept, interaction.CreatedAt)
		}
	}
	for _, decision := range pf.RecentDecisions {
		if decision != nil && decision.Contract.RecommendedActivityType == activityType {
			note(decision.Contract.TargetConcept, decision.CreatedAt)
		}
	}
	return last
}

// leastRecentlyServed picks the candidate served least recently with
// activityType; never-served candidates come first and ties keep the
// candidates' order.
func leastRecentlyServed(candidates []string, pf *pipelineFixtures, activityType models.ActivityType) string {
	last := lastServedAt(pf, activityType)
	best := ""
	for _, concept := range candidates {
		if best == "" || last[concept].Before(last[best]) {
			best = concept
		}
	}
	return best
}

// masteryChallengeSelection routes a MASTERY_CHALLENGE for a concept that
// carries AlertMasteryReady but is not yet demonstrated. Outside MAINTENANCE
// the high-mastery rotation is otherwise unreachable, because the fringe
// excludes concepts above the routing threshold. Cadence: no mastery
// challenge among the last MasteryChallengeCadence interactions or served
// decisions of the domain. Candidates rotate, least recently challenged first.
func masteryChallengeSelection(domain *models.Domain, pf *pipelineFixtures, phase models.Phase, input OrchestratorInput) (Selection, bool) {
	if servedWithinCadence(pf, models.ActivityMasteryChallenge) {
		return Selection{}, false
	}
	inDomain := make(map[string]bool, len(domain.Graph.Concepts))
	for _, concept := range domain.Graph.Concepts {
		inDomain[concept] = true
	}
	var candidates []string
	for _, alert := range pf.Alerts {
		if alert.Type != models.AlertMasteryReady || !inDomain[alert.Concept] || pf.ActiveMisc[alert.Concept] {
			continue
		}
		if _, eligible := resolveRelevance(pf.GoalRelevance, alert.Concept); !eligible {
			continue
		}
		evidence := pf.Evidence.ForConcept(alert.Concept)
		status := AssessMasteryStatus(input.LearnerID, alert.Concept, pf.StatesByConcept[alert.Concept],
			pf.RecentInteractions, evidence.Transfers, evidence.Assessments, input.Now)
		if status.Demonstrated {
			continue
		}
		candidates = append(candidates, alert.Concept)
	}
	if len(candidates) == 0 {
		return Selection{}, false
	}
	slices.Sort(candidates)
	return Selection{
		Concept:   leastRecentlyServed(candidates, pf, models.ActivityMasteryChallenge),
		Score:     1,
		Phase:     phase,
		Rationale: "MASTERY_READY without demonstration: mastery challenge",
	}, true
}

// MaintenanceChallengeRefreshAge is the age of the latest demonstration after
// which MAINTENANCE re-tests a demonstrated concept with a new challenge.
const MaintenanceChallengeRefreshAge = 60 * 24 * time.Hour

// demonstratedMaintenanceSelection handles MAINTENANCE once every
// goal-relevant concept is demonstrated and no recall is due. Re-practising
// demonstrated material at that point is wasted time, so in order:
//
//  1. a concept not yet Transferred gets a TRANSFER_PROBE on its first
//     missing (or failing) transfer dimension;
//  2. a concept whose latest demonstration is older than
//     MaintenanceChallengeRefreshAge gets a new MASTERY_CHALLENGE;
//  3. otherwise REST: the domain is maintained until the next card is due.
//
// Probes and challenges follow MasteryChallengeCadence and rotate, least
// recently served concept first. While one is pending but held back by the
// cadence, ok is false so normal MAINTENANCE practice runs in between.
//
// ok is false while any goal-relevant concept is not demonstrated, so the
// normal MAINTENANCE selection keeps running.
func demonstratedMaintenanceSelection(domain *models.Domain, pf *pipelineFixtures, phase models.Phase, input OrchestratorInput) (Action, Selection, bool) {
	type candidate struct {
		concept   string
		status    MasteryStatus
		transfer  TransferProfile
		lastShown time.Time
	}
	var concepts []candidate
	for _, concept := range domain.Graph.Concepts {
		relevance, eligible := resolveRelevance(pf.GoalRelevance, concept)
		if !eligible || relevance <= input.Config.GoalRelevantCutoff {
			continue
		}
		evidence := pf.Evidence.ForConcept(concept)
		status := AssessMasteryStatus(input.LearnerID, concept, pf.StatesByConcept[concept],
			pf.RecentInteractions, evidence.Transfers, evidence.Assessments, input.Now)
		if !status.Demonstrated || pf.ActiveMisc[concept] {
			return Action{}, Selection{}, false
		}
		attempts := assessmentEvidenceAt(input.LearnerID, concept, evidence.Assessments, input.Now)
		lastShown, _ := latestTrustedDemonstrationAt(concept, attempts)
		concepts = append(concepts, candidate{
			concept:   concept,
			status:    status,
			transfer:  BuildTrustedTransferProfileFromEvidence(concept, evidence.Transfers, attempts, input.Now),
			lastShown: lastShown,
		})
	}
	if len(concepts) == 0 {
		return Action{}, Selection{}, false
	}
	byConcept := make(map[string]candidate, len(concepts))
	var untransferred, stale []string
	for _, c := range concepts {
		byConcept[c.concept] = c
		if !c.status.Transferred {
			untransferred = append(untransferred, c.concept)
		}
		if c.lastShown.IsZero() || input.Now.Sub(c.lastShown) >= MaintenanceChallengeRefreshAge {
			stale = append(stale, c.concept)
		}
	}
	held := false
	if len(untransferred) > 0 {
		if !servedWithinCadence(pf, models.ActivityTransferProbe) {
			c := byConcept[leastRecentlyServed(untransferred, pf, models.ActivityTransferProbe)]
			dimension := transferProbeDimension(c.transfer)
			return Action{
					Type:             models.ActivityTransferProbe,
					DifficultyTarget: 0.65,
					Format:           "transfer_" + string(dimension),
					EstimatedMinutes: 20,
					Rationale:        fmt.Sprintf("demonstrated but not transferred: probe the %s transfer dimension", dimension),
				}, Selection{Concept: c.concept, Score: 1, Phase: phase,
					Rationale: "every goal-relevant concept demonstrated, nothing due"}, true
		}
		held = true
	}
	if len(stale) > 0 {
		if !servedWithinCadence(pf, models.ActivityMasteryChallenge) {
			action := masteryChallengeAction()
			action.Rationale = "latest demonstration is old: refresh the mastery challenge"
			return action, Selection{Concept: leastRecentlyServed(stale, pf, models.ActivityMasteryChallenge), Score: 1, Phase: phase,
				Rationale: "every goal-relevant concept demonstrated, nothing due"}, true
		}
		held = true
	}
	if held {
		return Action{}, Selection{}, false
	}
	return Action{
			Type:      models.ActivityRest,
			Format:    "maintenance_complete",
			Rationale: "nothing is due for recall and every goal-relevant concept is demonstrated and transferred",
		}, Selection{Phase: phase,
			Rationale: "maintenance complete until the next scheduled review"}, true
}

// transferProbeDimension picks the first canonical dimension that is missing,
// then the first failing one, then the weakest.
func transferProbeDimension(profile TransferProfile) TransferDimension {
	if len(profile.MissingDimensions) > 0 {
		return profile.MissingDimensions[0]
	}
	for _, summary := range profile.DimensionSummaries {
		if summary.Covered && !summary.Passing {
			return summary.Dimension
		}
	}
	if len(profile.WeakestDimensions) > 0 {
		return profile.WeakestDimensions[0]
	}
	return TransferDimensionNear
}

func masteryChallengeAction() Action {
	return Action{
		Type:             models.ActivityMasteryChallenge,
		DifficultyTarget: 0.75,
		Format:           "integrated_performance_task",
		EstimatedMinutes: 45,
		Rationale:        "ready for a mastery challenge: prepare it as a decision-bound assessment attempt",
	}
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
	phase models.Phase,
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
	return SelectActionForPhaseAt(phase, selection.Concept, cs, mc, ActionHistory{
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
