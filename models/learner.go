// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

type Learner struct {
	ID           string
	Email        string
	PasswordHash string
	Objective    string
	WebhookURL   string
	ProfileJSON  string
	CreatedAt    time.Time
	LastActive   time.Time
}

type ConceptState struct {
	ID            int64
	LearnerID     string
	DomainID      string
	Concept       string
	Stability     float64
	Difficulty    float64
	ElapsedDays   int
	ScheduledDays int
	Reps          int
	Lapses        int
	CardState     string
	LastReview    *time.Time
	NextReview    *time.Time
	PMastery      float64
	PLearn        float64
	PForget       float64
	PSlip         float64
	PGuess        float64
	Theta         float64
	UpdatedAt     time.Time
}

func NewConceptState(learnerID, concept string) *ConceptState {
	return NewConceptStateInDomain(learnerID, "", concept)
}

// NewConceptStateInDomain creates a cognitive state whose identity is scoped
// to one learning domain. Concept labels are intentionally only unique inside
// a domain; using DomainID here prevents common labels such as "functions" or
// "probability" from sharing BKT/FSRS/IRT state across unrelated subjects.
func NewConceptStateInDomain(learnerID, domainID, concept string) *ConceptState {
	return &ConceptState{
		LearnerID:  learnerID,
		DomainID:   domainID,
		Concept:    concept,
		Stability:  1.0,
		Difficulty: 0.3,
		CardState:  "new",
		PMastery:   0.1,
		PLearn:     0.15,
		PForget:    0.05,
		PSlip:      0.1,
		PGuess:     0.2,
		Theta:      0.0,
	}
}

type Interaction struct {
	ID                  int64
	LearnerID           string
	SessionID           string // empty only for interactions created before durable sessions
	AssessmentAttemptID string // empty only for legacy/unverified observations
	Concept             string
	ActivityType        string
	Success             bool
	ResponseTime        int
	Confidence          float64
	ErrorType           string
	Notes               string
	HintsRequested      int
	SelfInitiated       bool
	CalibrationID       string
	IsProactiveReview   bool
	MisconceptionType   string
	MisconceptionDetail string
	DomainID            string // optional, blank for pre-issue-#24 rows
	// BKTSlip / BKTGuess capture the slip/guess parameters the project's
	// non-canonical error-type-aware heuristic
	// (algorithms.BKTUpdateHeuristicSlipByErrorType) actually fed into the
	// BKT update for this observation, so the run can be replayed
	// deterministically. Pointers so a NULL on pre-issue-#51 rows stays
	// distinguishable from a legitimate zero. See issue #51 / #8.
	BKTSlip         *float64
	BKTGuess        *float64
	RubricJSON      string
	RubricScoreJSON string
	CreatedAt       time.Time
}

type RefreshToken struct {
	Token     string
	LearnerID string
	ClientID  string // optional, blank for pre-issue-#30 tokens
	FamilyID  string
	ExpiresAt time.Time
	CreatedAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}
