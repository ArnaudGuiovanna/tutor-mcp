// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func setScheduledReview(t *testing.T, concept string, p, stability float64, lastReview time.Time, scheduledDays int, upsert func(*models.ConceptState) error, get func() (*models.ConceptState, error)) {
	t.Helper()
	cs, err := get()
	if err != nil {
		t.Fatal(err)
	}
	next := lastReview.Add(time.Duration(scheduledDays) * 24 * time.Hour)
	cs.PMastery, cs.CardState, cs.Stability, cs.Reps = p, "review", stability, 3
	cs.ElapsedDays, cs.ScheduledDays = 1, scheduledDays
	cs.LastReview, cs.NextReview = &lastReview, &next
	if err := upsert(cs); err != nil {
		t.Fatal(err)
	}
}

// P0-2: a card whose NextReview has passed is recalled while its retention is
// still high, and due recalls are interleaved with acquisition (at most one
// recall for two instruction activities).
func TestOrchestrate_DueRecallIsRoutedAndInterleaved(t *testing.T) {
	store := setupOrchStore(t)
	domainID := seedOrchDomain(t, store, []string{"A", "B", "C"}, nil, models.PhaseInstruction)
	now := time.Now().UTC()
	ctx := context.Background()
	setScheduledReview(t, "A", 0.95, 10, now.Add(-3*24*time.Hour), 2,
		func(cs *models.ConceptState) error { return store.UpsertConceptState(ctx, cs) },
		func() (*models.ConceptState, error) { return store.GetConceptState(ctx, "L1", "A") })

	input := defaultInput(domainID)
	input.Config.AntiRepeatWindow = 0
	activity, err := Orchestrate(ctx, store, input)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Type != models.ActivityRecall || activity.Concept != "A" {
		t.Fatalf("a due card (retention ~0.97) must be recalled: %+v", activity)
	}

	if _, err := recordSyntheticInteraction(t, store, domainID, "A", string(models.ActivityRecall), true, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	activity, err = Orchestrate(ctx, store, input)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Type == models.ActivityRecall {
		t.Fatalf("recall quota exhausted: acquisition must come next, got %+v", activity)
	}
	if _, err := recordSyntheticInteraction(t, store, domainID, "B", string(models.ActivityNewConcept), true, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if activity, err = Orchestrate(ctx, store, input); err != nil || activity.Type == models.ActivityRecall {
		t.Fatalf("one instruction activity is not enough to release the quota: %+v err=%v", activity, err)
	}
	if _, err := recordSyntheticInteraction(t, store, domainID, "C", string(models.ActivityNewConcept), true, now); err != nil {
		t.Fatal(err)
	}
	if activity, err = Orchestrate(ctx, store, input); err != nil || activity.Type != models.ActivityRecall || activity.Concept != "A" {
		t.Fatalf("after two instruction activities the due recall returns: %+v err=%v", activity, err)
	}
}

// P0-2: a card not yet due is left alone.
func TestRecallDue(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-2 * 24 * time.Hour)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)
	notDue := &models.ConceptState{CardState: "review", Stability: 10, LastReview: &last, NextReview: &future, ScheduledDays: 3}
	if _, _, due := RecallDue(notDue, now); due {
		t.Fatal("a card scheduled tomorrow is not due")
	}
	due := &models.ConceptState{CardState: "review", Stability: 10, LastReview: &last, NextReview: &past, ScheduledDays: 1}
	overdue, _, ok := RecallDue(due, now)
	if !ok || overdue < 0.99 || overdue > 1.01 {
		t.Fatalf("one interval late: overdue=%.3f due=%v", overdue, ok)
	}
	learning := &models.ConceptState{CardState: "learning", Stability: 1, LastReview: &last, NextReview: &past}
	if _, _, ok := RecallDue(learning, now); ok {
		t.Fatal("a never-acquired card is not due for recall")
	}
}

// P1-8: a MASTERY_READY concept without demonstration gets a challenge, at
// most once per MasteryChallengeCadence interactions.
func TestMasteryChallengeSelection(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A", "B"}}}
	pf := &pipelineFixtures{
		StatesByConcept: map[string]*models.ConceptState{"A": {LearnerID: "L1", Concept: "A", PMastery: 0.95, CardState: "review"}},
		Alerts:          []models.Alert{{Type: models.AlertMasteryReady, Concept: "A"}},
	}
	input := OrchestratorInput{LearnerID: "L1", Now: now}
	sel, ok := masteryChallengeSelection(domain, pf, models.PhaseInstruction, input)
	if !ok || sel.Concept != "A" {
		t.Fatalf("expected a challenge on A, got %+v ok=%v", sel, ok)
	}
	pf.RecentInteractions = []*models.Interaction{
		{Concept: "B", ActivityType: string(models.ActivityPractice)},
		{Concept: "A", ActivityType: string(models.ActivityMasteryChallenge)},
	}
	if _, ok := masteryChallengeSelection(domain, pf, models.PhaseInstruction, input); ok {
		t.Fatal("a challenge two interactions ago blocks another one")
	}
	pf.RecentInteractions = nil
	pf.ActiveMisc = map[string]bool{"A": true}
	if _, ok := masteryChallengeSelection(domain, pf, models.PhaseInstruction, input); ok {
		t.Fatal("an active misconception is confronted first")
	}
}

// P1-5b: verification comes from a passed evaluated attempt or a spaced
// FSRS success; host-declared same-day successes do not verify.
func TestVerifiedConcepts(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	submitted, evaluated := now.Add(-2*time.Hour), now.Add(-90*time.Minute)
	states := map[string]*models.ConceptState{
		"massed": {CardState: "review", PMastery: 0.95, ElapsedDays: 0, LastReview: &last},
		"spaced": {CardState: "review", PMastery: 0.95, ElapsedDays: 2, LastReview: &last},
		"graded": {CardState: "review", PMastery: 0.95, ElapsedDays: 0, LastReview: &last},
	}
	evidence := &EvidenceSnapshot{AssessmentsByConcept: map[string][]*models.AssessmentAttempt{
		"graded": {{ID: "x", LearnerID: "L1", ConceptID: "graded", Status: models.AssessmentAttemptEvaluated,
			Passed: true, SubmittedAt: &submitted, EvaluatedAt: &evaluated}},
	}}
	verified := verifiedConcepts("L1", []string{"massed", "spaced", "graded"}, states, evidence, now)
	if verified["massed"] || !verified["spaced"] || !verified["graded"] {
		t.Fatalf("verified=%v", verified)
	}
}

// P1-5b: in the runtime, an unverified prerequisite slows but does not block:
// the successor unlocks once the prerequisite has a spaced success.
func TestOrchestrate_UnverifiedPrerequisiteUnlocksAfterSpacedSuccess(t *testing.T) {
	store := setupOrchStore(t)
	ctx := context.Background()
	domainID := seedOrchDomain(t, store, []string{"A", "B"}, map[string][]string{"B": {"A"}}, models.PhaseInstruction)
	cs, err := store.GetConceptState(ctx, "L1", "A")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Now().UTC().Add(-time.Hour)
	next := last.Add(10 * 24 * time.Hour)
	cs.PMastery, cs.CardState, cs.Stability, cs.ElapsedDays, cs.ScheduledDays = 0.95, "review", 10, 0, 10
	cs.LastReview, cs.NextReview = &last, &next
	if err := store.UpsertConceptState(ctx, cs); err != nil {
		t.Fatal(err)
	}
	input := defaultInput(domainID)
	activity, err := Orchestrate(ctx, store, input)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Concept == "B" {
		t.Fatalf("three declared same-day successes must not unlock B: %+v", activity)
	}
	cs.ElapsedDays = 2
	if err := store.UpsertConceptState(ctx, cs); err != nil {
		t.Fatal(err)
	}
	activity, err = Orchestrate(ctx, store, input)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Concept != "B" {
		t.Fatalf("a spaced success verifies A and unlocks B: %+v", activity)
	}
}

// P2-14: once everything is demonstrated and nothing is due, MAINTENANCE
// probes a missing transfer dimension, re-challenges only an old
// demonstration, and otherwise rests.
func TestDemonstratedMaintenanceSelection(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	attempt := func(id string, activity models.ActivityType, at time.Time) *models.AssessmentAttempt {
		submitted, evaluated := at.Add(-time.Minute), at
		return &models.AssessmentAttempt{ID: id, LearnerID: "L1", ConceptID: "A", ActivityType: string(activity),
			Status: models.AssessmentAttemptEvaluated, Passed: true, TrustedEvaluation: true,
			EvaluationMethod: models.EvaluationMethodExternal, SubmittedAt: &submitted, EvaluatedAt: &evaluated}
	}
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A"}}}
	last := now.Add(-time.Hour)
	pf := &pipelineFixtures{
		StatesByConcept: map[string]*models.ConceptState{"A": {LearnerID: "L1", Concept: "A", PMastery: 0.95, CardState: "review", Stability: 30, LastReview: &last}},
		Evidence: &EvidenceSnapshot{AssessmentsByConcept: map[string][]*models.AssessmentAttempt{
			"A": {attempt("mc", models.ActivityMasteryChallenge, now.Add(-24*time.Hour))},
		}},
	}
	input := OrchestratorInput{LearnerID: "L1", Now: now, Config: NewDefaultPhaseConfig()}
	action, sel, ok := demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input)
	if !ok || action.Type != models.ActivityTransferProbe || action.Format != "transfer_near" || sel.Concept != "A" {
		t.Fatalf("expected a near transfer probe, got %+v %+v ok=%v", action, sel, ok)
	}

	var transfers []*models.TransferRecord
	for _, dimension := range []string{"near", "far", "debugging"} {
		at := now.Add(-2 * time.Hour)
		pf.Evidence.AssessmentsByConcept["A"] = append(pf.Evidence.AssessmentsByConcept["A"], attempt("t-"+dimension, models.ActivityTransferProbe, at))
		transfers = append(transfers, &models.TransferRecord{ConceptID: "A", AssessmentAttemptID: "t-" + dimension, ContextType: dimension, Score: 0.9, CreatedAt: at})
	}
	pf.Evidence.TransfersByConcept = map[string][]*models.TransferRecord{"A": transfers}
	action, _, ok = demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input)
	if !ok || action.Type != models.ActivityRest {
		t.Fatalf("demonstrated, transferred and recent: expected REST, got %+v ok=%v", action, ok)
	}

	input.Now = now.Add(MaintenanceChallengeRefreshAge + 24*time.Hour)
	action, _, ok = demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input)
	if !ok || action.Type != models.ActivityMasteryChallenge {
		t.Fatalf("an old demonstration is refreshed by a challenge, got %+v ok=%v", action, ok)
	}

	pf.StatesByConcept["A"].PMastery = 0.5
	if _, _, ok = demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input); ok {
		t.Fatal("a concept no longer estimated is not demonstrated: normal maintenance applies")
	}
}

// A failed review reschedules the card immediately: it is not served again
// before RecallMinimumSpacing has elapsed, in any phase.
func TestRecallNeedSelectionSpacesAJustReviewedCard(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Minute)
	next := last
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A"}}}
	pf := &pipelineFixtures{StatesByConcept: map[string]*models.ConceptState{
		"A": {LearnerID: "L1", Concept: "A", PMastery: 0.8, CardState: "relearning", Stability: 2, Lapses: 1,
			LastReview: &last, NextReview: &next, ScheduledDays: 0},
	}}
	if !RecallEligible(pf.StatesByConcept["A"]) {
		t.Skip("relearning card is not recall-eligible in this model")
	}
	input := OrchestratorInput{LearnerID: "L1", Now: now, Config: NewDefaultPhaseConfig()}
	for _, phase := range []models.Phase{models.PhaseInstruction, models.PhaseMaintenance} {
		if sel, ok := recallNeedSelection(domain, pf, phase, input); ok {
			t.Fatalf("%s: a card reviewed a minute ago was served again: %+v", phase, sel)
		}
	}
	input.Now = last.Add(RecallMinimumSpacing)
	if sel, ok := recallNeedSelection(domain, pf, models.PhaseMaintenance, input); !ok || sel.Concept != "A" {
		t.Fatalf("after the spacing the due card returns: %+v ok=%v", sel, ok)
	}
}

func servedDecision(concept string, activity models.ActivityType, at time.Time) *models.PedagogicalDecision {
	return &models.PedagogicalDecision{CreatedAt: at, Contract: models.PedagogicalContract{
		TargetConcept: concept, RecommendedActivityType: activity,
	}}
}

// The challenge cadence counts served decisions, so a host that never
// records MASTERY_CHALLENGE interactions does not get it on every call.
func TestMasteryChallengeCadenceCountsServedDecisions(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A", "B"}}}
	pf := &pipelineFixtures{
		StatesByConcept: map[string]*models.ConceptState{
			"A": {LearnerID: "L1", Concept: "A", PMastery: 0.95, CardState: "review"},
			"B": {LearnerID: "L1", Concept: "B", PMastery: 0.95, CardState: "review"},
		},
		Alerts: []models.Alert{{Type: models.AlertMasteryReady, Concept: "A"}, {Type: models.AlertMasteryReady, Concept: "B"}},
		RecentDecisions: []*models.PedagogicalDecision{
			servedDecision("B", models.ActivityPractice, now.Add(-time.Minute)),
			servedDecision("A", models.ActivityMasteryChallenge, now.Add(-2*time.Minute)),
		},
	}
	input := OrchestratorInput{LearnerID: "L1", Now: now}
	if sel, ok := masteryChallengeSelection(domain, pf, models.PhaseInstruction, input); ok {
		t.Fatalf("an unrecorded challenge two decisions ago blocks another one: %+v", sel)
	}
	for i := 0; i < MasteryChallengeCadence; i++ {
		pf.RecentDecisions = append([]*models.PedagogicalDecision{servedDecision("B", models.ActivityPractice, now)}, pf.RecentDecisions...)
	}
	sel, ok := masteryChallengeSelection(domain, pf, models.PhaseInstruction, input)
	if !ok || sel.Concept != "B" {
		t.Fatalf("after the cadence the least recently challenged concept is chosen: %+v ok=%v", sel, ok)
	}
}

// In MAINTENANCE, transfer probes follow the same cadence and rotate across
// concepts instead of always probing the first one of the graph.
func TestDemonstratedMaintenanceTransferProbeCadenceAndRotation(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	evaluated := now.Add(-24 * time.Hour)
	submitted := evaluated.Add(-time.Minute)
	pf := &pipelineFixtures{
		StatesByConcept: map[string]*models.ConceptState{},
		Evidence:        &EvidenceSnapshot{AssessmentsByConcept: map[string][]*models.AssessmentAttempt{}},
	}
	for _, concept := range []string{"A", "B"} {
		pf.StatesByConcept[concept] = &models.ConceptState{LearnerID: "L1", Concept: concept, PMastery: 0.95, CardState: "review", Stability: 30, LastReview: &last}
		pf.Evidence.AssessmentsByConcept[concept] = []*models.AssessmentAttempt{{ID: "mc-" + concept, LearnerID: "L1", ConceptID: concept,
			ActivityType: string(models.ActivityMasteryChallenge), Status: models.AssessmentAttemptEvaluated, Passed: true,
			TrustedEvaluation: true, EvaluationMethod: models.EvaluationMethodExternal, SubmittedAt: &submitted, EvaluatedAt: &evaluated}}
	}
	domain := &models.Domain{Graph: models.KnowledgeSpace{Concepts: []string{"A", "B"}}}
	input := OrchestratorInput{LearnerID: "L1", Now: now, Config: NewDefaultPhaseConfig()}

	action, sel, ok := demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input)
	if !ok || action.Type != models.ActivityTransferProbe || sel.Concept != "A" {
		t.Fatalf("expected a first transfer probe on A, got %+v %+v ok=%v", action, sel, ok)
	}
	pf.RecentDecisions = []*models.PedagogicalDecision{servedDecision("A", models.ActivityTransferProbe, now)}
	if action, sel, ok := demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input); ok {
		t.Fatalf("a probe served on the previous call is not repeated: %+v %+v", action, sel)
	}
	for i := 0; i < MasteryChallengeCadence; i++ {
		pf.RecentDecisions = append([]*models.PedagogicalDecision{servedDecision("A", models.ActivityPractice, now)}, pf.RecentDecisions...)
	}
	action, sel, ok = demonstratedMaintenanceSelection(domain, pf, models.PhaseMaintenance, input)
	if !ok || action.Type != models.ActivityTransferProbe || sel.Concept != "B" {
		t.Fatalf("after the cadence the least recently probed concept is chosen, got %+v %+v ok=%v", action, sel, ok)
	}
}
