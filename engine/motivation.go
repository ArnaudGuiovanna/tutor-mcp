// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"encoding/json"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// BriefInput gathers the signals the motivation engine needs to decide which brief (if any) to fire.
// Keep this purely data — no store access — so the selection logic can be unit-tested in isolation.
type BriefInput struct {
	Domain               *models.Domain
	Concept              string
	ActivityType         models.ActivityType
	ConceptState         *models.ConceptState
	LastFailure          *models.Interaction // within 24h, same concept
	LatestAffect         *models.AffectState
	SessionsOnConcept    int
	SelfInitiatedRatio   float64
	SessionExerciseCount int  // number of concepts already practiced in current session
	PlateauActive        bool // any PLATEAU alert active on this concept
	Now                  time.Time
}

// InferInterestPhase maps session count / mastery / self-initiated ratio to a
// Hidi-Renninger phase label.
//
// The 0.85 here is *not* a BKT mastery threshold and intentionally bypasses
// algorithms.MasteryBKT() / REGULATION_THRESHOLD. It is an empirical bound
// for the "individual interest" phase from Hidi & Renninger 2006 (Four-Phase
// Model of Interest Development), which is conceptually orthogonal to the
// runtime's notion of "concept BKT-mastered". Coupling them was rejected in
// docs/regulation-design/07-threshold-resolver.md OQ-7.2.
func InferInterestPhase(sessions int, mastery, selfInitRatio float64) string {
	if mastery > 0.85 || (selfInitRatio > 0.6 && sessions >= 3) {
		return models.InterestPhaseIndividual
	}
	if mastery >= 0.5 {
		return models.InterestPhaseEmerging
	}
	if sessions >= 3 {
		return models.InterestPhaseSustained
	}
	return models.InterestPhaseTriggered
}

// crossedMilestone returns the nearest threshold (0.5, 0.7, 0.85) the current
// mastery sits within ±0.02 of, or 0 if none.
func crossedMilestone(p float64) float64 {
	thresholds := []float64{algorithms.MasteryBKT(), 0.7, 0.5}
	for _, t := range thresholds {
		if p >= t-0.02 && p <= t+0.02 {
			return t
		}
	}
	return 0
}

// shouldFireCompetenceValue fires:
//   - on the first exercise of a session when the chosen concept is new (sessions <= 1)
//   - every 5 sessions on a concept (sustained reminder of stakes)
//   - when a milestone was also just crossed (combined emphasis)
func shouldFireCompetenceValue(in BriefInput, milestone float64) bool {
	if in.Domain == nil {
		return false
	}
	if milestone > 0 {
		return true
	}
	if in.SessionsOnConcept <= 1 && in.SessionExerciseCount <= 1 {
		return true
	}
	if in.SessionsOnConcept > 0 && in.SessionsOnConcept%5 == 0 && in.SessionExerciseCount <= 1 {
		return true
	}
	return false
}

// nextValueAxis picks the next axis in round-robin order, skipping axes whose
// authored statement is empty if at least one is populated (prefer authored content).
func nextValueAxis(lastAxis string, framings *models.DomainValueFramings) string {
	axes := models.ValueAxes
	// Determine starting index (one past lastAxis)
	start := 0
	for i, a := range axes {
		if a == lastAxis {
			start = (i + 1) % len(axes)
			break
		}
	}

	// If any authored statement exists, prefer the next authored axis.
	hasAuthored := framings != nil && (framings.Financial != "" || framings.Employment != "" || framings.Intellectual != "" || framings.Innovation != "")
	if hasAuthored {
		for i := 0; i < len(axes); i++ {
			a := axes[(start+i)%len(axes)]
			if framings.StatementFor(a) != "" {
				return a
			}
		}
	}
	return axes[start]
}

// SelectBrief chooses which motivation brief kind to fire (or "" for silence).
// Priority order (first match wins):
//  1. milestone         — just crossed a mastery threshold
//  2. competence_value  — reminder of the concrete gain this skill unlocks
//  3. growth_mindset    — a failure occurred on this concept within 24h
//  4. affect_reframe    — the latest end-of-session affect was negative
//  5. plateau_recontext — plateau detected on this concept
//  6. why_this_exercise — utility-value fallback, linked to personal_goal
func SelectBrief(in BriefInput) string {
	milestone := 0.0
	if in.ConceptState != nil {
		milestone = crossedMilestone(in.ConceptState.PMastery)
	}
	if milestone > 0 {
		return models.MotivationKindMilestone
	}
	if shouldFireCompetenceValue(in, milestone) {
		return models.MotivationKindCompetenceValue
	}
	if in.LastFailure != nil && !in.LastFailure.Success {
		return models.MotivationKindGrowthMindset
	}
	if affectIsNegative(in.LatestAffect, in.Now) {
		return models.MotivationKindAffectReframe
	}
	if in.PlateauActive {
		return models.MotivationKindPlateauRecontext
	}
	if in.Domain != nil && in.Domain.PersonalGoal != "" &&
		(in.ActivityType == models.ActivityNewConcept || in.SessionExerciseCount <= 1) {
		return models.MotivationKindWhyThisExercise
	}
	return ""
}

// affectIsNegative checks whether the most recent affect (within the last ~24h)
// carries a clear negative signal. Returns false if there is no affect state or
// if it is older than 24h.
func affectIsNegative(a *models.AffectState, now time.Time) bool {
	if a == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.Sub(a.CreatedAt) > 24*time.Hour {
		return false
	}
	if a.Satisfaction > 0 && a.Satisfaction <= 2 {
		return true
	}
	if a.PerceivedDifficulty == 4 {
		return true
	}
	if a.Energy > 0 && a.Energy <= 1 {
		return true
	}
	return false
}

// ComposeBrief builds a MotivationBrief from the given signals and the chosen kind.
// The caller is responsible for persisting Domain.LastValueAxis when kind == competence_value.
func ComposeBrief(in BriefInput, kind, pickedAxis string) *models.MotivationBrief {
	if kind == "" {
		return &models.MotivationBrief{Kind: ""}
	}

	brief := &models.MotivationBrief{Kind: kind}
	if in.ConceptState != nil {
		brief.InterestPhase = InferInterestPhase(in.SessionsOnConcept, in.ConceptState.PMastery, in.SelfInitiatedRatio)
	}

	switch kind {
	case models.MotivationKindMilestone:
		threshold := 0.0
		if in.ConceptState != nil {
			threshold = crossedMilestone(in.ConceptState.PMastery)
			brief.ProgressDelta = &models.ProgressDelta{
				Concept:    in.Concept,
				MasteryNow: in.ConceptState.PMastery,
				Threshold:  threshold,
			}
		}
		brief.Instruction = "Briefly celebrate crossing the threshold, without excessive emphasis. Tie it to overall progress in one sentence."

	case models.MotivationKindCompetenceValue:
		var framings *models.DomainValueFramings
		if in.Domain != nil && in.Domain.ValueFramingsJSON != "" {
			var parsed models.DomainValueFramings
			if err := json.Unmarshal([]byte(in.Domain.ValueFramingsJSON), &parsed); err == nil {
				framings = &parsed
			}
		}
		statement := ""
		if framings != nil {
			statement = framings.StatementFor(pickedAxis)
		}
		brief.ValueFraming = &models.ValueFraming{
			Axis:      pickedAxis,
			Statement: statement,
		}
		if statement == "" {
			brief.Instruction = "Compose one sentence highlighting the concrete gain on the " + pickedAxis + " axis from mastering this concept. Then ground it by reminding the user this is one view among others."
		} else {
			brief.Instruction = "Integrate the gain on the " + pickedAxis + " axis in one sentence, tied to the exercise concept. No invented numbers, no verbatim copy of the statement."
		}
		if in.Domain != nil {
			brief.GoalLink = in.Domain.PersonalGoal
		}

	case models.MotivationKindGrowthMindset:
		if in.LastFailure != nil {
			fm := &models.FailureMeta{
				Concept:           in.LastFailure.Concept,
				HintsRequested:    in.LastFailure.HintsRequested,
				ErrorType:         in.LastFailure.ErrorType,
				MisconceptionType: in.LastFailure.MisconceptionType,
				HoursAgo:          int(in.Now.Sub(in.LastFailure.CreatedAt).Hours()),
			}
			if in.Now.IsZero() {
				fm.HoursAgo = int(time.Since(in.LastFailure.CreatedAt).Hours())
			}
			brief.FailureContext = fm
		}
		brief.Instruction = "Reframe l'echec precedent en termes d'effort/strategie, jamais d'aptitude personnelle. Une phrase courte qui valide et relance."

	case models.MotivationKindAffectReframe:
		if in.LatestAffect != nil {
			am := &models.AffectMeta{SessionID: in.LatestAffect.SessionID}
			switch {
			case in.LatestAffect.Satisfaction > 0 && in.LatestAffect.Satisfaction <= 2:
				am.Dimension = "satisfaction"
				am.Value = in.LatestAffect.Satisfaction
			case in.LatestAffect.PerceivedDifficulty == 4:
				am.Dimension = "difficulty"
				am.Value = in.LatestAffect.PerceivedDifficulty
			case in.LatestAffect.Energy > 0 && in.LatestAffect.Energy <= 1:
				am.Dimension = "energy"
				am.Value = in.LatestAffect.Energy
			}
			am.HoursAgo = int(in.Now.Sub(in.LatestAffect.CreatedAt).Hours())
			if in.Now.IsZero() {
				am.HoursAgo = int(time.Since(in.LatestAffect.CreatedAt).Hours())
			}
			brief.AffectContext = am
		}
		brief.Instruction = "Valide d'abord l'emotion detectee, puis reframe brievement (frustration = bord du ZPD ; fatigue = sois plus leger). Une phrase."

	case models.MotivationKindPlateauRecontext:
		brief.Instruction = "Propose un angle d'attaque different pour sortir du plateau. Pas de discours general, un hook concret sur ce concept precis."

	case models.MotivationKindWhyThisExercise:
		if in.Domain != nil {
			brief.GoalLink = in.Domain.PersonalGoal
		}
		brief.Instruction = "Tie exercise -> concept -> goal_link in ONE sentence. No more, no less."
	}

	return brief
}

// MotivationEngine wires SelectBrief / ComposeBrief to the persistence layer and
// handles side effects (e.g., rotating the domain's last_value_axis).
type MotivationEngine struct {
	store storeport.Store
}

func NewMotivationEngine(store storeport.Store) *MotivationEngine {
	return &MotivationEngine{store: store}
}

// Build gathers signals from the store, selects a brief kind, composes the brief,
// and persists side effects (rotates Domain.LastValueAxis when a competence_value
// brief fires). Returns a brief with Kind == "" when no trigger matches.
func (m *MotivationEngine) Build(ctx context.Context, learnerID string, domain *models.Domain, concept string, activityType models.ActivityType, plateauActive bool, sessionExerciseCount int) (*models.MotivationBrief, error) {
	now := time.Now().UTC()

	in := BriefInput{
		Domain:               domain,
		Concept:              concept,
		ActivityType:         activityType,
		SessionExerciseCount: sessionExerciseCount,
		PlateauActive:        plateauActive,
		Now:                  now,
	}

	// Concept-scoped signals (only if we have a concept target)
	if concept != "" {
		domainID := ""
		if domain != nil {
			domainID = domain.ID
		}
		cs, _ := m.store.GetConceptStateInDomain(ctx, learnerID, domainID, concept)
		in.ConceptState = cs

		if fail, _ := m.store.LastFailureOnConceptInDomain(ctx, learnerID, domainID, concept, 24*time.Hour); fail != nil {
			in.LastFailure = fail
		}
		if sessions, err := m.store.CountSessionsOnConceptInDomain(ctx, learnerID, domainID, concept); err == nil {
			in.SessionsOnConcept = sessions
		}
		if ratio, err := m.store.SelfInitiatedRatioInDomain(ctx, learnerID, domainID, concept); err == nil {
			in.SelfInitiatedRatio = ratio
		}
	}

	// Latest affect (global, not concept-scoped)
	if affects, _ := m.store.GetRecentAffectStates(ctx, learnerID, 1); len(affects) > 0 {
		in.LatestAffect = affects[0]
	}

	kind := SelectBrief(in)

	// Rotate axis if kind is competence_value
	pickedAxis := ""
	if kind == models.MotivationKindCompetenceValue && domain != nil {
		var framings *models.DomainValueFramings
		if domain.ValueFramingsJSON != "" {
			var parsed models.DomainValueFramings
			if err := json.Unmarshal([]byte(domain.ValueFramingsJSON), &parsed); err == nil {
				framings = &parsed
			}
		}
		pickedAxis = nextValueAxis(domain.LastValueAxis, framings)
		_ = m.store.UpdateDomainLastValueAxis(ctx, domain.ID, pickedAxis)
	}

	return ComposeBrief(in, kind, pickedAxis), nil
}
