// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

// Package engine — [4] ConceptSelector.
//
// SelectConcept is component [4] of the regulation pipeline (see
// docs/regulation-design/04-concept-selector.md). It is a (quasi-)pure
// function that, given a phase, the learner's per-concept state, the
// domain graph, and a goal_relevance vector, decides *which concept*
// to work on next.
//
// The function is the first real consumer of goal_relevance produced
// by [1] GoalDecomposer — and therefore the empirical test of the
// LLM-driven decomposition contract. If the LLM's decomposition is
// wrong (or absent), [4] reveals it: routing is observably degraded.
//
// Wired into the runtime by engine.Orchestrate (see orchestrator.go).
// The standalone REGULATION_CONCEPT flag only toggles the system-prompt
// documentation appendix in tools/prompt.go — the selector itself runs
// as part of the orchestrator regardless.
package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

// ErrUnknownPhase is returned by SelectConcept when the caller passes
// a Phase value that is neither PhaseInstruction, PhaseDiagnostic,
// nor PhaseMaintenance. The caller is expected to crash on this —
// silent fallback would mask version-skew bugs (cf. OQ-4.1).
var ErrUnknownPhase = errors.New("concept_selector: unknown phase")

// Selection is the structured output of SelectConcept. NoFringe is a
// signal (not an error) telling the caller to consider transitioning
// to a different phase: an empty INSTRUCTION fringe means everything
// is mastered (caller should switch to MAINTENANCE), an empty
// MAINTENANCE pool means nothing is mastered (caller should switch
// back to INSTRUCTION), and an empty DIAGNOSTIC pool means the BKT
// state is fully saturated (no informative observation possible).
type Selection struct {
	Concept   string
	Score     float64
	NoFringe  bool
	Phase     models.Phase
	Rationale string
}

// SelectConcept dispatches on phase to the appropriate sub-selector.
// Returns ErrUnknownPhase (and slog.Error logs the corrupt phase) for
// any value not in the {Instruction, Diagnostic, Maintenance} set —
// no silent fallback (OQ-4.1 = A with explicit-error correction).
//
// goalRelevance:
//   - nil           → uniform 1.0 fallback for every concept (per [1] §1.2)
//   - non-nil, key present → score used as multiplier
//   - non-nil, key absent  → concept EXCLUDED from the eligible pool
//     (OQ-4.3 = B'). Forces the LLM to call set_goal_relevance for
//     newly-added concepts; uncovered concepts are not silently
//     defaulted. The caller (in production) surfaces NoFringe to
//     prompt re-decomposition.
//
// Note that DIAGNOSTIC ignores goalRelevance entirely in v1 (OQ-4.2 sub
// A1) — uncovered concepts are still eligible for diagnosis. This is
// a v1 choice to be re-arbitrated with real data.
func SelectConcept(
	phase models.Phase,
	states []*models.ConceptState,
	graph models.KnowledgeSpace,
	goalRelevance map[string]float64,
) (Selection, error) {
	return SelectConceptAt(phase, states, graph, goalRelevance, time.Now().UTC())
}

// SelectConceptAt is the clock-injected decision boundary used by the runtime.
// In MAINTENANCE it derives card age from now - LastReview; ElapsedDays is the
// historical interval that preceded the last FSRS review and is never used as
// the current age.
func SelectConceptAt(
	phase models.Phase,
	states []*models.ConceptState,
	graph models.KnowledgeSpace,
	goalRelevance map[string]float64,
	now time.Time,
) (Selection, error) {
	switch phase {
	case models.PhaseInstruction:
		return selectInstruction(states, graph, goalRelevance), nil
	case models.PhaseMaintenance:
		return selectMaintenance(states, graph, goalRelevance, now), nil
	case models.PhaseDiagnostic:
		return selectDiagnostic(states, graph), nil
	default:
		slog.Error("concept_selector: unknown phase",
			"phase", string(phase))
		return Selection{}, fmt.Errorf("%w: %q", ErrUnknownPhase, string(phase))
	}
}

// fringeItem pairs a concept with its current mastery for scoring.
// Built once by externalFringe so the scoring loop is allocation-free.
type fringeItem struct {
	concept string
	mastery float64
}

// externalFringe returns the concepts that are currently *learnable*:
// prereqs satisfied at MasteryKST(), own mastery strictly below
// MasteryBKT(). Concepts with NaN mastery are excluded defensively.
//
// Concepts present in graph.Concepts but absent from states are
// treated as mastery=0, eligible if their prereqs are satisfied
// (OQ-4.1 = A for sub-question (a)).
func externalFringe(states []*models.ConceptState, graph models.KnowledgeSpace) []fringeItem {
	stateByConcept := make(map[string]*models.ConceptState, len(states))
	for _, cs := range states {
		if cs == nil {
			continue
		}
		stateByConcept[cs.Concept] = cs
	}
	bktThreshold := algorithms.MasteryBKT()
	kstThreshold := algorithms.MasteryKST()

	var fringe []fringeItem
	for _, concept := range graph.Concepts {
		mastery := 0.0
		if cs := stateByConcept[concept]; cs != nil {
			if math.IsNaN(cs.PMastery) {
				continue
			}
			mastery = cs.PMastery
		}
		if mastery >= bktThreshold {
			continue
		}

		prereqsOK := true
		for _, prereq := range graph.Prerequisites[concept] {
			pm := 0.0
			if cs := stateByConcept[prereq]; cs != nil {
				if math.IsNaN(cs.PMastery) {
					prereqsOK = false
					break
				}
				pm = cs.PMastery
			}
			if pm < kstThreshold {
				prereqsOK = false
				break
			}
		}
		if !prereqsOK {
			continue
		}
		fringe = append(fringe, fringeItem{concept: concept, mastery: mastery})
	}
	// Alphabetical sort for deterministic tie-break (OQ-4.4 = A).
	sort.Slice(fringe, func(i, j int) bool {
		return fringe[i].concept < fringe[j].concept
	})
	return fringe
}

// resolveRelevance implements the goal_relevance lookup with the
// OQ-4.3 = B' contract: missing concept in a non-nil vector ≡ NOT
// eligible. The boolean return tells the caller whether to skip the
// concept entirely. nil vector falls back to uniform 1.0 (eligible).
func resolveRelevance(gr map[string]float64, concept string) (score float64, eligible bool) {
	if gr == nil {
		return 1.0, true
	}
	v, ok := gr[concept]
	if !ok {
		return 0, false
	}
	return v, true
}

// selectInstruction implements the INSTRUCTION branch:
//
//	score = goal_relevance × (1 - mastery)
//
// over the external fringe. Tie-break is alphabetical (the fringe is
// pre-sorted by externalFringe). OQ-4.6 = A.
func selectInstruction(
	states []*models.ConceptState,
	graph models.KnowledgeSpace,
	goalRelevance map[string]float64,
) Selection {
	fringe := externalFringe(states, graph)
	if len(fringe) == 0 {
		return Selection{
			NoFringe:  true,
			Phase:     models.PhaseInstruction,
			Rationale: "outer fringe empty: everything mastered or prereqs missing",
		}
	}

	bestConcept := ""
	bestScore := math.Inf(-1)
	bestRel := 0.0
	bestMastery := 0.0
	candidatesEligible := 0
	for _, item := range fringe {
		rel, eligible := resolveRelevance(goalRelevance, item.concept)
		if !eligible {
			// OQ-4.3 = B' — uncovered concept: not selectable until
			// set_goal_relevance is called.
			continue
		}
		candidatesEligible++
		score := rel * (1 - item.mastery)
		if score > bestScore {
			bestScore = score
			bestConcept = item.concept
			bestRel = rel
			bestMastery = item.mastery
		}
	}
	if candidatesEligible == 0 {
		// All fringe candidates were absent from the goal_relevance
		// vector → re-decomposition required. NoFringe signals the
		// caller to surface this to the LLM via get_goal_relevance.
		return Selection{
			NoFringe:  true,
			Phase:     models.PhaseInstruction,
			Rationale: "fringe non-empty but no concept covered by goal_relevance - call set_goal_relevance",
		}
	}
	return Selection{
		Concept: bestConcept,
		Score:   bestScore,
		Phase:   models.PhaseInstruction,
		Rationale: fmt.Sprintf("argmax(rel=%.2f x (1-mastery)=%.2f) sur %d candidats eligibles",
			bestRel, 1-bestMastery, candidatesEligible),
	}
}

// selectMaintenance implements the MAINTENANCE branch:
//
//	urgency = 1 - retention      (OQ-4.5 = A)
//	score   = urgency × goal_relevance
//
// over the mastered set (PMastery >= MasteryBKT()). Cards in
// CardState=="new" get urgency=0 — they are mastered by BKT but
// have no FSRS history yet.
//
// v2 note: the dervative form "elapsed_days/stability" would be more
// sensitive near the decay knee; revisit if eval shows MAINTENANCE
// missing fast-forgetting concepts.
func selectMaintenance(
	states []*models.ConceptState,
	graph models.KnowledgeSpace,
	goalRelevance map[string]float64,
	now time.Time,
) Selection {
	bktThreshold := algorithms.MasteryBKT()

	// Domain filter: states whose concept is absent from graph.Concepts
	// are excluded. pf.StatesList is learner-wide, so without this guard
	// a concept mastered in another domain leaks into the active
	// domain's MAINTENANCE pool — particularly when goal_relevance is
	// nil and urgency × 1.0 alone drives selection. Empty graph.Concepts
	// = "no filter" preserves existing call sites passing
	// models.KnowledgeSpace{}; the orchestrator always supplies a
	// non-empty graph for the active domain. Issue #93.
	var domainSet map[string]struct{}
	if len(graph.Concepts) > 0 {
		domainSet = make(map[string]struct{}, len(graph.Concepts))
		for _, c := range graph.Concepts {
			domainSet[c] = struct{}{}
		}
	}

	// Collect mastered concepts; sort alphabetically for deterministic
	// tie-break.
	var mastered []*models.ConceptState
	for _, cs := range states {
		if cs == nil {
			continue
		}
		if math.IsNaN(cs.PMastery) {
			continue
		}
		if cs.PMastery < bktThreshold {
			continue
		}
		if domainSet != nil {
			if _, ok := domainSet[cs.Concept]; !ok {
				continue
			}
		}
		mastered = append(mastered, cs)
	}
	if len(mastered) == 0 {
		return Selection{
			NoFringe:  true,
			Phase:     models.PhaseMaintenance,
			Rationale: "no mastered concept: MAINTENANCE not applicable",
		}
	}
	sort.Slice(mastered, func(i, j int) bool {
		return mastered[i].Concept < mastered[j].Concept
	})

	bestConcept := ""
	bestScore := math.Inf(-1)
	bestUrgency := 0.0
	bestRel := 0.0
	candidatesEligible := 0
	for _, cs := range mastered {
		rel, eligible := resolveRelevance(goalRelevance, cs.Concept)
		if !eligible {
			continue // OQ-4.3 = B'
		}
		candidatesEligible++

		var urgency float64
		if cs.CardState == "new" {
			urgency = 0
		} else {
			retention := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
			urgency = 1 - retention
		}
		score := urgency * rel
		if score > bestScore {
			bestScore = score
			bestConcept = cs.Concept
			bestUrgency = urgency
			bestRel = rel
		}
	}
	if candidatesEligible == 0 {
		return Selection{
			NoFringe:  true,
			Phase:     models.PhaseMaintenance,
			Rationale: "mastered concepts but none covered by goal_relevance - call set_goal_relevance",
		}
	}
	return Selection{
		Concept: bestConcept,
		Score:   bestScore,
		Phase:   models.PhaseMaintenance,
		Rationale: fmt.Sprintf("argmax((1-retention)=%.2f x rel=%.2f) sur %d candidats",
			bestUrgency, bestRel, candidatesEligible),
	}
}

// selectDiagnostic implements the DIAGNOSTIC branch via BKT info-gain.
// OQ-4.2 sub A1: v1 ignores goal_relevance entirely — diagnosing is
// about reducing uncertainty over the *whole* state, not the
// goal-relevant subset. To be re-arbitrated with real data.
//
// Saturation pre-filter: P(L) <= 0.05 or >= 0.95 → info-gain ≈ 0
// already, but excluding them up-front keeps the score interpretation
// clean (the argmax is over genuinely ambiguous concepts only).
//
// Domain filter: states whose concept is absent from graph.Concepts are
// excluded. pf.StatesList is learner-wide (GetConceptStatesByLearner),
// so without this guard a concept mastered in another domain leaks into
// the active domain's DIAGNOSTIC selection — symmetric to the
// MAINTENANCE leak fixed for issue #93. An empty graph.Concepts is
// treated as "no filter" to preserve existing call sites that pass
// models.KnowledgeSpace{}; the orchestrator always supplies a non-empty
// graph for the active domain.
func selectDiagnostic(states []*models.ConceptState, graph models.KnowledgeSpace) Selection {
	var domainSet map[string]struct{}
	if len(graph.Concepts) > 0 {
		domainSet = make(map[string]struct{}, len(graph.Concepts))
		for _, c := range graph.Concepts {
			domainSet[c] = struct{}{}
		}
	}

	var candidates []*models.ConceptState
	for _, cs := range states {
		if cs == nil || math.IsNaN(cs.PMastery) {
			continue
		}
		if cs.PMastery <= 0.05 || cs.PMastery >= 0.95 {
			continue
		}
		if domainSet != nil {
			if _, ok := domainSet[cs.Concept]; !ok {
				continue
			}
		}
		candidates = append(candidates, cs)
	}
	if len(candidates) == 0 {
		return Selection{
			NoFringe:  true,
			Phase:     models.PhaseDiagnostic,
			Rationale: "tous concepts satures : aucune observation informative",
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Concept < candidates[j].Concept
	})

	bestConcept := ""
	bestScore := math.Inf(-1)
	for _, cs := range candidates {
		ig := algorithms.BKTInfoGain(cs)
		if ig > bestScore {
			bestScore = ig
			bestConcept = cs.Concept
		}
	}
	return Selection{
		Concept: bestConcept,
		Score:   bestScore,
		Phase:   models.PhaseDiagnostic,
		Rationale: fmt.Sprintf("max info-gain=%.3f sur %d candidats non satures",
			bestScore, len(candidates)),
	}
}

// SelectReviewConceptAt ranks already-seen concepts by current decayed
// retention (plus misconception and sub-mastery boosts) for review intent.
func SelectReviewConceptAt(
	allowed []string,
	states map[string]*models.ConceptState,
	interactions []*models.Interaction,
	activeMisc map[string]bool,
	now time.Time,
) Selection {
	interactionCounts := make(map[string]int)
	for _, interaction := range interactions {
		if interaction == nil {
			continue
		}
		interactionCounts[interaction.Concept]++
	}

	best := Selection{NoFringe: true, Phase: models.PhaseMaintenance}
	bestScore := -1.0
	for _, concept := range allowed {
		cs := states[concept]
		if !reviewableConcept(cs, interactionCounts[concept]) {
			continue
		}
		retention := reviewRetentionAt(cs, now)
		score := 1 - retention
		if activeMisc[concept] {
			score += 2
		}
		if cs != nil && cs.PMastery < algorithms.MasteryBKT() {
			score += 0.15
		}
		if interactionCounts[concept] > 0 {
			score += 0.05
		}
		if score > bestScore {
			bestScore = score
			best = Selection{
				Concept:   concept,
				Score:     score,
				NoFringe:  false,
				Phase:     models.PhaseMaintenance,
				Rationale: fmt.Sprintf("[intent=review] selected prior concept %q with retention %.2f", concept, retention),
			}
		}
	}
	return best
}

func reviewableConcept(cs *models.ConceptState, interactionCount int) bool {
	if interactionCount > 0 {
		return true
	}
	if cs == nil {
		return false
	}
	return cs.Reps > 0 || cs.CardState != "new"
}

func reviewRetentionAt(cs *models.ConceptState, now time.Time) float64 {
	if cs == nil || cs.CardState == "new" {
		return 0.70
	}
	return algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
}

func constrainReviewAction(action Action, cs *models.ConceptState, now time.Time) Action {
	switch action.Type {
	case models.ActivityRecall, models.ActivityPractice, models.ActivityDebugMisconception:
		action.Rationale = "review intent constraint : " + action.Rationale
		return action
	}
	if reviewRetentionAt(cs, now) < algorithms.RetentionRecallRoutingThreshold {
		return Action{
			Type:             models.ActivityRecall,
			DifficultyTarget: 0.60,
			Format:           "review_retrieval",
			EstimatedMinutes: 8,
			Rationale:        "review intent constraint : retrieval before new or mastery activity",
		}
	}
	return Action{
		Type:             models.ActivityPractice,
		DifficultyTarget: clampActionDifficulty(action.DifficultyTarget),
		Format:           "review_practice",
		EstimatedMinutes: 10,
		Rationale:        "review intent constraint : practice on prior material",
	}
}
