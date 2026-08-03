// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

import (
	"encoding/json"
	"log/slog"
	"time"
)

type AlertType string

const (
	AlertForgetting           AlertType = "FORGETTING"
	AlertPlateau              AlertType = "PLATEAU"
	AlertZPDDrift             AlertType = "ZPD_DRIFT"
	AlertOverload             AlertType = "OVERLOAD"
	AlertMasteryReady         AlertType = "MASTERY_READY"
	AlertDependencyIncreasing AlertType = "DEPENDENCY_INCREASING"
	AlertCalibrationDiverging AlertType = "CALIBRATION_DIVERGING"
	AlertAffectNegative       AlertType = "AFFECT_NEGATIVE"
	AlertTransferBlocked      AlertType = "TRANSFER_BLOCKED"
)

type AlertUrgency string

const (
	UrgencyCritical AlertUrgency = "critical"
	UrgencyWarning  AlertUrgency = "warning"
	UrgencyInfo     AlertUrgency = "info"
)

type Alert struct {
	DomainID           string       `json:"domain_id,omitempty"`
	Type               AlertType    `json:"type"`
	Concept            string       `json:"concept"`
	Urgency            AlertUrgency `json:"urgency"`
	Retention          float64      `json:"retention,omitempty"`
	HoursUntilCritical float64      `json:"hours_until_critical,omitempty"`
	SessionsStalled    int          `json:"sessions_stalled,omitempty"`
	ErrorRate          float64      `json:"error_rate,omitempty"`
	RecommendedAction  string       `json:"recommended_action"`
}

type ActivityType string

const (
	ActivityRecall           ActivityType = "RECALL_EXERCISE"
	ActivityNewConcept       ActivityType = "NEW_CONCEPT"
	ActivityMasteryChallenge ActivityType = "MASTERY_CHALLENGE"
	ActivityDebuggingCase    ActivityType = "DEBUGGING_CASE"
	ActivityRest             ActivityType = "REST"
	ActivitySetupDomain      ActivityType = "SETUP_DOMAIN"

	// Activity types emitted by [5] ActionSelector (engine/action_selector.go).
	//
	// ActivityDebugMisconception is intentionally distinct from
	// ActivityDebuggingCase: the latter is a *plateau-breaking* rotation
	// of varied formats ("debugging" / "real_world_case" /
	// "teaching_exercise" / "creative_application"), while
	// DebugMisconception is a *targeted* confrontation of one specific
	// active misconception detected on the concept. Different pedagogical
	// intent, different downstream LLM handling.
	ActivityPractice           ActivityType = "PRACTICE"
	ActivityDebugMisconception ActivityType = "DEBUG_MISCONCEPTION"
	ActivityFeynmanPrompt      ActivityType = "FEYNMAN_PROMPT"
	ActivityTransferProbe      ActivityType = "TRANSFER_PROBE"
	// ActivityDiagnosticAssessment is a cold, evidence-only probe used while
	// the domain FSM is in DIAGNOSTIC. It must not introduce, explain or scaffold
	// the target concept before the learner commits an answer.
	ActivityDiagnosticAssessment ActivityType = "DIAGNOSTIC_ASSESSMENT"

	// ActivityCloseSession is emitted by [3] Gate Controller as an
	// *escape action* when the OVERLOAD alert fires (session has run
	// past its hygiene budget, ~45 min). Distinct from ActivityRest:
	//
	//   - ActivityRest        = pause INTRA-session. The learner will
	//                            continue the same session afterwards.
	//   - ActivityCloseSession = forced END of session. The pipeline
	//                            short-circuits — no further activity
	//                            is selected. The LLM is expected to
	//                            emit recap_brief and call
	//                            record_session_close.
	ActivityCloseSession ActivityType = "CLOSE_SESSION"
)

type Activity struct {
	Type             ActivityType `json:"type"`
	Concept          string       `json:"concept"`
	DifficultyTarget float64      `json:"difficulty_target"`
	Format           string       `json:"format"`
	EstimatedMinutes int          `json:"estimated_minutes"`
	Rationale        string       `json:"rationale"`
	PromptForLLM     string       `json:"prompt_for_llm"`
}

type PedagogicalContract struct {
	Intent                  string                   `json:"intent"`
	TargetConcept           string                   `json:"target_concept,omitempty"`
	RecommendedActivityType ActivityType             `json:"recommended_activity_type"`
	Constraints             PedagogicalConstraints   `json:"constraints"`
	AllowedVariants         []string                 `json:"allowed_variants"`
	LLMDiscretion           PedagogicalLLMDiscretion `json:"llm_discretion"`
	FadeGuidance            *PedagogicalFadeGuidance `json:"fade_guidance,omitempty"`
	EpisodicContext         any                      `json:"episodic_context,omitempty"`
	ReasoningRequest        *ReasoningRequest        `json:"reasoning_request,omitempty"`
	LearnerExplanation      string                   `json:"learner_explanation"`
	AuditRationale          string                   `json:"audit_rationale"`
	LLMInstruction          string                   `json:"llm_instruction"`
}

type ReasoningRequest struct {
	Task           string   `json:"task"`
	Constraints    []string `json:"constraints"`
	OutputRequired string   `json:"output_required"`
	EnabledBecause []string `json:"enabled_because,omitempty"`
}

type PedagogicalConstraints struct {
	MustCollect []string `json:"must_collect,omitempty"`
	Avoid       []string `json:"avoid,omitempty"`
}

type PedagogicalLLMDiscretion struct {
	CanChangeFormat                  bool `json:"can_change_format"`
	CanRequestClarification          bool `json:"can_request_clarification"`
	CanProposeNegotiation            bool `json:"can_propose_negotiation"`
	CannotMarkMasteryWithoutEvidence bool `json:"cannot_mark_mastery_without_evidence"`
}

type PedagogicalFadeGuidance struct {
	HintLevel              string `json:"hint_level"`
	WebhookFrequency       string `json:"webhook_frequency"`
	ZPDAggressiveness      string `json:"zpd_aggressiveness"`
	ProactiveReviewEnabled bool   `json:"proactive_review_enabled"`
	Instruction            string `json:"instruction"`
}

type GoalRelevanceStatus struct {
	Status          string   `json:"status"`
	Message         string   `json:"message"`
	RecommendedTool string   `json:"recommended_tool,omitempty"`
	MissingConcepts []string `json:"missing_concepts,omitempty"`
	Stale           bool     `json:"stale,omitempty"`
}

type KnowledgeSpace struct {
	Concepts      []string            `json:"concepts"`
	Prerequisites map[string][]string `json:"prerequisites"`
}

type Domain struct {
	ID                string
	LearnerID         string
	Name              string
	PersonalGoal      string
	Graph             KnowledgeSpace
	ValueFramingsJSON string
	LastValueAxis     string
	Archived          bool
	// HighStakes activates the conservative evidence and notification policy:
	// demonstrated claims and intrusive pushes require a trusted human-reviewed
	// assessment. The public MCP surface can mark this flag but cannot unset it.
	HighStakes           bool
	PriorityRank         *int
	GraphVersion         int
	GoalRelevanceJSON    string
	GoalRelevanceVersion int
	// Phase is the FSM state set by [2] PhaseController. Empty string
	// means "not yet initialised" — read as PhaseInstruction by the
	// orchestrator (legacy backward-compat fallback per OQ-2.1).
	Phase Phase
	// PhaseChangedAt is the UTC timestamp of the last phase transition. It
	// bounds the query for qualified, distinct diagnostic concept coverage.
	PhaseChangedAt time.Time
	// PhaseEntryEntropy is the mean binary entropy of P(L) over the
	// domain's concepts at the moment the phase was last set to
	// DIAGNOSTIC. It enriches the transition audit rationale; qualified
	// concept coverage remains mandatory even when entropy falls.
	PhaseEntryEntropy float64
	CreatedAt         time.Time
	// DeletedAt is set for a logically deleted domain. Runtime readers filter
	// tombstones, while assessment and curriculum audit rows keep their stable
	// domain reference.
	DeletedAt *time.Time
}

// GoalRelevance is the parsed payload of Domain.GoalRelevanceJSON. It maps
// concept ids to a relevance score in [0,1] (1 = goal-critical, 0 =
// orthogonal). Only concepts that appear in Domain.Graph.Concepts are
// stored — unknown concepts are rejected at write time (cf. tools/goal_relevance.go).
type GoalRelevance struct {
	ForGraphVersion int                `json:"for_graph_version"`
	Relevance       map[string]float64 `json:"relevance"`
	SetAt           time.Time          `json:"set_at"`
}

// ParseGoalRelevance returns the structured GoalRelevance parsed from
// d.GoalRelevanceJSON. Returns nil (no error) when the JSON is empty or
// unparseable — callers treat nil as "no relevance set, use uniform
// fallback". Silent fallback is intentional from the *session's* point
// of view: a corrupt vector must never block an exercise. A WARN is
// emitted via slog.Default() so a systematic corruption surfaces in
// logs without disrupting the running session.
func (d *Domain) ParseGoalRelevance() *GoalRelevance {
	if d.GoalRelevanceJSON == "" {
		return nil
	}
	var gr GoalRelevance
	if err := json.Unmarshal([]byte(d.GoalRelevanceJSON), &gr); err != nil {
		slog.Warn("goal_relevance JSON corrupt, falling back to nil",
			"domain_id", d.ID, "err", err)
		return nil
	}
	return &gr
}

// IsGoalRelevanceStale reports whether the stored vector was last set
// against an older graph version than the current one. Per OQ-1.1, this
// does NOT block any operation — it is observable via get_goal_relevance
// so the LLM can choose to re-decompose for the new concepts.
func (d *Domain) IsGoalRelevanceStale() bool {
	gr := d.ParseGoalRelevance()
	if gr == nil {
		return d.GraphVersion > 0
	}
	return gr.ForGraphVersion < d.GraphVersion
}

// UncoveredConcepts returns the concepts present in d.Graph.Concepts that
// have no entry in the stored relevance vector. Per OQ-1.2 these are the
// "per-concept stale" concepts: visible to the LLM via get_goal_relevance,
// not auto-flagged on the rest of the vector.
func (d *Domain) UncoveredConcepts() []string {
	gr := d.ParseGoalRelevance()
	covered := map[string]bool{}
	if gr != nil {
		for c := range gr.Relevance {
			covered[c] = true
		}
	}
	var uncovered []string
	for _, c := range d.Graph.Concepts {
		if !covered[c] {
			uncovered = append(uncovered, c)
		}
	}
	return uncovered
}

type TimeWindow struct {
	Day   string `json:"day"`
	Start string `json:"start"`
	End   string `json:"end"`
}
