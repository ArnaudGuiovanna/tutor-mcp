// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

// Package engine — Open Learner Model layer.
//
// BuildOLMSnapshot computes a transparent snapshot of a learner's state on a
// given (active) domain: mastery distribution buckets, the most actionable
// focus concept, metacognitive signals if active, and KST progress vs the
// personal goal. This snapshot is consumed by the get_olm_snapshot MCP tool
// and by the scheduler's sendOLM dispatch.
//
// All concept-level data is filtered through ActiveDomainConceptSet so
// archived or orphan concepts never leak into the OLM.

package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// nodeFragileMasteryThreshold is the lower mastery boundary for the Fragile
// state in NodeClassify. Mirrored in db/store.go (fragileThreshold) — the
// import cycle prevents sharing. If you tune this, update both packages.
const nodeFragileMasteryThreshold = 0.30

// NodeState classifies a concept's mastery for OLM rendering. The values are
// stable JSON strings consumed by callers that surface OLM state to the
// learner (dashboards, webhooks).
type NodeState string

const (
	NodeNotStarted NodeState = "not_started"
	NodeFragile    NodeState = "fragile"
	NodeInProgress NodeState = "in_progress"
	NodeEstimated  NodeState = "estimated"
	// NodeSolid retains the stable legacy classifier value. The full OLM only
	// counts strong evidence as demonstrated/solid; use MasteryStatus for the
	// authoritative distinction between estimated and demonstrated.
	NodeSolid NodeState = "solid"
	NodeFocus NodeState = "focus"
)

// NodeClassify maps a concept_state to its OLM node state, matching the
// classification logic of BuildOLMSnapshot's bucket loop. The Focus state is
// applied separately by the caller (when this concept is the snapshot focus).
//
// Rules (in order):
//   - nil OR CardState == "new"             → NotStarted
//   - retention(current age, stability) < 0.50         → Fragile
//   - PMastery >= MasteryKST() (unified 0.85 threshold) → Estimated
//   - PMastery < 0.30                                   → Fragile
//   - otherwise                              → InProgress
func NodeClassify(cs *models.ConceptState) NodeState {
	return NodeClassifyAt(cs, time.Now().UTC())
}

// NodeClassifyAt is the clock-injected OLM classification. Retention is based
// on the current age derived from LastReview, never on historical ElapsedDays.
func NodeClassifyAt(cs *models.ConceptState, now time.Time) NodeState {
	if cs == nil || cs.CardState == "new" {
		return NodeNotStarted
	}
	if algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability) < 0.50 {
		return NodeFragile
	}
	// This classifier only sees model state, not assessment evidence. It may
	// therefore surface a high estimate, but can never call it demonstrated or
	// solid. BuildOLMSnapshot owns the evidence-bearing solid counter.
	if cs.PMastery >= algorithms.MasteryKST() {
		return NodeEstimated
	}
	if cs.PMastery < nodeFragileMasteryThreshold {
		return NodeFragile
	}
	return NodeInProgress
}

// Discord embed colors for FormatOLMEmbed, keyed on FocusUrgency.
const (
	colorCritical = 0xFF6B6B // red
	colorWarning  = 0xF5A623 // amber
	colorInfo     = 0xEB459E // pink (default / info)
)

// OLMSnapshot is the structured Open Learner Model returned to the LLM in
// session and rendered by the scheduler when no LLM-authored copy is queued.
// Fields with zero values mean "no signal" — the LLM and the Go fallback
// template both use the empty string / zero count to skip the corresponding
// line.
type OLMSnapshot struct {
	DomainID     string `json:"domain_id"`
	DomainName   string `json:"domain_name"`
	PersonalGoal string `json:"personal_goal,omitempty"`

	// Evidence ladder on active concepts. Counts are cumulative: transferred is
	// also demonstrated, retained and estimated. Solid is a deprecated alias for
	// Demonstrated kept for response compatibility.
	Estimated     int                      `json:"estimated"`
	Retained      int                      `json:"retained"`
	Demonstrated  int                      `json:"demonstrated"`
	Transferred   int                      `json:"transferred"`
	Solid         int                      `json:"solid"`
	MasteryStages map[string]MasteryStatus `json:"mastery_stages"`

	// Mutually exclusive display buckets for concepts below demonstrated.
	InProgress int `json:"in_progress"`
	Fragile    int `json:"fragile"`
	NotStarted int `json:"not_started"`

	// Focus — the most actionable concept right now. Empty string if nothing
	// is actionable on this domain.
	FocusConcept string              `json:"focus_concept,omitempty"`
	FocusReason  string              `json:"focus_reason,omitempty"`
	FocusUrgency models.AlertUrgency `json:"focus_urgency,omitempty"`

	// Metacognitive signals — empty string / zero means "no actionable signal".
	AutonomyTrend      string  `json:"autonomy_trend,omitempty"` // "improving" | "stable" | "declining"
	CalibrationBias    float64 `json:"calibration_bias"`
	CalibrationSamples int     `json:"calibration_samples"`
	AffectTrend        string  `json:"affect_trend,omitempty"` // "improving" | "stable" | "declining"

	// EvidenceProgress is the demonstrated fraction of active competencies.
	// KSTProgress is a deprecated response alias retained for older clients; it
	// carries the same evidence-backed value and is not a graph-threshold count.
	EvidenceProgress float64 `json:"evidence_progress"` // 0..1
	KSTProgress      float64 `json:"kst_progress"`      // deprecated alias
	NextStepConcept  string  `json:"next_step_concept,omitempty"`

	// HasActionable: true if anything in this snapshot is worth surfacing.
	// The scheduler skips dispatch when false (silence ≠ panne).
	HasActionable bool `json:"has_actionable"`
}

// BuildOLMSnapshot composes an OLMSnapshot for the given (learner, domain).
// If domainID is empty, the most recently created non-archived domain is used.
// Returns an error if no active domain exists or the requested domain is
// archived.
func BuildOLMSnapshot(ctx context.Context, store storeport.Store, learnerID, domainID string) (*OLMSnapshot, error) {
	return BuildOLMSnapshotAt(ctx, store, learnerID, domainID, time.Now().UTC())
}

// BuildOLMSnapshotAt is the clock-injected OLM builder used where a caller
// already owns a decision timestamp (orchestrator/scheduler/tests).
func BuildOLMSnapshotAt(ctx context.Context, store storeport.Store, learnerID, domainID string, now time.Time) (*OLMSnapshot, error) {
	domain, err := resolveActiveDomain(ctx, store, learnerID, domainID)
	if err != nil {
		return nil, err
	}

	snap := &OLMSnapshot{
		DomainID:      domain.ID,
		DomainName:    domain.Name,
		PersonalGoal:  domain.PersonalGoal,
		MasteryStages: make(map[string]MasteryStatus, len(domain.Graph.Concepts)),
	}

	allStates, err := store.GetConceptStatesByDomain(ctx, learnerID, domain.ID)
	if err != nil {
		return nil, fmt.Errorf("olm: get states: %w", err)
	}
	statesByConcept := make(map[string]*models.ConceptState, len(allStates))
	for _, cs := range allStates {
		statesByConcept[cs.Concept] = cs
	}

	// Focus: alerts (forgetting, ZPD, plateau) win over frontier fallback.
	// Filter states/interactions to this domain's concepts only — alerts on
	// concepts from other domains are not relevant to this OLM.
	domainConceptSet := make(map[string]bool, len(domain.Graph.Concepts))
	for _, c := range domain.Graph.Concepts {
		domainConceptSet[c] = true
	}
	var domainStates []*models.ConceptState
	for _, cs := range allStates {
		if domainConceptSet[cs.Concept] {
			domainStates = append(domainStates, cs)
		}
	}
	recent, err := store.GetRecentInteractionsByDomain(ctx, learnerID, domain.ID, DefaultRecentInteractionsWindow)
	if err != nil {
		return nil, fmt.Errorf("olm: get interactions: %w", err)
	}
	var domainInteractions []*models.Interaction
	for _, in := range recent {
		if domainConceptSet[in.Concept] {
			domainInteractions = append(domainInteractions, in)
		}
	}

	// Authoritative mastery ladder. A high BKT estimate alone is represented as
	// estimated; only strong, retained evidence becomes demonstrated/solid.
	for _, concept := range domain.Graph.Concepts {
		transfers, err := store.GetTransferScoresInDomain(ctx, learnerID, domain.ID, concept)
		if err != nil {
			return nil, fmt.Errorf("olm: get transfer evidence for %s: %w", concept, err)
		}
		assessments, err := store.GetEvaluatedAssessmentAttemptsInDomain(ctx, learnerID, domain.ID, concept, 100)
		if err != nil {
			return nil, fmt.Errorf("olm: get evaluated assessment evidence for %s: %w", concept, err)
		}
		status := AssessMasteryStatus(learnerID, concept, statesByConcept[concept], domainInteractions, transfers, assessments, now)
		snap.MasteryStages[concept] = status
		if status.Estimated {
			snap.Estimated++
		}
		if status.Retained {
			snap.Retained++
		}
		if status.Demonstrated {
			snap.Demonstrated++
		}
		if status.Transferred {
			snap.Transferred++
		}

		cs := statesByConcept[concept]
		switch {
		case cs == nil || cs.CardState == "new":
			snap.NotStarted++
		case status.Demonstrated:
			snap.Solid++
		case cs.PMastery < nodeFragileMasteryThreshold || status.RetentionEstimate < algorithms.RetentionRecallRoutingThreshold:
			snap.Fragile++
		default:
			snap.InProgress++
		}
	}
	alertEvidence, err := LoadMasteryAlertEvidence(ctx, store, learnerID, domain.ID, domainStates)
	if err != nil {
		return nil, fmt.Errorf("olm: load alert evidence: %w", err)
	}
	alerts := ComputeAlertsWithEvidenceAt(domainStates, domainInteractions, alertEvidence, time.Time{}, now)

	if focus := pickFocus(alerts); focus != nil {
		snap.FocusConcept = focus.Concept
		snap.FocusReason = formatFocusReason(*focus)
		snap.FocusUrgency = focus.Urgency
	} else {
		// Frontier fallback.
		mastery := make(map[string]float64, len(domainStates))
		for _, cs := range domainStates {
			mastery[cs.Concept] = cs.PMastery
		}
		graph := algorithms.KSTGraph{
			Concepts:      domain.Graph.Concepts,
			Prerequisites: domain.Graph.Prerequisites,
		}
		frontier := algorithms.ComputeFrontier(graph, mastery)
		if len(frontier) > 0 {
			snap.FocusConcept = frontier[0]
			snap.FocusReason = "next frontier"
			snap.FocusUrgency = models.UrgencyInfo
		}
	}

	// Metacognitive signals — only set if a clear trend exists across the
	// last 3 affects (or if calibration bias exceeds the actionable threshold).
	affects, err := store.GetRecentAffectStates(ctx, learnerID, 3)
	if err != nil {
		return nil, fmt.Errorf("olm: get affect history: %w", err)
	}
	if len(affects) >= 3 {
		snap.AutonomyTrend = trendDirection(affects[0].AutonomyScore-affects[2].AutonomyScore, 0.10)
		// Satisfaction is a 1..4 Likert; require a ≥2-step move before calling it a trend.
		snap.AffectTrend = trendDirection(float64(affects[0].Satisfaction-affects[2].Satisfaction), 1.5)
	}
	calibrationHistory, err := store.GetCalibrationBiasHistoryInDomain(ctx, learnerID, domain.ID, 20)
	if err != nil {
		return nil, fmt.Errorf("olm: get calibration history: %w", err)
	}
	for _, delta := range calibrationHistory {
		snap.CalibrationBias += delta
	}
	if len(calibrationHistory) > 0 {
		snap.CalibrationBias /= float64(len(calibrationHistory))
	}
	snap.CalibrationSamples = len(calibrationHistory)

	// Progress is evidence-backed: fraction of active concepts demonstrated,
	// rather than the fraction whose BKT estimate merely crossed a threshold.
	totalConcepts := snap.Solid + snap.InProgress + snap.Fragile + snap.NotStarted
	if totalConcepts > 0 {
		snap.EvidenceProgress = float64(snap.Demonstrated) / float64(totalConcepts)
		snap.KSTProgress = snap.EvidenceProgress
	}

	// HasActionable: a focus exists OR a metacog signal warrants surfacing.
	if snap.FocusConcept != "" ||
		calibrationBiasIsActionable(snap.CalibrationBias, snap.CalibrationSamples) ||
		snap.AutonomyTrend == "declining" ||
		snap.AffectTrend == "declining" {
		snap.HasActionable = true
	}

	return snap, nil
}

// trendDirection returns "improving", "stable", or "declining" based on the
// signed difference between newest and oldest. Threshold defines the dead band:
// |diff| < threshold → "stable".
func trendDirection(diff, threshold float64) string {
	switch {
	case diff > threshold:
		return "improving"
	case diff < -threshold:
		return "declining"
	default:
		return "stable"
	}
}

// resolveActiveDomain returns the domain to use for the OLM. If domainID is
// empty, picks the most recently created non-archived domain. Returns an error
// if the learner has no active domain, or the requested domain is archived.
func resolveActiveDomain(ctx context.Context, store storeport.Store, learnerID, domainID string) (*models.Domain, error) {
	if domainID == "" {
		domains, err := store.GetDomainsByLearner(ctx, learnerID, false /*includeArchived*/)
		if err != nil {
			return nil, fmt.Errorf("olm: list domains: %w", err)
		}
		if len(domains) == 0 {
			return nil, fmt.Errorf("olm: no active domain for learner %s", learnerID)
		}
		return domains[0], nil
	}
	d, err := store.GetDomainByID(ctx, domainID)
	if err != nil {
		return nil, fmt.Errorf("olm: get domain %s: %w", domainID, err)
	}
	if d == nil || d.LearnerID != learnerID {
		return nil, fmt.Errorf("olm: domain %s not found for learner", domainID)
	}
	if d.Archived {
		return nil, fmt.Errorf("olm: domain %s is archived", domainID)
	}
	return d, nil
}

// pickFocus returns the most actionable alert by descending priority:
// FORGETTING (critical first) > ZPD_DRIFT > PLATEAU. Returns nil if no
// such alert is present. Other alert types (e.g., MASTERY_READY, OVERLOAD)
// are not focus-worthy for the OLM.
func pickFocus(alerts []models.Alert) *models.Alert {
	var critical, warning, plateau, zpd *models.Alert
	for i := range alerts {
		a := &alerts[i]
		switch a.Type {
		case models.AlertForgetting:
			if a.Urgency == models.UrgencyCritical && critical == nil {
				critical = a
			} else if warning == nil {
				warning = a
			}
		case models.AlertZPDDrift:
			if zpd == nil {
				zpd = a
			}
		case models.AlertPlateau:
			if plateau == nil {
				plateau = a
			}
		}
	}
	switch {
	case critical != nil:
		return critical
	case warning != nil:
		return warning
	case zpd != nil:
		return zpd
	case plateau != nil:
		return plateau
	}
	return nil
}

// formatFocusReason produces the short reason string shown next to the focus
// concept in the OLM message.
func formatFocusReason(a models.Alert) string {
	switch a.Type {
	case models.AlertForgetting:
		return fmt.Sprintf("retention %.0f%%", a.Retention*100)
	case models.AlertZPDDrift:
		return fmt.Sprintf("%.0f%% recent errors", a.ErrorRate*100)
	case models.AlertPlateau:
		return fmt.Sprintf("plateau %d sessions", a.SessionsStalled)
	}
	return a.RecommendedAction
}

// FormatOLMEmbed renders an OLMSnapshot as a Discord embed, used by the
// scheduler when no LLM-authored copy is queued. The text is intentionally
// factual: distribution + focus + (one) metacognitive line if active +
// goal progress phrase. No pep talk.
func FormatOLMEmbed(snap *OLMSnapshot) DiscordEmbed {
	title := "🧭 Current state"
	color := colorInfo
	switch snap.FocusUrgency {
	case models.UrgencyCritical:
		title = "🚨 State — one concept needs attention now"
		color = colorCritical
	case models.UrgencyWarning:
		color = colorWarning
	}

	var lines []string

	buckets := compactBuckets(snap)
	if buckets != "" {
		lines = append(lines, fmt.Sprintf("For **%s**:\n%s.", snap.DomainName, buckets))
	}

	if snap.FocusConcept != "" {
		var prefix string
		switch snap.FocusUrgency {
		case models.UrgencyCritical:
			prefix = "One concept needs attention now"
		case models.UrgencyWarning:
			prefix = "Current focus"
		default:
			prefix = "Next milestone"
		}
		lines = append(lines, fmt.Sprintf("%s : **%s** (%s).", prefix, snap.FocusConcept, snap.FocusReason))
	}

	if line := MetacogLine(snap); line != "" {
		lines = append(lines, line)
	}

	if snap.PersonalGoal != "" {
		lines = append(lines, fmt.Sprintf("Goal \"%s\": %s.", snap.PersonalGoal, progressPhrase(snap.EvidenceProgress)))
	}

	return DiscordEmbed{
		Title:       title,
		Description: strings.Join(lines, "\n\n"),
		Color:       color,
	}
}

// DiscordEmbed mirrors engine/scheduler.go's discordEmbed but is exported so
// tests in this file (and the scheduler) can both build embeds. The scheduler
// converts to its private discordEmbed by direct struct copy (same shape).
type DiscordEmbed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Color       int    `json:"color"`
}

func compactBuckets(snap *OLMSnapshot) string {
	parts := []string{}
	if snap.Solid > 0 {
		parts = append(parts, fmt.Sprintf("%d demonstrated", snap.Solid))
	}
	if snap.InProgress > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", snap.InProgress))
	}
	if snap.Fragile > 0 {
		parts = append(parts, fmt.Sprintf("%d fragile", snap.Fragile))
	}
	if snap.NotStarted > 0 {
		parts = append(parts, fmt.Sprintf("%d not started", snap.NotStarted))
	}
	return strings.Join(parts, " · ")
}

// MetacogLine returns the single most actionable metacognitive sentence,
// in descending priority: calibration bias, then autonomy trend, then affect
// trend. Empty string when no signal is active. Exported so any caller
// that needs the metacog signal can produce the same text as the webhook's
// FormatOLMEmbed.
func MetacogLine(snap *OLMSnapshot) string {
	if calibrationBiasIsActionable(snap.CalibrationBias, snap.CalibrationSamples) && snap.CalibrationBias > 0 {
		return "You have been slightly over-estimating your knowledge for 3 sessions — a few cold exercises will help you recalibrate."
	}
	if calibrationBiasIsActionable(snap.CalibrationBias, snap.CalibrationSamples) && snap.CalibrationBias < 0 {
		return "You are slightly under-estimating yourself — you know more than you think."
	}
	if snap.AutonomyTrend == "declining" {
		return "You have been leaning on hints a bit more lately — try a few exercises without help to see how you do."
	}
	if snap.AffectTrend == "declining" {
		return "The last 3 sessions have been tough — feel free to lighten the load or take a break."
	}
	return ""
}

func progressPhrase(p float64) string {
	switch {
	case p < 0.30:
		return "just getting started"
	case p < algorithms.MasteryKST():
		return "halfway there"
	default:
		return "almost there"
	}
}
