// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"tutor-mcp/algorithms"
	"tutor-mcp/db"
	"tutor-mcp/engine"
	"tutor-mcp/models"
)

const (
	activityIntentAuto   = "auto"
	activityIntentReview = "review"

	reviewIntentStatusApplied      = "applied"
	reviewIntentStatusNoReviewable = "no_reviewable_concept"
)

func normalizeActivityIntent(raw string) (string, error) {
	token := compactToken(raw)
	switch token {
	case "", "auto":
		return activityIntentAuto, nil
	case "review", "revise", "revision", "reviser", "practice", "practise":
		return activityIntentReview, nil
	default:
		return "", fmt.Errorf("unsupported intent %q: allowed values are auto, review", raw)
	}
}

func resolveActivityDomain(ctx context.Context, store *db.Store, learnerID, domainID, domainName string) (*models.Domain, error) {
	if domainID != "" || strings.TrimSpace(domainName) == "" {
		if domainID == "" {
			if d, err := resolveCriticalRetentionDomain(ctx, store, learnerID); err != nil {
				return nil, err
			} else if d != nil {
				return d, nil
			}
		}
		return resolveDomain(ctx, store, learnerID, domainID)
	}

	domains, err := store.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return nil, err
	}
	needle := compactToken(domainName)
	if needle == "" {
		return nil, fmt.Errorf("domain_name is empty after normalization")
	}

	for _, d := range domains {
		if compactToken(d.Name) == needle {
			return d, nil
		}
	}

	var matches []*models.Domain
	for _, d := range domains {
		haystack := compactToken(d.Name)
		if strings.Contains(haystack, needle) || strings.Contains(needle, haystack) {
			matches = append(matches, d)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("domain_name %q did not match an active domain; available domains: %s", domainName, domainNames(domains))
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("domain_name %q is ambiguous; matching domains: %s", domainName, domainNames(matches))
	}
}

func resolveCriticalRetentionDomain(ctx context.Context, store *db.Store, learnerID string) (*models.Domain, error) {
	domains, err := store.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return nil, err
	}
	if len(domains) == 0 {
		return nil, nil
	}

	states, err := store.GetConceptStatesByLearner(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	stateByConcept := make(map[string]*models.ConceptState, len(states))
	for _, cs := range states {
		if cs != nil {
			stateByConcept[cs.Concept] = cs
		}
	}

	var bestDomain *models.Domain
	bestRetention := 1.0
	now := time.Now().UTC()
	for _, d := range domains {
		for _, concept := range d.Graph.Concepts {
			cs := stateByConcept[concept]
			if cs == nil || cs.CardState == "new" {
				continue
			}
			elapsed := cs.ElapsedDays
			if cs.LastReview != nil {
				elapsed = int(now.Sub(*cs.LastReview).Hours() / 24)
			}
			retention := algorithms.Retrievability(elapsed, cs.Stability)
			if retention < algorithms.RetentionAlertCriticalThreshold && retention < bestRetention {
				bestRetention = retention
				bestDomain = d
			}
		}
	}
	return bestDomain, nil
}

func compactToken(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func domainNames(domains []*models.Domain) string {
	if len(domains) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(domains))
	for _, d := range domains {
		parts = append(parts, fmt.Sprintf("%s (%s)", d.Name, d.ID))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

type reviewCandidate struct {
	concept      string
	state        *models.ConceptState
	score        float64
	retention    float64
	hasActiveMis bool
}

type reviewIntentConstraints struct {
	concepts                   []string
	stateByConcept             map[string]*models.ConceptState
	seenByInteraction          map[string]bool
	activeMisconceptions       map[string]bool
	restrictedToMisconceptions bool
	sessionConceptFallbackUsed bool
}

// resolveReviewIntentActivity routes explicit review intent through the
// regulation pipeline components with review-specific constraints:
// MAINTENANCE phase, previously studied concepts only, and review-safe
// activity families for concept-bearing activities. Gate escape actions still
// win. The legacy selector is retained as a fallback for NoFringe/constraint-
// miss cases until the engine grows first-class intent input.
func resolveReviewIntentActivity(ctx context.Context, store *db.Store, learnerID string, domain *models.Domain, states []*models.ConceptState, interactions []*models.Interaction, sessionConcepts map[string]int, alerts []models.Alert, now time.Time) (models.Activity, models.Phase, string, error) {
	activity, status, err := selectReviewIntentPipelineActivity(ctx, store, learnerID, domain, states, interactions, sessionConcepts, alerts, now)
	if err != nil {
		return models.Activity{}, "", "", err
	}
	if status == reviewIntentStatusApplied || status == reviewIntentStatusNoReviewable {
		return activity, models.PhaseMaintenance, status, nil
	}

	activity, status = selectReviewIntentActivity(ctx, store, learnerID, domain, states, interactions, sessionConcepts, now)
	return activity, models.PhaseMaintenance, status, nil
}

func selectReviewIntentPipelineActivity(ctx context.Context, store *db.Store, learnerID string, domain *models.Domain, states []*models.ConceptState, interactions []*models.Interaction, sessionConcepts map[string]int, alerts []models.Alert, now time.Time) (models.Activity, string, error) {
	constraints, err := buildReviewIntentConstraints(ctx, store, learnerID, domain, states, interactions, sessionConcepts)
	if err != nil {
		return models.Activity{}, "", err
	}
	if len(constraints.concepts) == 0 {
		return reviewUnavailableActivity(), reviewIntentStatusNoReviewable, nil
	}

	gateResult, err := engine.ApplyGate(engine.GateInput{
		Phase:                models.PhaseMaintenance,
		Concepts:             constraints.concepts,
		States:               constraints.stateByConcept,
		Graph:                domain.Graph,
		ActiveMisconceptions: constraints.activeMisconceptions,
		RecentConcepts:       recentConceptsFromInteractions(interactions),
		Alerts:               alerts,
		AntiRepeatWindow:     engine.DefaultAntiRepeatWindow,
	})
	if err != nil {
		return models.Activity{}, "", fmt.Errorf("review intent gate: %w", err)
	}
	if gateResult.EscapeAction != nil {
		return composeReviewEscapeActivity(*gateResult.EscapeAction), reviewIntentStatusApplied, nil
	}
	if gateResult.NoCandidate {
		return reviewUnavailableActivity(), reviewIntentStatusNoReviewable, nil
	}

	allowedSet := make(map[string]bool, len(gateResult.AllowedConcepts))
	for _, c := range gateResult.AllowedConcepts {
		allowedSet[c] = true
	}
	var goalRelevance map[string]float64
	if gr := domain.ParseGoalRelevance(); gr != nil {
		goalRelevance = gr.Relevance
	}
	selection, err := engine.SelectConcept(models.PhaseMaintenance, states, models.KnowledgeSpace{
		Concepts:      gateResult.AllowedConcepts,
		Prerequisites: reviewPrerequisitesForAllowed(domain.Graph.Prerequisites, allowedSet),
	}, goalRelevance)
	if err != nil {
		return models.Activity{}, "", fmt.Errorf("review intent concept selector: %w", err)
	}
	if selection.NoFringe {
		return models.Activity{}, "pipeline_no_fringe", nil
	}

	cs := constraints.stateByConcept[selection.Concept]
	var mc *db.MisconceptionGroup
	if constraints.activeMisconceptions[selection.Concept] {
		mc, err = store.GetFirstActiveMisconception(ctx, learnerID, selection.Concept)
		if err != nil {
			return models.Activity{}, "", fmt.Errorf("review intent misconception fetch: %w", err)
		}
	}
	history, err := store.GetActionHistoryForConcept(ctx, learnerID, selection.Concept, 50)
	if err != nil {
		return models.Activity{}, "", fmt.Errorf("review intent action history: %w", err)
	}
	action := engine.SelectAction(selection.Concept, cs, mc, engine.ActionHistory{
		InteractionsAboveBKT:  history.InteractionsAboveBKT,
		MasteryChallengeCount: history.MasteryChallengeCount,
		FeynmanCount:          history.FeynmanCount,
		TransferCount:         history.TransferCount,
	})
	if restrictions, ok := gateResult.ActionRestriction[selection.Concept]; ok && len(restrictions) > 0 {
		if !reviewIntentContainsActivityType(restrictions, action.Type) {
			action.Type = restrictions[0]
			action.Rationale = "gate ActionRestriction override : " + action.Rationale
		}
	}
	action = constrainReviewIntentAction(selection.Concept, cs, constraints.activeMisconceptions[selection.Concept], action, now)

	activity := composeReviewPipelineActivity(action, selection, constraints)
	if !isReviewIntentAllowedActivityType(activity.Type) {
		return models.Activity{}, "pipeline_unsupported_activity", nil
	}
	if !isReviewableConcept(cs, constraints.seenByInteraction[activity.Concept]) {
		return models.Activity{}, "pipeline_unreviewable_concept", nil
	}
	return activity, reviewIntentStatusApplied, nil
}

func buildReviewIntentConstraints(ctx context.Context, store *db.Store, learnerID string, domain *models.Domain, states []*models.ConceptState, interactions []*models.Interaction, sessionConcepts map[string]int) (reviewIntentConstraints, error) {
	stateByConcept := statesByConcept(states)
	seenByInteraction := interactionConceptSet(interactions)
	activeMisconceptions, err := store.GetActiveMisconceptionsBatch(ctx, learnerID, domain.Graph.Concepts)
	if err != nil {
		return reviewIntentConstraints{}, fmt.Errorf("review intent active misconceptions: %w", err)
	}

	concepts := reviewableConceptsForDomain(domain, stateByConcept, seenByInteraction, sessionConcepts, true)
	sessionFallback := false
	if len(concepts) == 0 {
		concepts = reviewableConceptsForDomain(domain, stateByConcept, seenByInteraction, sessionConcepts, false)
		sessionFallback = len(concepts) > 0
	}

	var misconceptionConcepts []string
	for _, c := range concepts {
		if activeMisconceptions[c] {
			misconceptionConcepts = append(misconceptionConcepts, c)
		}
	}
	restrictedToMisconceptions := len(misconceptionConcepts) > 0
	if restrictedToMisconceptions {
		concepts = misconceptionConcepts
	}

	return reviewIntentConstraints{
		concepts:                   concepts,
		stateByConcept:             stateByConcept,
		seenByInteraction:          seenByInteraction,
		activeMisconceptions:       activeMisconceptions,
		restrictedToMisconceptions: restrictedToMisconceptions,
		sessionConceptFallbackUsed: sessionFallback,
	}, nil
}

func reviewableConceptsForDomain(domain *models.Domain, states map[string]*models.ConceptState, seen map[string]bool, sessionConcepts map[string]int, skipSessionConcepts bool) []string {
	if domain == nil {
		return nil
	}
	concepts := make([]string, 0, len(domain.Graph.Concepts))
	for _, concept := range domain.Graph.Concepts {
		if skipSessionConcepts && sessionConcepts[concept] > 0 {
			continue
		}
		if !isReviewableConcept(states[concept], seen[concept]) {
			continue
		}
		concepts = append(concepts, concept)
	}
	return concepts
}

func statesByConcept(states []*models.ConceptState) map[string]*models.ConceptState {
	out := make(map[string]*models.ConceptState, len(states))
	for _, cs := range states {
		if cs == nil {
			continue
		}
		out[cs.Concept] = cs
	}
	return out
}

func interactionConceptSet(interactions []*models.Interaction) map[string]bool {
	out := make(map[string]bool, len(interactions))
	for _, i := range interactions {
		if i == nil || i.Concept == "" {
			continue
		}
		out[i.Concept] = true
	}
	return out
}

func recentConceptsFromInteractions(interactions []*models.Interaction) []string {
	seen := make(map[string]bool, len(interactions))
	recent := make([]string, 0, len(interactions))
	for _, i := range interactions {
		if i == nil || i.Concept == "" || seen[i.Concept] {
			continue
		}
		seen[i.Concept] = true
		recent = append(recent, i.Concept)
	}
	return recent
}

func reviewPrerequisitesForAllowed(src map[string][]string, allowed map[string]bool) map[string][]string {
	if src == nil {
		return nil
	}
	out := make(map[string][]string, len(allowed))
	for concept := range allowed {
		if prereqs, ok := src[concept]; ok {
			out[concept] = prereqs
		}
	}
	return out
}

func constrainReviewIntentAction(concept string, cs *models.ConceptState, hasActiveMisconception bool, action engine.Action, now time.Time) engine.Action {
	if isReviewIntentAllowedActivityType(action.Type) {
		return action
	}
	if hasActiveMisconception {
		return engine.Action{
			Type:             models.ActivityDebugMisconception,
			DifficultyTarget: 0.55,
			Format:           "misconception_targeted",
			EstimatedMinutes: 12,
			Rationale:        fmt.Sprintf("review intent constraint: active misconception on %s; action selector proposed %s", concept, action.Type),
		}
	}
	if cs != nil && cs.CardState != "" && cs.CardState != "new" {
		return engine.Action{
			Type:             models.ActivityRecall,
			DifficultyTarget: 0.60,
			Format:           "retrieval_review",
			EstimatedMinutes: 8,
			Rationale:        fmt.Sprintf("review intent constraint: recall prior concept with retention %.0f%%; action selector proposed %s", reviewRetention(cs, now)*100, action.Type),
		}
	}
	return engine.Action{
		Type:             models.ActivityPractice,
		DifficultyTarget: 0.55,
		Format:           "review_practice",
		EstimatedMinutes: 10,
		Rationale:        fmt.Sprintf("review intent constraint: practice prior concept; action selector proposed %s", action.Type),
	}
}

func composeReviewPipelineActivity(action engine.Action, selection engine.Selection, constraints reviewIntentConstraints) models.Activity {
	priority := "retention"
	if constraints.restrictedToMisconceptions {
		priority = "misconception"
	}
	if constraints.sessionConceptFallbackUsed {
		priority += "+session_fallback"
	}
	return models.Activity{
		Type:             action.Type,
		Concept:          selection.Concept,
		DifficultyTarget: action.DifficultyTarget,
		Format:           action.Format,
		EstimatedMinutes: action.EstimatedMinutes,
		Rationale:        fmt.Sprintf("[intent=review phase=%s priority=%s] %s - %s", selection.Phase, priority, selection.Rationale, action.Rationale),
		PromptForLLM:     fmt.Sprintf("Generate a review activity on %s. Do not introduce a new concept; focus on retrieval, applied practice, or resolving an active misconception from prior material.", selection.Concept),
	}
}

func composeReviewEscapeActivity(esc engine.EscapeAction) models.Activity {
	return models.Activity{
		Type:         esc.Type,
		Format:       esc.Format,
		Rationale:    "[intent=review] gate escape: " + esc.Rationale,
		PromptForLLM: "Session terminee. Emets le recap_brief et appelle record_session_close.",
	}
}

func isReviewIntentAllowedActivityType(t models.ActivityType) bool {
	switch t {
	case models.ActivityRecall, models.ActivityPractice, models.ActivityDebugMisconception:
		return true
	default:
		return false
	}
}

func reviewIntentContainsActivityType(set []models.ActivityType, t models.ActivityType) bool {
	for _, candidate := range set {
		if candidate == t {
			return true
		}
	}
	return false
}

// selectReviewIntentActivity is the legacy review selector. Keep it as a
// fallback-only path for issue #146: normal integration should call
// resolveReviewIntentActivity so Gate, ConceptSelector and ActionSelector stay
// on the routing path before this selector is considered.
func selectReviewIntentActivity(ctx context.Context, store *db.Store, learnerID string, domain *models.Domain, states []*models.ConceptState, interactions []*models.Interaction, sessionConcepts map[string]int, now time.Time) (models.Activity, string) {
	candidates := reviewCandidatesForDomain(ctx, store, learnerID, domain, states, interactions, sessionConcepts, now, true)
	if len(candidates) == 0 {
		candidates = reviewCandidatesForDomain(ctx, store, learnerID, domain, states, interactions, sessionConcepts, now, false)
	}
	if len(candidates) == 0 {
		return reviewUnavailableActivity(), reviewIntentStatusNoReviewable
	}

	c := candidates[0]
	activityType := models.ActivityPractice
	format := "review_practice"
	difficulty := 0.55
	minutes := 10
	if c.hasActiveMis {
		activityType = models.ActivityDebugMisconception
		format = "targeted_review"
		difficulty = 0.6
		minutes = 12
	} else if c.state != nil && c.state.CardState != "" && c.state.CardState != "new" {
		activityType = models.ActivityRecall
		format = "retrieval_review"
		difficulty = 0.6
		minutes = 8
	}

	return models.Activity{
		Type:             activityType,
		Concept:          c.concept,
		DifficultyTarget: difficulty,
		Format:           format,
		EstimatedMinutes: minutes,
		Rationale:        fmt.Sprintf("[intent=review fallback] selected prior concept %q with retention %.2f", c.concept, c.retention),
		PromptForLLM:     fmt.Sprintf("Generate a review activity on %s. Do not introduce a new concept; focus on retrieval or applied practice from prior material.", c.concept),
	}, reviewIntentStatusApplied
}

func reviewUnavailableActivity() models.Activity {
	return models.Activity{
		Type:             models.ActivityRest,
		DifficultyTarget: 0.3,
		Format:           "review_unavailable",
		EstimatedMinutes: 3,
		Rationale:        "[intent=review] no previously studied concept is available in this domain",
		PromptForLLM:     "No reviewed concept is available in this domain. Tell the learner there is nothing to revise yet in this domain and ask whether they want to start a new concept.",
	}
}

func reviewCandidatesForDomain(ctx context.Context, store *db.Store, learnerID string, domain *models.Domain, states []*models.ConceptState, interactions []*models.Interaction, sessionConcepts map[string]int, now time.Time, skipSessionConcepts bool) []reviewCandidate {
	stateByConcept := map[string]*models.ConceptState{}
	for _, cs := range states {
		stateByConcept[cs.Concept] = cs
	}
	seenByInteraction := map[string]bool{}
	for _, i := range interactions {
		seenByInteraction[i.Concept] = true
	}

	candidates := make([]reviewCandidate, 0, len(domain.Graph.Concepts))
	for _, concept := range domain.Graph.Concepts {
		if skipSessionConcepts && sessionConcepts[concept] > 0 {
			continue
		}
		cs := stateByConcept[concept]
		if !isReviewableConcept(cs, seenByInteraction[concept]) {
			continue
		}

		retention := 1.0
		score := 0.1
		if cs != nil {
			retention = reviewRetention(cs, now)
			score += (1 - retention) * 2
			score += (1 - clampUnit(cs.PMastery)) * 0.5
			if cs.Lapses > 0 {
				score += 0.2
			}
		}
		hasMis := false
		if mis, err := store.GetFirstActiveMisconception(ctx, learnerID, concept); err == nil && mis != nil {
			hasMis = true
			score += 1.0
		}

		candidates = append(candidates, reviewCandidate{
			concept:      concept,
			state:        cs,
			score:        score,
			retention:    retention,
			hasActiveMis: hasMis,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].concept < candidates[j].concept
		}
		return candidates[i].score > candidates[j].score
	})
	return candidates
}

func isReviewableConcept(cs *models.ConceptState, seenInteraction bool) bool {
	if seenInteraction {
		return true
	}
	if cs == nil {
		return false
	}
	return cs.Reps > 0 || cs.LastReview != nil || (cs.CardState != "" && cs.CardState != "new")
}

func reviewRetention(cs *models.ConceptState, now time.Time) float64 {
	if cs == nil || cs.Stability <= 0 || cs.CardState == "" || cs.CardState == "new" {
		return 1.0
	}
	elapsedDays := cs.ElapsedDays
	if cs.LastReview != nil {
		elapsedDays = int(now.Sub(*cs.LastReview).Hours() / 24)
		if elapsedDays < 0 {
			elapsedDays = 0
		}
	}
	return clampUnit(algorithms.Retrievability(elapsedDays, cs.Stability))
}
