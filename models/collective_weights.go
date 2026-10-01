// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package models

import "time"

const (
	CollectiveWeightsPolicyVersion = "institution-collective-weights-v1"
	// A concept is weighted only from this many distinct learners...
	CollectiveWeightMinContributors = 30
	// ...each with at least this many recorded reviews of it.
	CollectiveWeightMinReps = 5
	// A new version is published only if a parameter moves at least this much,
	// and never by more than the cap, so the collective stays stable.
	CollectiveWeightMinChange = 0.01
	CollectiveWeightMaxStep   = 0.02
	// Applying a weight to a learner who already has reviews.
	CollectiveBlendFactor  = 0.25
	CollectiveBlendMaxStep = 0.05
	CollectiveParamMin     = 0.01
	CollectiveParamMax     = 0.5
)

const (
	CollectiveApplyPrior = "prior"
	CollectiveApplyBlend = "blend"
)

// BKTParams are the four BKT probabilities a collective weight can carry.
type BKTParams struct {
	PLearn  float64 `json:"p_learn"`
	PForget float64 `json:"p_forget"`
	PSlip   float64 `json:"p_slip"`
	PGuess  float64 `json:"p_guess"`
}

// CollectiveWeight is one immutable version of a concept's collective
// parameters. It holds no learner identifier.
type CollectiveWeight struct {
	Version       int64     `json:"version"`
	Contributors  int       `json:"contributors"`
	PolicyVersion string    `json:"policy_version"`
	ComputedAt    time.Time `json:"computed_at"`
	BKTParams
}

// CollectiveWeightApplication traces one change to one learner's parameters.
type CollectiveWeightApplication struct {
	Version int64     `json:"version"`
	Mode    string    `json:"mode"`
	Before  BKTParams `json:"before"`
	After   BKTParams `json:"after"`
}
