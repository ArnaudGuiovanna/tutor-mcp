// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestResolveReviewIntentActivity_PipelineRetentionFirstNoNewConcept(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeReviewIntentDomain(t, store, []string{"low", "high", "fresh"})
	now := time.Now().UTC()
	low := seedReviewIntentState(t, store, "low", 0.99, 1, 40, "review")
	high := seedReviewIntentState(t, store, "high", 0.99, 20, 1, "review")

	activity, phase, status, err := resolveReviewIntentActivity(context.Background(), store, "L_owner", d, []*models.ConceptState{low, high}, nil, nil, nil, now)
	if err != nil {
		t.Fatalf("resolve review intent: %v", err)
	}
	if phase != models.PhaseMaintenance {
		t.Fatalf("phase = %q, want MAINTENANCE", phase)
	}
	if status != reviewIntentStatusApplied {
		t.Fatalf("status = %q, want applied", status)
	}
	if activity.Concept != "low" {
		t.Fatalf("concept = %q, want low-retention reviewed concept; activity=%+v", activity.Concept, activity)
	}
	if activity.Type == models.ActivityNewConcept {
		t.Fatalf("review intent must not introduce a new concept: %+v", activity)
	}
	if !strings.Contains(activity.Rationale, "phase=MAINTENANCE") || !strings.Contains(activity.Rationale, "priority=retention") {
		t.Fatalf("expected pipeline rationale with maintenance/retention priority, got %q", activity.Rationale)
	}
}

func TestResolveReviewIntentActivity_MisconceptionBeatsRetention(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeReviewIntentDomain(t, store, []string{"retention", "misconception"})
	now := time.Now().UTC()
	retention := seedReviewIntentState(t, store, "retention", 0.99, 1, 60, "review")
	misconception := seedReviewIntentState(t, store, "misconception", 0.99, 20, 1, "review")
	seedReviewIntentInteraction(t, store, d.ID, "misconception", string(models.ActivityPractice), false, "wrong_sign")

	activity, _, status, err := resolveReviewIntentActivity(context.Background(), store, "L_owner", d, []*models.ConceptState{retention, misconception}, nil, nil, nil, now)
	if err != nil {
		t.Fatalf("resolve review intent: %v", err)
	}
	if status != reviewIntentStatusApplied {
		t.Fatalf("status = %q, want applied", status)
	}
	if activity.Concept != "misconception" {
		t.Fatalf("concept = %q, want active-misconception concept; activity=%+v", activity.Concept, activity)
	}
	if activity.Type != models.ActivityDebugMisconception {
		t.Fatalf("type = %q, want DEBUG_MISCONCEPTION; activity=%+v", activity.Type, activity)
	}
	if !strings.Contains(activity.Rationale, "priority=misconception") {
		t.Fatalf("expected misconception priority in rationale, got %q", activity.Rationale)
	}
}

func TestResolveReviewIntentActivity_NoReviewedConceptDoesNotIntroduce(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeReviewIntentDomain(t, store, []string{"fresh"})

	activity, _, status, err := resolveReviewIntentActivity(context.Background(), store, "L_owner", d, nil, nil, nil, nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve review intent: %v", err)
	}
	if status != reviewIntentStatusNoReviewable {
		t.Fatalf("status = %q, want no_reviewable_concept", status)
	}
	if activity.Type == models.ActivityNewConcept {
		t.Fatalf("review intent with no reviewed concept must not introduce: %+v", activity)
	}
	if activity.Type != models.ActivityRest {
		t.Fatalf("type = %q, want REST fallback for no reviewable concept", activity.Type)
	}
}

func TestResolveActivityDomain_DefaultCriticalRetentionOverridesPriorityRank(t *testing.T) {
	store, _ := setupToolsTest(t)
	math, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "math", "", models.KnowledgeSpace{
		Concepts:      []string{"math_new"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create math domain: %v", err)
	}
	critical, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "adaptive", "", models.KnowledgeSpace{
		Concepts:      []string{"forgotten"},
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create critical domain: %v", err)
	}
	if err := store.SetDomainPriority(context.Background(), math.ID, "L_owner", 1); err != nil {
		t.Fatalf("set math priority: %v", err)
	}
	if err := store.SetDomainPriority(context.Background(), critical.ID, "L_owner", 2); err != nil {
		t.Fatalf("set critical priority: %v", err)
	}
	seedReviewIntentState(t, store, "math_new", 0.10, 30, 1, "review")
	seedReviewIntentState(t, store, "forgotten", 0.95, 1, 80, "review")

	got, err := resolveActivityDomain(context.Background(), store, "L_owner", "", "")
	if err != nil {
		t.Fatalf("resolve default domain: %v", err)
	}
	if got.ID != critical.ID {
		t.Fatalf("default domain = %q, want critical retention domain %q", got.ID, critical.ID)
	}

	explicit, err := resolveActivityDomain(context.Background(), store, "L_owner", math.ID, "")
	if err != nil {
		t.Fatalf("resolve explicit domain: %v", err)
	}
	if explicit.ID != math.ID {
		t.Fatalf("explicit domain = %q, want math domain %q", explicit.ID, math.ID)
	}

	byName, err := resolveActivityDomain(context.Background(), store, "L_owner", "", "math")
	if err != nil {
		t.Fatalf("resolve domain by name: %v", err)
	}
	if byName.ID != math.ID {
		t.Fatalf("domain_name selection = %q, want math domain %q", byName.ID, math.ID)
	}
}

func TestResolveReviewIntentActivity_ConstrainsHighMasteryRotation(t *testing.T) {
	store, _ := setupToolsTest(t)
	d := makeReviewIntentDomain(t, store, []string{"stable"})
	now := time.Now().UTC()
	stable := seedReviewIntentState(t, store, "stable", 0.99, 20, 1, "review")
	for i := 0; i < 3; i++ {
		seedReviewIntentInteraction(t, store, d.ID, "stable", string(models.ActivityPractice), true, "")
	}

	activity, _, status, err := resolveReviewIntentActivity(context.Background(), store, "L_owner", d, []*models.ConceptState{stable}, nil, nil, nil, now)
	if err != nil {
		t.Fatalf("resolve review intent: %v", err)
	}
	if status != reviewIntentStatusApplied {
		t.Fatalf("status = %q, want applied", status)
	}
	if !isReviewIntentAllowedActivityType(activity.Type) {
		t.Fatalf("review intent emitted unsupported type %q: %+v", activity.Type, activity)
	}
	if activity.Type == models.ActivityMasteryChallenge || activity.Type == models.ActivityFeynmanPrompt || activity.Type == models.ActivityTransferProbe {
		t.Fatalf("review intent must constrain high-mastery rotation, got %+v", activity)
	}
	if !strings.Contains(activity.Rationale, "review intent constraint") {
		t.Fatalf("expected action-family constraint in rationale, got %q", activity.Rationale)
	}
}

func makeReviewIntentDomain(t *testing.T, store *db.Store, concepts []string) *models.Domain {
	t.Helper()
	d, err := store.CreateDomainWithValueFramings(context.Background(), "L_owner", "review-intent", "", models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	}, "")
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	return d
}

func seedReviewIntentState(t *testing.T, store *db.Store, concept string, mastery, stability float64, elapsedDays int, cardState string) *models.ConceptState {
	t.Helper()
	lastReview := time.Now().UTC().Add(-time.Duration(elapsedDays) * 24 * time.Hour)
	cs := models.NewConceptState("L_owner", concept)
	cs.PMastery = mastery
	cs.Stability = stability
	cs.ElapsedDays = elapsedDays
	cs.Reps = 2
	cs.CardState = cardState
	cs.LastReview = &lastReview
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatalf("upsert concept state %q: %v", concept, err)
	}
	return cs
}

func seedReviewIntentInteraction(t *testing.T, store *db.Store, domainID, concept, activityType string, success bool, misconception string) {
	t.Helper()
	i := &models.Interaction{
		LearnerID:           "L_owner",
		Concept:             concept,
		ActivityType:        activityType,
		Success:             success,
		ResponseTime:        30,
		Confidence:          0.6,
		DomainID:            domainID,
		MisconceptionType:   misconception,
		MisconceptionDetail: "seeded from review intent test",
	}
	if err := store.CreateInteraction(context.Background(), i); err != nil {
		t.Fatalf("create interaction %q: %v", concept, err)
	}
}
