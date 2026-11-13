package engine

import (
	"testing"
	"time"

	"tutor-mcp/models"
)

func newDomainWithGoal(goal string) *models.Domain {
	return &models.Domain{ID: "D1", Name: "Go", PersonalGoal: goal}
}

// TestInferInterestPhase covers all four phases.
func TestInferInterestPhase(t *testing.T) {
	cases := []struct {
		name          string
		sessions      int
		mastery       float64
		selfInitRatio float64
		want          string
	}{
		{"beginner", 1, 0.1, 0.0, models.InterestPhaseTriggered},
		{"a few sessions, low mastery", 4, 0.3, 0.0, models.InterestPhaseSustained},
		{"emerging mastery", 5, 0.6, 0.2, models.InterestPhaseEmerging},
		{"individual via mastery", 10, 0.9, 0.1, models.InterestPhaseIndividual},
		{"individual via self-initiation", 5, 0.4, 0.7, models.InterestPhaseIndividual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferInterestPhase(tc.sessions, tc.mastery, tc.selfInitRatio)
			if got != tc.want {
				t.Errorf("phase for sessions=%d mastery=%.2f selfInit=%.2f = %q, want %q",
					tc.sessions, tc.mastery, tc.selfInitRatio, got, tc.want)
			}
		})
	}
}

// TestSelectBrief_WhyThisExercise — new concept in a domain with personal_goal fires utility-value.
func TestSelectBrief_WhyThisExercise(t *testing.T) {
	in := BriefInput{
		Domain:               newDomainWithGoal("devenir SRE"),
		Concept:              "Goroutines",
		ActivityType:         models.ActivityNewConcept,
		ConceptState:         &models.ConceptState{PMastery: 0.1},
		SessionExerciseCount: 1,
		SessionsOnConcept:    1,
		Now:                  time.Now().UTC(),
	}
	got := SelectBrief(in)
	// SessionsOnConcept == 1 means competence_value fires first (1st session rule)
	if got != models.MotivationKindCompetenceValue {
		t.Errorf("expected competence_value (1st session on concept), got %q", got)
	}
}

// TestSelectBrief_GrowthMindsetOnFailure — a fresh failure takes precedence over fallback.
func TestSelectBrief_GrowthMindsetOnFailure(t *testing.T) {
	now := time.Now().UTC()
	in := BriefInput{
		Domain:       newDomainWithGoal("objectif"),
		Concept:      "Goroutines",
		ActivityType: models.ActivityRecall,
		ConceptState: &models.ConceptState{PMastery: 0.35},
		LastFailure: &models.Interaction{
			Concept: "Goroutines", Success: false,
			HintsRequested: 2, ErrorType: "LOGIC_ERROR",
			CreatedAt: now.Add(-2 * time.Hour),
		},
		SessionExerciseCount: 3, // not 1st exercise, so why_this_exercise would not fire
		SessionsOnConcept:    4,
		Now:                  now,
	}
	got := SelectBrief(in)
	if got != models.MotivationKindGrowthMindset {
		t.Errorf("expected growth_mindset, got %q", got)
	}
}

// TestSelectBrief_AffectReframe — negative satisfaction triggers affect_reframe.
func TestSelectBrief_AffectReframe(t *testing.T) {
	now := time.Now().UTC()
	in := BriefInput{
		Domain:       newDomainWithGoal("goal"),
		Concept:      "Goroutines",
		ActivityType: models.ActivityRecall,
		ConceptState: &models.ConceptState{PMastery: 0.35},
		LatestAffect: &models.AffectState{
			SessionID: "s1", Satisfaction: 1, CreatedAt: now.Add(-3 * time.Hour),
		},
		SessionExerciseCount: 3,
		SessionsOnConcept:    4,
		Now:                  now,
	}
	got := SelectBrief(in)
	if got != models.MotivationKindAffectReframe {
		t.Errorf("expected affect_reframe, got %q", got)
	}
}

// TestSelectBrief_Milestone — mastery in the 0.85 band fires milestone, beating others.
func TestSelectBrief_Milestone(t *testing.T) {
	now := time.Now().UTC()
	in := BriefInput{
		Domain:       newDomainWithGoal("goal"),
		Concept:      "Goroutines",
		ActivityType: models.ActivityRecall,
		ConceptState: &models.ConceptState{PMastery: 0.86},
		LastFailure: &models.Interaction{
			Concept: "Goroutines", Success: false, CreatedAt: now.Add(-1 * time.Hour),
		},
		SessionExerciseCount: 3,
		SessionsOnConcept:    7,
		Now:                  now,
	}
	got := SelectBrief(in)
	if got != models.MotivationKindMilestone {
		t.Errorf("expected milestone (priority 1), got %q", got)
	}
}

// TestSelectBrief_Silent — no triggers match → empty kind.
func TestSelectBrief_Silent(t *testing.T) {
	in := BriefInput{
		Domain:               &models.Domain{ID: "D1", Name: "Go", PersonalGoal: ""}, // no goal
		Concept:              "Goroutines",
		ActivityType:         models.ActivityRecall,
		ConceptState:         &models.ConceptState{PMastery: 0.35},
		SessionExerciseCount: 5,
		SessionsOnConcept:    4, // not new, not a 5th-session trigger
		Now:                  time.Now().UTC(),
	}
	got := SelectBrief(in)
	if got != "" {
		t.Errorf("expected silent (empty), got %q", got)
	}
}

// TestSelectBrief_CompetenceValueEveryFifthSession — at session 5 with a fresh
// session_exercise_count of 1, competence_value should fire even without other triggers.
func TestSelectBrief_CompetenceValueEveryFifthSession(t *testing.T) {
	in := BriefInput{
		Domain:               newDomainWithGoal("goal"),
		Concept:              "Goroutines",
		ActivityType:         models.ActivityRecall,
		ConceptState:         &models.ConceptState{PMastery: 0.45},
		SessionExerciseCount: 1,
		SessionsOnConcept:    5,
		Now:                  time.Now().UTC(),
	}
	got := SelectBrief(in)
	if got != models.MotivationKindCompetenceValue {
		t.Errorf("expected competence_value at 5th session, got %q", got)
	}
}

// TestNextValueAxis_RoundRobin confirms the rotation is deterministic and wraps.
func TestNextValueAxis_RoundRobin(t *testing.T) {
	// No framings authored: pure rotation starting from after lastAxis.
	if got := nextValueAxis("", nil); got != "financial" {
		t.Errorf("expected first axis 'financial', got %q", got)
	}
	if got := nextValueAxis("financial", nil); got != "employment" {
		t.Errorf("expected 'employment', got %q", got)
	}
	if got := nextValueAxis("innovation", nil); got != "financial" {
		t.Errorf("expected wrap to 'financial', got %q", got)
	}
}

// TestNextValueAxis_PrefersAuthored — when some axes have authored statements,
// the rotation skips unauthored axes.
func TestNextValueAxis_PrefersAuthored(t *testing.T) {
	framings := &models.DomainValueFramings{
		Financial:    "", // unauthored
		Employment:   "good employment statement",
		Intellectual: "", // unauthored
		Innovation:   "good innovation statement",
	}
	// Starting from nothing, we should hit employment first (the first authored axis in order).
	if got := nextValueAxis("", framings); got != "employment" {
		t.Errorf("expected 'employment' (first authored), got %q", got)
	}
	// After employment, the next authored axis is innovation.
	if got := nextValueAxis("employment", framings); got != "innovation" {
		t.Errorf("expected 'innovation' (next authored), got %q", got)
	}
	// After innovation, wrap back to employment (financial and intellectual are empty).
	if got := nextValueAxis("innovation", framings); got != "employment" {
		t.Errorf("expected wrap to 'employment', got %q", got)
	}
}

// TestComposeBrief_CompetenceValue — the composed brief carries axis + statement.
func TestComposeBrief_CompetenceValue(t *testing.T) {
	domain := newDomainWithGoal("goal")
	domain.ValueFramingsJSON = `{"financial":"salaires quant 80k+"}`
	in := BriefInput{
		Domain:       domain,
		Concept:      "Goroutines",
		ConceptState: &models.ConceptState{PMastery: 0.1},
	}
	brief := composeBrief(in, models.MotivationKindCompetenceValue, "financial", nil, false)
	if brief.ValueFraming == nil {
		t.Fatal("expected ValueFraming populated")
	}
	if brief.ValueFraming.Axis != "financial" {
		t.Errorf("expected axis 'financial', got %q", brief.ValueFraming.Axis)
	}
	if brief.ValueFraming.Statement != "salaires quant 80k+" {
		t.Errorf("expected statement copied from JSON, got %q", brief.ValueFraming.Statement)
	}
}

// TestComposeBrief_Silent — empty kind yields empty brief.
func TestComposeBrief_Silent(t *testing.T) {
	brief := composeBrief(BriefInput{}, "", "", nil, false)
	if brief.Kind != "" {
		t.Errorf("expected empty kind, got %q", brief.Kind)
	}
}

// TestAffectNegative_StaleIgnored — an affect older than 24h should not trigger.
func TestAffectNegative_StaleIgnored(t *testing.T) {
	now := time.Now().UTC()
	old := &models.AffectState{Satisfaction: 1, CreatedAt: now.Add(-48 * time.Hour)}
	if affectIsNegative(old, now) {
		t.Errorf("expected stale affect to be ignored")
	}
}
