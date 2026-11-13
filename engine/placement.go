// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// Adaptive placement (DIAGNOSTIC phase).
//
// The diagnostic places the learner on the prerequisite graph with as few
// cold items as possible:
//
//   - the next item targets the concept of median prerequisite depth among
//     the concepts not yet classified (dichotomy on the graph depth);
//   - a hint-free success is also applied to every ancestor (transitive
//     prerequisite) of the tested concept, a failure to every descendant;
//   - a concept receives at most PlacementMaxItemsPerConcept direct items, and a
//     second one only while its estimate stays strictly inside
//     (PlacementLowerBound, PlacementUpperBound). Further answers on the same
//     concept (a host repeating one item) are ignored by the placement: they
//     neither move an estimate nor spend the item budget;
//   - a concept whose stored P(L) is not finite is unplaceable: it is never
//     served and never keeps the placement open;
//   - the phase ends when every placeable concept is classified or when the
//     item budget (PhaseConfig.NDiagnosticMax) is spent.
//
// Every concept starts the placement at PlacementPrior instead of the
// bootstrap P(L)=0.1 of a new concept state: a cold diagnostic measures prior
// knowledge, it must not presume ignorance. With the reference slip/guess
// (0.1/0.2) one hint-free success moves 0.5 to 0.82 and one failure to 0.11,
// so a failure classifies after one item and a success after two.
//
// The placement is a pure replay of the qualified diagnostic observations of
// the current DIAGNOSTIC phase, as listed by the store
// (storeport.QualifiedDiagnosticObservationReader), oldest first. The replay
// reads each concept's slip/guess as stored now, not as they were when an
// answer was recorded, so a later change of those parameters changes the
// replayed estimates. For fixed slip/guess, observation-only BKT updates
// commute, so the estimates do not depend on observation order; which
// answers count against the per-concept cap and the budget does.
const (
	// PlacementPrior is the uninformative starting estimate of a concept
	// during placement.
	PlacementPrior = 0.5
	// PlacementLowerBound and PlacementUpperBound delimit the ambiguous band:
	// an observed concept whose estimate leaves the open interval is
	// classified (not known / known).
	PlacementLowerBound = 0.15
	PlacementUpperBound = 0.85
	// PlacementMaxItemsPerConcept caps direct diagnostic items per concept.
	PlacementMaxItemsPerConcept = 2
	// DefaultDiagnosticItemBudget is the default qualified-item budget of one
	// DIAGNOSTIC phase (PhaseConfig.NDiagnosticMax).
	DefaultDiagnosticItemBudget = 12
)

// PlacementEstimate is the placement state of one concept.
type PlacementEstimate struct {
	Estimate        float64
	DirectItems     int
	PropagatedItems int
	Depth           int
	// Unplaceable marks a concept whose stored P(L) is not finite: the
	// placement neither serves nor waits for it.
	Unplaceable bool
}

// Observed reports whether the concept received any direct or propagated
// observation.
func (e PlacementEstimate) Observed() bool {
	return e.DirectItems+e.PropagatedItems > 0
}

// Classified reports whether the placement no longer needs to test the
// concept: either its estimate left the ambiguous band after an observation,
// or it already received the maximum number of direct items.
func (e PlacementEstimate) Classified() bool {
	if e.DirectItems >= PlacementMaxItemsPerConcept {
		return true
	}
	return e.Observed() && (e.Estimate <= PlacementLowerBound || e.Estimate >= PlacementUpperBound)
}

// Placement is the replayed state of an adaptive placement over one domain
// graph.
type Placement struct {
	// Concepts holds one estimate per concept of the domain graph.
	Concepts map[string]PlacementEstimate
	// Items counts the direct observations applied: at most
	// PlacementMaxItemsPerConcept per concept.
	Items int
	// Budget is the item budget; zero or negative means unbounded.
	Budget int

	ancestors   map[string][]string
	descendants map[string][]string
}

// ReplayPlacement rebuilds the placement of a domain graph from its qualified
// diagnostic observations (hint-free by construction), oldest first. states
// supplies the current slip/guess of each concept (reference parameters when
// absent); its P(L) only marks a non-finite concept unplaceable. Observations
// on concepts outside the graph, and those beyond the
// PlacementMaxItemsPerConcept first ones of a concept, are ignored.
func ReplayPlacement(graph models.KnowledgeSpace, states map[string]*models.ConceptState, observations []models.DiagnosticObservation, budget int) Placement {
	ancestors, descendants := prerequisiteClosure(graph)
	depths := prerequisiteDepths(graph)
	p := Placement{
		Concepts:    make(map[string]PlacementEstimate, len(graph.Concepts)),
		Budget:      budget,
		ancestors:   ancestors,
		descendants: descendants,
	}
	for _, concept := range graph.Concepts {
		p.Concepts[concept] = PlacementEstimate{
			Estimate:    PlacementPrior,
			Depth:       depths[concept],
			Unplaceable: unplaceableState(states[concept]),
		}
	}
	for _, obs := range observations {
		estimate, ok := p.Concepts[obs.Concept]
		if !ok || estimate.DirectItems >= PlacementMaxItemsPerConcept {
			continue
		}
		p.Items++
		p.observe(obs.Concept, obs.Success, states, true)
		targets := descendants[obs.Concept]
		if obs.Success {
			targets = ancestors[obs.Concept]
		}
		for _, target := range targets {
			p.observe(target, obs.Success, states, false)
		}
	}
	return p
}

func (p *Placement) observe(concept string, success bool, states map[string]*models.ConceptState, direct bool) {
	estimate, ok := p.Concepts[concept]
	if !ok {
		return
	}
	slip, guess := placementObservationParams(states[concept])
	estimate.Estimate = algorithms.BKTObserve(algorithms.BKTState{
		PMastery: estimate.Estimate,
		PSlip:    slip,
		PGuess:   guess,
	}, success).PMastery
	if direct {
		estimate.DirectItems++
	} else {
		estimate.PropagatedItems++
	}
	p.Concepts[concept] = estimate
}

// placementObservationParams returns the concept's slip/guess when they are
// usable probabilities, otherwise the immutable reference parameters.
func placementObservationParams(cs *models.ConceptState) (slip, guess float64) {
	reference := models.BKTReferenceParams()
	slip, guess = reference.PSlip, reference.PGuess
	if cs == nil {
		return slip, guess
	}
	if validObservationProbability(cs.PSlip) && validObservationProbability(cs.PGuess) && cs.PSlip+cs.PGuess < 1 {
		return cs.PSlip, cs.PGuess
	}
	return slip, guess
}

func validObservationProbability(v float64) bool {
	return !math.IsNaN(v) && v > 0 && v < 1
}

// unplaceableState reports a stored estimate that is not a finite number.
func unplaceableState(cs *models.ConceptState) bool {
	return cs != nil && (math.IsNaN(cs.PMastery) || math.IsInf(cs.PMastery, 0))
}

// Unclassified returns the placeable concepts still to place, sorted by name.
func (p Placement) Unclassified() []string {
	var out []string
	for concept, estimate := range p.Concepts {
		if !estimate.Unplaceable && !estimate.Classified() {
			out = append(out, concept)
		}
	}
	sort.Strings(out)
	return out
}

// BudgetReached reports whether the item budget is spent.
func (p Placement) BudgetReached() bool {
	return p.Budget > 0 && p.Items >= p.Budget
}

// Complete reports whether the placement can stop.
func (p Placement) Complete() bool {
	return p.BudgetReached() || len(p.Unclassified()) == 0
}

// NextConcept picks the next concept to test among the unclassified concepts
// admitted by allowed (nil admits every concept). It returns false when the
// placement is complete or when no unclassified concept is admitted.
//
// The pick is the concept of median prerequisite depth among the admitted
// unclassified concepts (lower median). Among concepts at that depth, the one
// whose outcome reaches the most unclassified ancestors and descendants wins,
// then the lexical order breaks ties.
func (p Placement) NextConcept(allowed map[string]bool) (string, string, bool) {
	if p.BudgetReached() {
		return "", "", false
	}
	unclassified := p.Unclassified()
	open := make(map[string]bool, len(unclassified))
	for _, concept := range unclassified {
		open[concept] = true
	}
	var candidates []string
	for _, concept := range unclassified {
		if allowed == nil || allowed[concept] {
			candidates = append(candidates, concept)
		}
	}
	if len(candidates) == 0 {
		return "", "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := p.Concepts[candidates[i]].Depth, p.Concepts[candidates[j]].Depth
		if di != dj {
			return di < dj
		}
		return candidates[i] < candidates[j]
	})
	medianDepth := p.Concepts[candidates[(len(candidates)-1)/2]].Depth
	best, bestReach := "", -1
	for _, concept := range candidates {
		if p.Concepts[concept].Depth != medianDepth {
			continue
		}
		reach := 0
		for _, relative := range p.ancestors[concept] {
			if open[relative] {
				reach++
			}
		}
		for _, relative := range p.descendants[concept] {
			if open[relative] {
				reach++
			}
		}
		if reach > bestReach {
			best, bestReach = concept, reach
		}
	}
	rationale := fmt.Sprintf(
		"adaptive placement: median depth %d among %d unclassified concepts, outcome reaches %d unclassified relatives (items %d/%s)",
		medianDepth, len(candidates), bestReach, p.Items, placementBudgetLabel(p.Budget))
	return best, rationale, true
}

func placementBudgetLabel(budget int) string {
	if budget <= 0 {
		return "unbounded"
	}
	return fmt.Sprintf("%d", budget)
}

// LoadPlacementObservations returns the qualified diagnostic observations of
// the DIAGNOSTIC phase of domainID started at since, oldest first. A zero since
// (no phase start) yields none.
func LoadPlacementObservations(ctx context.Context, store storeport.QualifiedDiagnosticObservationReader, scope models.TenantScope, domainID string, since time.Time) ([]models.DiagnosticObservation, error) {
	if since.IsZero() {
		return nil, nil
	}
	return store.ListQualifiedDiagnosticObservationsInDomain(ctx, scope, domainID, since)
}

// PersistableEstimates returns the placement estimate of every observed
// concept whose evidence comes only from this placement. A concept listed in
// foreign (it has an interaction before the phase, of another kind, hinted or
// not evaluated) keeps its own model estimate.
func (p Placement) PersistableEstimates(foreign map[string]bool) map[string]float64 {
	out := make(map[string]float64)
	for concept, estimate := range p.Concepts {
		if estimate.Observed() && !foreign[concept] {
			out[concept] = estimate.Estimate
		}
	}
	return out
}

// PlacementStore is the persistence surface of ApplyPlacementEstimates: the
// domain, its qualified diagnostic observations, the concepts with other
// evidence, and the concept states it locks and rewrites.
type PlacementStore interface {
	storeport.DomainStore
	storeport.ConceptStateStore
	storeport.QualifiedDiagnosticObservationReader
}

// ApplyPlacementEstimates persists the placement estimates of the learner's
// domain while it is in DIAGNOSTIC. It must run inside the transaction that
// records an attempt-linked DIAGNOSTIC_ASSESSMENT, after the interaction and
// the attempt evaluation are written, so the replay includes them. Only P(L)
// is written: FSRS fields are never touched, so a placement can neither
// create nor reschedule a memory card. Returns the placement estimates of the
// domain (nil outside DIAGNOSTIC or without a domain). scope.LearnerID selects
// the learner and must match the caller's tenant transaction.
func ApplyPlacementEstimates(ctx context.Context, store PlacementStore, scope models.TenantScope, domainID string, cfg PhaseConfig) (map[string]float64, error) {
	if domainID == "" {
		return nil, nil
	}
	learnerID := scope.LearnerID
	domain, err := store.GetLearnerDomainByID(ctx, learnerID, domainID)
	if err != nil {
		return nil, fmt.Errorf("placement: load domain: %w", err)
	}
	if domain.Phase != models.PhaseDiagnostic || domain.PhaseChangedAt.IsZero() {
		return nil, nil
	}
	observations, err := LoadPlacementObservations(ctx, store, scope, domain.ID, domain.PhaseChangedAt)
	if err != nil {
		return nil, fmt.Errorf("placement: qualified diagnostics: %w", err)
	}
	foreignConcepts, err := store.ListConceptsWithNonPlacementEvidenceInDomain(ctx, scope, domain.ID, domain.PhaseChangedAt)
	if err != nil {
		return nil, fmt.Errorf("placement: other evidence: %w", err)
	}
	foreign := make(map[string]bool, len(foreignConcepts))
	for _, concept := range foreignConcepts {
		foreign[concept] = true
	}
	states, err := store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("placement: concept states: %w", err)
	}
	stateMap := make(map[string]*models.ConceptState, len(states))
	for _, cs := range states {
		stateMap[cs.Concept] = cs
	}
	placement := ReplayPlacement(domain.Graph, stateMap, observations, cfg.NDiagnosticMax)
	estimates := placement.PersistableEstimates(foreign)
	concepts := make([]string, 0, len(estimates))
	for concept := range estimates {
		concepts = append(concepts, concept)
	}
	sort.Strings(concepts) // stable lock order
	for _, concept := range concepts {
		cs, err := store.GetOrCreateConceptStateForUpdateInDomain(ctx, learnerID, domain.ID, concept)
		if err != nil {
			return nil, fmt.Errorf("placement: lock %q: %w", concept, err)
		}
		if cs.PMastery == estimates[concept] {
			continue
		}
		cs.PMastery = estimates[concept]
		if err := store.UpsertConceptState(ctx, cs); err != nil {
			return nil, fmt.Errorf("placement: write %q: %w", concept, err)
		}
	}
	return estimates, nil
}

// prerequisiteClosure returns, for every concept of the graph, its transitive
// prerequisites (ancestors) and the concepts that transitively require it
// (descendants), each sorted. Edges to concepts outside the graph are ignored
// and cycles are tolerated.
func prerequisiteClosure(graph models.KnowledgeSpace) (ancestors, descendants map[string][]string) {
	inGraph := make(map[string]bool, len(graph.Concepts))
	for _, concept := range graph.Concepts {
		inGraph[concept] = true
	}
	parents := make(map[string][]string, len(graph.Concepts))
	children := make(map[string][]string, len(graph.Concepts))
	for concept, prereqs := range graph.Prerequisites {
		if !inGraph[concept] {
			continue
		}
		for _, prereq := range prereqs {
			if !inGraph[prereq] || prereq == concept {
				continue
			}
			parents[concept] = append(parents[concept], prereq)
			children[prereq] = append(children[prereq], concept)
		}
	}
	walk := func(start string, next map[string][]string) []string {
		seen := map[string]bool{start: true}
		queue := append([]string(nil), next[start]...)
		var out []string
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if seen[current] {
				continue
			}
			seen[current] = true
			out = append(out, current)
			queue = append(queue, next[current]...)
		}
		sort.Strings(out)
		return out
	}
	ancestors = make(map[string][]string, len(graph.Concepts))
	descendants = make(map[string][]string, len(graph.Concepts))
	for _, concept := range graph.Concepts {
		ancestors[concept] = walk(concept, parents)
		descendants[concept] = walk(concept, children)
	}
	return ancestors, descendants
}

// prerequisiteDepths returns the length of the longest prerequisite chain
// below each concept (roots have depth 0). A cycle contributes no depth.
func prerequisiteDepths(graph models.KnowledgeSpace) map[string]int {
	inGraph := make(map[string]bool, len(graph.Concepts))
	for _, concept := range graph.Concepts {
		inGraph[concept] = true
	}
	depths := make(map[string]int, len(graph.Concepts))
	visiting := make(map[string]bool)
	var depth func(string) int
	depth = func(concept string) int {
		if d, ok := depths[concept]; ok {
			return d
		}
		if visiting[concept] {
			return -1 // cycle edge: ignored by the caller
		}
		visiting[concept] = true
		best := 0
		for _, prereq := range graph.Prerequisites[concept] {
			if !inGraph[prereq] || prereq == concept {
				continue
			}
			if d := depth(prereq); d >= 0 && d+1 > best {
				best = d + 1
			}
		}
		visiting[concept] = false
		depths[concept] = best
		return best
	}
	for _, concept := range graph.Concepts {
		depth(concept)
	}
	return depths
}
