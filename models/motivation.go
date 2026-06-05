// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

// MotivationBrief is attached to get_next_activity responses. When Kind == ""
// no trigger fired — Claude should compose the exercise without motivational
// preamble. When populated, Claude weaves the signals into its message per
// the Motivation doctrine in the system prompt.
type MotivationBrief struct {
	Kind           string         `json:"kind"`
	GoalLink       string         `json:"goal_link,omitempty"`
	ValueFraming   *ValueFraming  `json:"value_framing,omitempty"`
	ProgressDelta  *ProgressDelta `json:"progress_delta,omitempty"`
	FailureContext *FailureMeta   `json:"failure_context,omitempty"`
	AffectContext  *AffectMeta    `json:"affect_context,omitempty"`
	InterestPhase  string         `json:"interest_phase,omitempty"`
	Instruction    string         `json:"instruction"`
}

// Motivation brief kinds, in priority order (earlier kinds win when multiple triggers fire).
const (
	MotivationKindMilestone        = "milestone"
	MotivationKindPhaseTransition  = "phase_transition"
	MotivationKindCompetenceValue  = "competence_value"
	MotivationKindGrowthMindset    = "growth_mindset"
	MotivationKindAffectReframe    = "affect_reframe"
	MotivationKindPlateauRecontext = "plateau_recontext"
	MotivationKindWhyThisExercise  = "why_this_exercise"
)

// Hidi-Renninger 4-phase interest development.
const (
	InterestPhaseTriggered  = "triggered"
	InterestPhaseSustained  = "sustained"
	InterestPhaseEmerging   = "emerging"
	InterestPhaseIndividual = "individual"
)

// ValueFraming — Eccles-Wigfield utility/attainment value axes.
// One axis is surfaced per brief (round-robin rotation via Domain.LastValueAxis).
type ValueFraming struct {
	Axis             string `json:"axis"` // financial | employment | intellectual | innovation
	Statement        string `json:"statement,omitempty"`
	ConceptRelevance string `json:"concept_relevance,omitempty"`
}

// Canonical value axes (and their rotation order).
var ValueAxes = []string{"financial", "employment", "intellectual", "innovation"}

// DomainValueFramings is the JSON shape persisted in domains.value_framings_json.
// Each axis maps to a short (1-2 sentence) authored statement.
type DomainValueFramings struct {
	Financial    string `json:"financial,omitempty"`
	Employment   string `json:"employment,omitempty"`
	Intellectual string `json:"intellectual,omitempty"`
	Innovation   string `json:"innovation,omitempty"`
}

// StatementFor returns the authored statement for a given axis, or "" if none.
func (v *DomainValueFramings) StatementFor(axis string) string {
	switch axis {
	case "financial":
		return v.Financial
	case "employment":
		return v.Employment
	case "intellectual":
		return v.Intellectual
	case "innovation":
		return v.Innovation
	}
	return ""
}

// ConceptDelta captures the change in mastery on a concept over a window.
type ConceptDelta struct {
	Concept    string  `json:"concept"`
	MasteryNow float64 `json:"mastery_now"`
	MasteryWas float64 `json:"mastery_was"`
	Delta      float64 `json:"delta"`
}

// ProgressDelta shows forward motion on the current concept (used for milestone / narrative).
type ProgressDelta struct {
	Concept    string  `json:"concept"`
	MasteryNow float64 `json:"mastery_now"`
	MasteryWas float64 `json:"mastery_was"`
	Threshold  float64 `json:"threshold,omitempty"` // the milestone just crossed, if any
}

// FailureMeta summarises the most recent failure on a concept (for growth_mindset reframing).
type FailureMeta struct {
	Concept           string `json:"concept"`
	HintsRequested    int    `json:"hints_requested"`
	ErrorType         string `json:"error_type,omitempty"`
	MisconceptionType string `json:"misconception_type,omitempty"`
	HoursAgo          int    `json:"hours_ago"`
}

// AffectMeta surfaces the dominant negative signal from the most recent record_affect
// call (for affect_reframe).
type AffectMeta struct {
	SessionID string `json:"session_id"`
	Dimension string `json:"dimension"` // satisfaction | difficulty | energy
	Value     int    `json:"value"`
	HoursAgo  int    `json:"hours_ago"`
}

// ProgressNarrative is attached to get_learner_context responses. Claude uses it
// to open the session with a short (1-2 sentence) trajectory story.
type ProgressNarrative struct {
	MasteryTrajectory  []ConceptDelta `json:"mastery_trajectory"`
	SessionStreak      int            `json:"session_streak"`
	AutonomyTrend      string         `json:"autonomy_trend"` // rising | stable | declining
	MilestonesThisWeek []string       `json:"milestones_this_week"`
	DormancyImminent   bool           `json:"dormancy_imminent"` // true if last_session > 24h ago
	Instruction        string         `json:"instruction"`
}

// ImplementationIntention is a Gollwitzer-style if-then commitment captured at
// session close ("when X happens, I will Y").
type ImplementationIntention struct {
	ID           int64      `json:"id"`
	LearnerID    string     `json:"learner_id"`
	DomainID     string     `json:"domain_id"`
	Trigger      string     `json:"trigger"`
	Action       string     `json:"action"`
	Honored      *bool      `json:"honored,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
}

// RecapBrief is returned by record_session_close — signals for Claude to compose
// the closing message.
type RecapBrief struct {
	ConceptsPracticed             []string `json:"concepts_practiced"`
	Wins                          []string `json:"wins"`
	InterestingStruggles          []string `json:"interesting_struggles"`
	NextScheduledReview           string   `json:"next_scheduled_review,omitempty"`
	PromptForImplementationIntent bool     `json:"prompt_for_implementation_intent"`
	Instruction                   string   `json:"instruction"`
}

// Webhook queue kinds.
const (
	WebhookKindDailyMotivation = "daily_motivation"
	WebhookKindDailyRecap      = "daily_recap"
	WebhookKindReactivation    = "reactivation"
	WebhookKindReminder        = "reminder"
	// WebhookKindMirror carries a metacognitive mirror message authored by
	// engine.DetectMirrorPattern. Persisted into webhook_message_queue so
	// the scheduler can push it proactively (see #59 / engine.EnqueueMirrorWebhook).
	WebhookKindMirror = "mirror_message"
)

// Webhook queue statuses.
const (
	WebhookStatusPending = "pending"
	WebhookStatusSent    = "sent"
	WebhookStatusExpired = "expired"
	WebhookStatusFailed  = "failed"
)

// WebhookQueueItem represents a scheduled, LLM-authored webhook nudge.
type WebhookQueueItem struct {
	ID           int64      `json:"id"`
	LearnerID    string     `json:"learner_id"`
	Kind         string     `json:"kind"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Content      string     `json:"content"`
	Priority     int        `json:"priority"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
}
